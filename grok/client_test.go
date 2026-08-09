// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// itemContent digs out the single content entry of a
// conversation.item.create message, failing the test on any mismatch.
func itemContent(t *testing.T, msg map[string]any) (role string, content map[string]any) {
	t.Helper()
	item, ok := msg["item"].(map[string]any)
	if !ok {
		t.Fatalf("message has no item object: %#v", msg)
	}
	role, _ = item["role"].(string)
	entries, ok := item["content"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("item content = %#v, want exactly one entry", item["content"])
	}
	content, ok = entries[0].(map[string]any)
	if !ok {
		t.Fatalf("content entry = %#v, want an object", entries[0])
	}
	return role, content
}

// modalities digs the requested modalities out of a response.create.
func modalities(t *testing.T, msg map[string]any) []any {
	t.Helper()
	response, ok := msg["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.create has no response object: %#v", msg)
	}
	got, ok := response["modalities"].([]any)
	if !ok {
		t.Fatalf("modalities = %#v, want a list", response["modalities"])
	}
	return got
}

func TestSendText(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{})

	if err := c.SendText(context.Background(), "hello there", ModalitiesText); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	item := s.nextOfType(t, "conversation.item.create")
	role, content := itemContent(t, item)
	if role != "user" {
		t.Errorf("role = %q, want user", role)
	}
	if content["type"] != "input_text" || content["text"] != "hello there" {
		t.Errorf("content = %#v, want the input_text message", content)
	}

	create := s.nextOfType(t, "response.create")
	if got := modalities(t, create); !reflect.DeepEqual(got, []any{"text"}) {
		t.Errorf("modalities = %#v, want text only", got)
	}
}

// TestSendTextDefaultModalities pins the documented default: an unset
// modality list means text+audio, not an empty request.
func TestSendTextDefaultModalities(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{})

	if err := c.SendText(context.Background(), "hello", nil); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	s.nextOfType(t, "conversation.item.create")

	create := s.nextOfType(t, "response.create")
	if got := modalities(t, create); !reflect.DeepEqual(got, []any{"text", "audio"}) {
		t.Errorf("modalities = %#v, want text and audio", got)
	}
}

func TestSendAudioEncodesPCM(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{})

	pcm := []byte{0x00, 0x01, 0xfe, 0xff, 0x7f}
	if err := c.SendAudio(context.Background(), pcm); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}

	msg := s.nextOfType(t, "input_audio_buffer.append")
	encoded, ok := msg["audio"].(string)
	if !ok {
		t.Fatalf("append has no audio string: %#v", msg)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("audio is not base64: %v", err)
	}
	if !reflect.DeepEqual(decoded, pcm) {
		t.Errorf("decoded audio = %v, want %v", decoded, pcm)
	}
}

// TestBufferControlMessages covers the one-line buffer verbs together —
// each is a single wire message with no payload.
func TestBufferControlMessages(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) error
		want string
	}{
		{"ClearBuffer", func(c *Client) error { return c.ClearBuffer(context.Background()) }, "input_audio_buffer.clear"},
		{"Commit", func(c *Client) error { return c.Commit(context.Background()) }, "input_audio_buffer.commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := (&fakeGrok{}).start(t)
			c, s := connectFake(t, f, Config{})

			if err := tt.call(c); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if got := s.next(t)["type"]; got != tt.want {
				t.Errorf("sent %v, want %s", got, tt.want)
			}
		})
	}
}

func TestCommitAndRespond(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{ManualCommit: true})

	if err := c.CommitAndRespond(context.Background()); err != nil {
		t.Fatalf("CommitAndRespond: %v", err)
	}

	if got := s.next(t)["type"]; got != "input_audio_buffer.commit" {
		t.Fatalf("first message = %v, want the commit", got)
	}
	create := s.next(t)
	if got := create["type"]; got != "response.create" {
		t.Fatalf("second message = %v, want response.create", got)
	}
	if got := modalities(t, create); !reflect.DeepEqual(got, []any{"text", "audio"}) {
		t.Errorf("modalities = %#v, want text and audio", got)
	}
}

func TestRequestResponse(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{})

	if err := c.RequestResponse(context.Background(), ModalitiesText); err != nil {
		t.Fatalf("RequestResponse: %v", err)
	}
	create := s.nextOfType(t, "response.create")
	if got := modalities(t, create); !reflect.DeepEqual(got, []any{"text"}) {
		t.Errorf("modalities = %#v, want text only", got)
	}
}

// TestInjectConversationItem covers history replay: per-role content
// types, and no response.create — the model must not react to replay.
func TestInjectConversationItem(t *testing.T) {
	tests := []struct {
		role        string
		wantContent string
	}{
		{"user", "input_text"},
		{"system", "input_text"},
		{"assistant", "text"},
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			f := (&fakeGrok{}).start(t)
			c, s := connectFake(t, f, Config{})

			if err := c.InjectConversationItem(context.Background(), tt.role, "prior turn"); err != nil {
				t.Fatalf("InjectConversationItem: %v", err)
			}
			msg := s.next(t)
			if msg["type"] != "conversation.item.create" {
				t.Fatalf("sent %v, want conversation.item.create", msg["type"])
			}
			role, content := itemContent(t, msg)
			if role != tt.role {
				t.Errorf("role = %q, want %q", role, tt.role)
			}
			if content["type"] != tt.wantContent || content["text"] != "prior turn" {
				t.Errorf("content = %#v, want %s carrying the replayed text", content, tt.wantContent)
			}

			// Replay must not trigger a response: the next message on
			// the wire is the sentinel we send ourselves.
			if err := c.ClearBuffer(context.Background()); err != nil {
				t.Fatalf("ClearBuffer: %v", err)
			}
			if got := s.next(t)["type"]; got != "input_audio_buffer.clear" {
				t.Errorf("message after replay = %v, want no response.create", got)
			}
		})
	}
}

// TestInjectConversationItemEmptyIsNoop is the negative case: empty text
// must not put a blank item into the conversation.
func TestInjectConversationItemEmptyIsNoop(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{})

	if err := c.InjectConversationItem(context.Background(), "user", ""); err != nil {
		t.Fatalf("InjectConversationItem: %v", err)
	}
	if err := c.ClearBuffer(context.Background()); err != nil {
		t.Fatalf("ClearBuffer: %v", err)
	}
	if got := s.next(t)["type"]; got != "input_audio_buffer.clear" {
		t.Errorf("sent %v for empty text, want nothing at all", got)
	}
}

func TestSendSystemNote(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{})

	if err := c.SendSystemNote(context.Background(), "task finished", ModalitiesText); err != nil {
		t.Fatalf("SendSystemNote: %v", err)
	}

	role, content := itemContent(t, s.nextOfType(t, "conversation.item.create"))
	if role != "system" {
		t.Errorf("role = %q, want system", role)
	}
	if content["type"] != "input_text" || content["text"] != "task finished" {
		t.Errorf("content = %#v, want the note as input_text", content)
	}
	if got := modalities(t, s.nextOfType(t, "response.create")); !reflect.DeepEqual(got, []any{"text"}) {
		t.Errorf("modalities = %#v, want text only", got)
	}
}

func TestInjectAssistantText(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, s := connectFake(t, f, Config{})

	if err := c.InjectAssistantText(context.Background(), "read this aloud"); err != nil {
		t.Fatalf("InjectAssistantText: %v", err)
	}

	role, content := itemContent(t, s.nextOfType(t, "conversation.item.create"))
	if role != "assistant" {
		t.Errorf("role = %q, want assistant", role)
	}
	if content["type"] != "text" {
		t.Errorf("content type = %v, want text for an assistant turn", content["type"])
	}

	create := s.nextOfType(t, "response.create")
	if got := modalities(t, create); !reflect.DeepEqual(got, []any{"audio"}) {
		t.Errorf("modalities = %#v, want audio only", got)
	}
	response := create["response"].(map[string]any)
	if _, ok := response["instructions"].(string); !ok {
		t.Errorf("response = %#v, want read-aloud instructions", response)
	}
}

// TestServerEventCallbacks drives each documented server event through
// a live session and checks the matching Config callback fires with the
// payload the caller is promised.
func TestServerEventCallbacks(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	audio := make(chan []byte, 4)
	transcript := make(chan string, 4)
	transcriptDone := make(chan struct{}, 4)
	userTranscript := make(chan string, 4)
	responseDone := make(chan struct{}, 4)
	onError := make(chan error, 4)

	_, s := connectFake(t, f, Config{
		OnAudio:          func(pcm []byte) { audio <- pcm },
		OnTranscript:     func(text string) { transcript <- text },
		OnTranscriptDone: func() { transcriptDone <- struct{}{} },
		OnUserTranscript: func(text string) { userTranscript <- text },
		OnResponseDone:   func() { responseDone <- struct{}{} },
		OnError:          func(err error) { onError <- err },
	})

	pcm := []byte{0xde, 0xad, 0xbe, 0xef}
	s.send(map[string]any{
		"type":  "response.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString(pcm),
	})
	select {
	case got := <-audio:
		if !reflect.DeepEqual(got, pcm) {
			t.Errorf("OnAudio = %v, want the decoded PCM %v", got, pcm)
		}
	case <-time.After(waitFor):
		t.Fatal("OnAudio never fired")
	}

	s.send(map[string]any{"type": "response.output_audio_transcript.delta", "delta": "hel"})
	select {
	case got := <-transcript:
		if got != "hel" {
			t.Errorf("OnTranscript = %q, want the delta", got)
		}
	case <-time.After(waitFor):
		t.Fatal("OnTranscript never fired")
	}

	s.send(map[string]any{"type": "response.output_audio_transcript.done"})
	select {
	case <-transcriptDone:
	case <-time.After(waitFor):
		t.Fatal("OnTranscriptDone never fired")
	}

	s.send(map[string]any{
		"type":       "conversation.item.input_audio_transcription.completed",
		"transcript": "what is the time",
	})
	select {
	case got := <-userTranscript:
		if got != "what is the time" {
			t.Errorf("OnUserTranscript = %q, want the transcript", got)
		}
	case <-time.After(waitFor):
		t.Fatal("OnUserTranscript never fired")
	}

	s.send(map[string]any{"type": "response.done"})
	select {
	case <-responseDone:
	case <-time.After(waitFor):
		t.Fatal("OnResponseDone never fired")
	}

	s.send(map[string]any{"type": "error", "error": map[string]any{"message": "rate limited"}})
	select {
	case err := <-onError:
		if err == nil || err.Error() != "grok server: rate limited" {
			t.Errorf("OnError = %v, want the server's message", err)
		}
	case <-time.After(waitFor):
		t.Fatal("OnError never fired for a server error event")
	}
}

// TestMalformedServerEventsAreSurvived is the fault case for the read
// loop: junk frames and undecodable audio must not kill the session or
// invent callbacks.
func TestMalformedServerEventsAreSurvived(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	audio := make(chan []byte, 4)
	responseDone := make(chan struct{}, 1)
	_, s := connectFake(t, f, Config{
		OnAudio:        func(pcm []byte) { audio <- pcm },
		OnResponseDone: func() { responseDone <- struct{}{} },
	})

	s.sendRaw([]byte("this is not JSON"))
	s.send(map[string]any{"type": "response.output_audio.delta", "delta": "!!! not base64 !!!"})
	s.send(map[string]any{"type": "some.event.we.do.not.handle"})
	s.send(map[string]any{"type": "response.done"})

	select {
	case <-responseDone:
	case <-time.After(waitFor):
		t.Fatal("the read loop stopped delivering events after malformed input")
	}
	select {
	case got := <-audio:
		t.Errorf("OnAudio fired with %v for undecodable base64, want no callback", got)
	default:
	}
}

// TestFunctionCallRoundTrip covers the tool path end to end: the
// handler's result goes back as function_call_output, followed by a
// continuation request.
func TestFunctionCallRoundTrip(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	type call struct {
		name string
		args json.RawMessage
	}
	calls := make(chan call, 1)
	_, s := connectFake(t, f, Config{
		OnFunctionCall: func(name string, args json.RawMessage) (string, error) {
			calls <- call{name, args}
			return `{"temp":21}`, nil
		},
	})

	s.send(map[string]any{
		"type":      "response.function_call_arguments.done",
		"call_id":   "call-1",
		"name":      "get_weather",
		"arguments": `{"city":"Melbourne"}`,
	})

	select {
	case got := <-calls:
		if got.name != "get_weather" {
			t.Errorf("handler name = %q, want get_weather", got.name)
		}
		if string(got.args) != `{"city":"Melbourne"}` {
			t.Errorf("handler args = %s, want the raw arguments", got.args)
		}
	case <-time.After(waitFor):
		t.Fatal("OnFunctionCall never fired")
	}

	out := s.nextOfType(t, "conversation.item.create")
	item := out["item"].(map[string]any)
	if item["type"] != "function_call_output" || item["call_id"] != "call-1" {
		t.Errorf("item = %#v, want the output for call-1", item)
	}
	if item["output"] != `{"temp":21}` {
		t.Errorf("output = %v, want the handler's result", item["output"])
	}
	if got := s.next(t)["type"]; got != "response.create" {
		t.Errorf("follow-up = %v, want response.create", got)
	}
}

// TestFunctionCallFaults covers the two failure modes the client must
// report back to the model rather than swallow.
func TestFunctionCallFaults(t *testing.T) {
	tests := []struct {
		name       string
		handler    func(string, json.RawMessage) (string, error)
		wantOutput string
	}{
		{
			name:       "handler error",
			handler:    func(string, json.RawMessage) (string, error) { return "", errors.New("boom") },
			wantOutput: `{"error":"boom"}`,
		},
		{
			name:       "no handler configured",
			handler:    nil,
			wantOutput: `{"error":"no handler"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := (&fakeGrok{}).start(t)
			_, s := connectFake(t, f, Config{OnFunctionCall: tt.handler})

			s.send(map[string]any{
				"type":      "response.function_call_arguments.done",
				"call_id":   "call-9",
				"name":      "explode",
				"arguments": `{}`,
			})

			item := s.nextOfType(t, "conversation.item.create")["item"].(map[string]any)
			if item["output"] != tt.wantOutput {
				t.Errorf("output = %v, want %s", item["output"], tt.wantOutput)
			}
			if got := s.next(t)["type"]; got != "response.create" {
				t.Errorf("follow-up = %v, want response.create", got)
			}
		})
	}
}
