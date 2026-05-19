// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package grok implements a client for the xAI Grok Realtime voice API.
//
// Connect to a session with [Connect], then stream PCM audio with
// [Client.SendAudio] or inject text with [Client.SendText]. Callbacks in
// [Config] receive audio deltas, transcripts, and function-call requests.
// Close the session with [Client.Close].
//
// Audio format: 24 kHz raw PCM (16-bit), matching the default for the
// xAI Realtime API. By default the server performs voice-activity
// detection (server_vad). Set [Config.ManualCommit] for push-to-talk
// — the caller then signals end-of-utterance via
// [Client.CommitAndRespond].
package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Tool defines a function tool available to the Grok model. Set Type to
// "function", Name to the callable identifier, Description to guide the
// model, and Parameters to a JSON Schema object describing the arguments.
type Tool struct {
	// Type should be "function".
	Type string `json:"type"`

	// Name is the function identifier the model uses in a function_call event.
	Name string `json:"name,omitempty"`

	// Description is shown to the model to describe what the function does.
	Description string `json:"description,omitempty"`

	// Parameters is a JSON Schema object describing the function's arguments.
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// Config holds configuration for a Grok Realtime session. All callback
// fields are optional — unset callbacks are silently ignored. The only
// required field is APIKey.
type Config struct {
	// APIKey is the xAI API key for authentication (required).
	APIKey string

	// OnAudio is called with base64-decoded PCM audio from Grok.
	OnAudio func(pcm []byte)

	// OnTranscript is called with text deltas the model speaks.
	OnTranscript func(text string)

	// OnTranscriptDone is called when the model finishes speaking.
	OnTranscriptDone func()

	// OnUserTranscript is called with transcribed user speech.
	OnUserTranscript func(text string)

	// OnFunctionCall is called when Grok invokes a tool.
	// The handler must return the result string.
	OnFunctionCall func(name string, args json.RawMessage) (string, error)

	// OnSessionReady is called when the session is configured.
	OnSessionReady func()

	// OnResponseDone fires when Grok has emitted response.done — i.e.
	// the full response for the current turn (audio, transcript, any
	// tool calls) is complete. Use this to know when it is safe to
	// commit the next utterance or close the session.
	OnResponseDone func()

	// OnError is called on protocol errors.
	OnError func(err error)

	// Voice for TTS output. Default: "Eve".
	Voice string

	// Tools available to the model.
	Tools []Tool

	// SystemPrompt for the session.
	SystemPrompt string

	// ManualCommit disables Grok's server-side VAD. The caller must
	// explicitly call [Client.CommitAndRespond] at end-of-utterance.
	// Use this for push-to-talk: with server VAD enabled, mid-phrase
	// pauses trigger spurious commits; with manual commit, the human
	// is the VAD.
	ManualCommit bool
}

// Client manages a Grok Realtime WebSocket session. Obtain one via [Connect].
// Call [Client.Close] when the session is no longer needed.
type Client struct {
	cfg  Config
	conn *websocket.Conn

	mu     sync.Mutex
	closed bool

	pendingCalls map[string]bool
}

// Connect dials wss://api.x.ai/v1/realtime, configures the session from cfg,
// and starts the event loop. It blocks until the first server acknowledgement
// is received, so the caller can detect auth failures before streaming audio.
// The event loop runs until [Client.Close] is called or ctx is cancelled.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("grok: API key required")
	}
	if cfg.Voice == "" {
		cfg.Voice = "Eve"
	}

	conn, _, err := websocket.Dial(ctx, "wss://api.x.ai/v1/realtime", &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer " + cfg.APIKey},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("grok: dial failed: %w", err)
	}

	conn.SetReadLimit(4 << 20) // 4 MB

	c := &Client{
		cfg:          cfg,
		conn:         conn,
		pendingCalls: make(map[string]bool),
	}

	slog.Info("grok: WebSocket connected, sending session config")

	if err := c.configureSession(ctx); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("grok: session config failed: %w", err)
	}

	slog.Info("grok: session config sent, waiting for response")

	_, firstMsg, err := conn.Read(ctx)
	if err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("grok: first read failed: %w", err)
	}
	slog.Info("grok: first message", "data", string(firstMsg))

	var firstEvent map[string]any
	if err := json.Unmarshal(firstMsg, &firstEvent); err == nil {
		if firstEvent["type"] == "error" {
			conn.CloseNow()
			if errObj, ok := firstEvent["error"].(map[string]any); ok {
				return nil, fmt.Errorf("grok: server rejected session: %v", errObj["message"])
			}
			return nil, fmt.Errorf("grok: server rejected session: %s", string(firstMsg))
		}
		c.handleEvent(ctx, firstEvent)
	}

	go c.readLoop(ctx)

	return c, nil
}

// SendAudio sends PCM audio data to Grok. The data should be raw PCM
// bytes (not base64 encoded) — this method handles the encoding.
func (c *Client) SendAudio(ctx context.Context, pcm []byte) error {
	encoded := base64.StdEncoding.EncodeToString(pcm)
	return c.send(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": encoded,
	})
}

// ClearBuffer discards any pending audio in the input buffer without
// committing. Use to abort an utterance the user changed their mind
// about — sending Commit on an empty/noisy buffer makes Grok transcribe
// noise and respond to it.
func (c *Client) ClearBuffer(ctx context.Context) error {
	return c.send(ctx, map[string]any{
		"type": "input_audio_buffer.clear",
	})
}

// Commit signals end-of-utterance to Grok and asks it to transcribe
// the buffered audio. Useful for push-to-talk: when the user releases
// the talk key, the trailing silence may be too short for server VAD
// to fire — call Commit to force a transcription before tearing down.
//
// Commit only flushes the audio buffer. In ManualCommit mode the server
// will not auto-generate a response; use [Client.CommitAndRespond] to
// commit and request a response in one call.
func (c *Client) Commit(ctx context.Context) error {
	return c.send(ctx, map[string]any{
		"type": "input_audio_buffer.commit",
	})
}

// CommitAndRespond commits the buffered audio and explicitly asks Grok
// to generate a response. Use this with [Config.ManualCommit] mode,
// where Grok does not auto-respond after a commit.
//
// WARNING: in ManualCommit mode the input-audio transcription happens
// asynchronously AFTER input_audio_buffer.commit is ACK'd. Sending
// response.create immediately (as this method does) races the
// transcription — the response is generated against the conversation
// state at request time, which usually does not yet include the new
// user item. Prefer [Client.RequestResponse] called from the
// [Config.OnUserTranscript] callback for correct ordering.
func (c *Client) CommitAndRespond(ctx context.Context) error {
	if err := c.Commit(ctx); err != nil {
		return err
	}
	return c.RequestResponse(ctx, nil)
}

// RequestResponse explicitly asks Grok to generate a response based on
// the current conversation state. The caller is responsible for
// ordering: in ManualCommit mode this should be called only after the
// new user item has actually been added to the conversation (i.e.
// after OnUserTranscript fires), otherwise the response will be
// generated without the user's just-committed audio in scope.
func (c *Client) RequestResponse(ctx context.Context, modalities ResponseModalities) error {
	if len(modalities) == 0 {
		modalities = ModalitiesTextAudio
	}
	return c.send(ctx, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string(modalities),
		},
	})
}

// ResponseModalities selects which output modalities Grok generates
// for a response. Pass to [Client.SendText] or [Client.SendSystemNote].
// "text" emits only the transcript (no audio billed); the
// "text,audio" combination produces spoken output too.
type ResponseModalities []string

var (
	ModalitiesTextAudio = ResponseModalities{"text", "audio"}
	ModalitiesText      = ResponseModalities{"text"}
)

// SendText sends a text user message into the conversation and asks
// Grok to respond in the given modalities. Pass [ModalitiesText] for
// typed input that should produce a text-only reply (no synthesised
// audio); [ModalitiesTextAudio] for voice-style replies.
func (c *Client) SendText(ctx context.Context, text string, modalities ResponseModalities) error {
	if err := c.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": text},
			},
		},
	}); err != nil {
		return err
	}
	if len(modalities) == 0 {
		modalities = ModalitiesTextAudio
	}
	return c.send(ctx, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string(modalities),
		},
	})
}

// InjectConversationItem inserts a single conversation item (user,
// assistant, or system role) WITHOUT triggering a response. Use this
// to replay prior conversation history into a fresh session so the
// model has context, without the model immediately reacting to each
// replayed turn. The content type is "input_text" for user/system and
// "text" for assistant, matching Realtime's per-role schema.
func (c *Client) InjectConversationItem(ctx context.Context, role, text string) error {
	if text == "" {
		return nil
	}
	contentType := "input_text"
	if role == "assistant" {
		contentType = "text"
	}
	return c.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": role,
			"content": []map[string]any{
				{"type": contentType, "text": text},
			},
		},
	})
}

// SendSystemNote inserts a system-role message into the conversation
// and triggers a response. Use this to notify the model of an async
// event the conversation should react to (e.g. a worker task
// completed, an external state change). The model treats system
// messages as out-of-band context, distinct from user input.
//
// Modalities controls how Grok responds; pass ModalitiesText for a
// text-only reaction (no synthesised audio) or ModalitiesTextAudio
// for a spoken response. Callers should normally pass the modality
// matching the original user input that initiated the work — text
// for typed prompts, text+audio for voice prompts.
func (c *Client) SendSystemNote(ctx context.Context, text string, modalities ResponseModalities) error {
	if err := c.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "system",
			"content": []map[string]any{
				{"type": "input_text", "text": text},
			},
		},
	}); err != nil {
		return err
	}
	if len(modalities) == 0 {
		modalities = ModalitiesTextAudio
	}
	return c.send(ctx, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string(modalities),
		},
	})
}

// InjectAssistantText inserts text into the conversation as an assistant
// turn, then asks Grok to speak it aloud verbatim. Use this to relay a
// Claude Code response through the Grok voice channel without re-phrasing.
// Config.OnTranscript will fire with the spoken text.
//
// Deprecated: prefer SendSystemNote so the model reacts conversationally
// to the new context rather than parroting it verbatim. Retained for
// backwards compatibility with the legacy voice bridge.
func (c *Client) InjectAssistantText(ctx context.Context, text string) error {
	if err := c.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	}); err != nil {
		return err
	}

	return c.send(ctx, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string{"audio"},
			"instructions": "Read the last assistant message aloud naturally. " +
				"Do not add commentary or change the content — just speak it.",
		},
	})
}

// Close terminates the session.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close(websocket.StatusNormalClosure, "bye")
}

func (c *Client) configureSession(ctx context.Context) error {
	session := map[string]any{
		"voice": c.cfg.Voice,
		"audio": map[string]any{
			"input": map[string]any{
				"format": map[string]any{
					"type": "audio/pcm",
					"rate": 24000,
				},
			},
			"output": map[string]any{
				"format": map[string]any{
					"type": "audio/pcm",
					"rate": 24000,
				},
			},
		},
	}

	if c.cfg.SystemPrompt != "" {
		session["instructions"] = c.cfg.SystemPrompt
	}

	if len(c.cfg.Tools) > 0 {
		session["tools"] = c.cfg.Tools
	}

	if c.cfg.ManualCommit {
		// Explicit null disables server-side VAD entirely. Omitting the
		// field leaves the API on its default, which is server_vad — so
		// we must send the explicit null to actually suppress it. The
		// caller drives commits via [Client.CommitAndRespond].
		session["turn_detection"] = nil
	} else {
		// Default: server-side VAD auto-commits on silence and triggers
		// a response.
		session["turn_detection"] = map[string]any{
			"type":                "server_vad",
			"threshold":           0.7,
			"silence_duration_ms": 800,
			"prefix_padding_ms":   300,
		}
	}

	return c.send(ctx, map[string]any{
		"type":    "session.update",
		"session": session,
	})
}

func (c *Client) readLoop(ctx context.Context) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()
			if !closed {
				status := websocket.CloseStatus(err)
				slog.Error("grok: read error", "err", err, "close_status", status)
				if c.cfg.OnError != nil {
					c.cfg.OnError(err)
				}
			}
			return
		}

		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("grok: invalid JSON", "err", err)
			continue
		}

		c.handleEvent(ctx, msg)
	}
}

func (c *Client) handleEvent(ctx context.Context, msg map[string]any) {
	eventType, _ := msg["type"].(string)

	switch eventType {
	case "session.updated":
		slog.Info("grok: session configured")
		if c.cfg.OnSessionReady != nil {
			c.cfg.OnSessionReady()
		}

	case "response.output_audio.delta":
		if delta, ok := msg["delta"].(string); ok && c.cfg.OnAudio != nil {
			pcm, err := base64.StdEncoding.DecodeString(delta)
			if err != nil {
				slog.Warn("grok: audio decode failed", "err", err)
				return
			}
			c.cfg.OnAudio(pcm)
		}

	case "response.output_audio_transcript.delta":
		if delta, ok := msg["delta"].(string); ok && c.cfg.OnTranscript != nil {
			c.cfg.OnTranscript(delta)
		}

	case "response.output_audio_transcript.done":
		if c.cfg.OnTranscriptDone != nil {
			c.cfg.OnTranscriptDone()
		}

	case "conversation.item.input_audio_transcription.completed":
		if transcript, ok := msg["transcript"].(string); ok && c.cfg.OnUserTranscript != nil {
			c.cfg.OnUserTranscript(transcript)
		}

	case "response.function_call_arguments.done":
		go c.handleFunctionCall(ctx, msg)

	case "input_audio_buffer.speech_started":
		slog.Debug("grok: speech started")

	case "input_audio_buffer.speech_stopped":
		slog.Debug("grok: speech stopped")

	case "error":
		errMsg, _ := msg["error"].(map[string]any)
		errText, _ := errMsg["message"].(string)
		slog.Error("grok: server error", "error", errText)
		if c.cfg.OnError != nil {
			c.cfg.OnError(fmt.Errorf("grok server: %s", errText))
		}

	case "response.done":
		slog.Debug("grok: response complete")
		if c.cfg.OnResponseDone != nil {
			c.cfg.OnResponseDone()
		}

	default:
		slog.Info("grok: unhandled event", "type", eventType)
	}
}

func (c *Client) handleFunctionCall(ctx context.Context, msg map[string]any) {
	callID, _ := msg["call_id"].(string)
	name, _ := msg["name"].(string)
	argsStr, _ := msg["arguments"].(string)

	slog.Info("grok: function call", "name", name, "call_id", callID)

	if c.cfg.OnFunctionCall == nil {
		c.sendFunctionResult(ctx, callID, `{"error":"no handler"}`)
		return
	}

	result, err := c.cfg.OnFunctionCall(name, json.RawMessage(argsStr))
	if err != nil {
		slog.Error("grok: function call failed", "name", name, "err", err)
		errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
		c.sendFunctionResult(ctx, callID, string(errJSON))
		return
	}

	c.sendFunctionResult(ctx, callID, result)
}

func (c *Client) sendFunctionResult(ctx context.Context, callID, output string) {
	if err := c.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  output,
		},
	}); err != nil {
		slog.Error("grok: failed to send function result", "err", err)
		return
	}

	if err := c.send(ctx, map[string]any{
		"type": "response.create",
	}); err != nil {
		slog.Error("grok: failed to request continuation", "err", err)
	}
}

func (c *Client) send(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("grok: marshal failed: %w", err)
	}

	if m, ok := v.(map[string]any); ok {
		if t, _ := m["type"].(string); t != "input_audio_buffer.append" {
			slog.Debug("grok: sending", "json", string(data))
		}
	}

	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return c.conn.Write(writeCtx, websocket.MessageText, data)
}
