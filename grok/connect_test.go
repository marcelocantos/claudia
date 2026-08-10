// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package grok

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestConnectRequiresAPIKey(t *testing.T) {
	dialed := false
	_, err := Connect(context.Background(), Config{
		Dial: &DialArgs{
			Dial: func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
				dialed = true
				return nil, nil, errors.New("should not be reached")
			},
		},
	})
	if err == nil {
		t.Fatal("Connect with no API key succeeded, want error")
	}
	if !strings.Contains(err.Error(), "API key required") {
		t.Errorf("error = %v, want it to mention the missing API key", err)
	}
	if dialed {
		t.Error("Connect dialled despite the missing API key")
	}
}

func TestConnectDialFailure(t *testing.T) {
	sentinel := errors.New("no route to host")
	_, err := Connect(context.Background(), Config{
		APIKey: testAPIKey,
		Dial: &DialArgs{
			Dial: func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
				return nil, nil, sentinel
			},
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "dial failed") {
		t.Errorf("error = %v, want it to mention the failed dial", err)
	}
}

// TestConnectDefaultEndpoint pins the production URL: the seam must not
// silently retarget callers that leave Config.Dial nil.
func TestConnectDefaultEndpoint(t *testing.T) {
	var got string
	_, err := Connect(context.Background(), Config{
		APIKey: testAPIKey,
		Dial: &DialArgs{
			Dial: func(_ context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
				got = url
				return nil, nil, errors.New("stop here")
			},
		},
	})
	if err == nil {
		t.Fatal("Connect succeeded against a failing dialler, want error")
	}
	if got != "wss://api.x.ai/v1/realtime" {
		t.Errorf("dialled %q, want the xAI Realtime endpoint", got)
	}
}

// TestConnectUpgradeRejected covers auth failure at the handshake: the
// server answers the upgrade with 401 rather than switching protocols.
func TestConnectUpgradeRejected(t *testing.T) {
	f := (&fakeGrok{upgradeStatus: http.StatusUnauthorized}).start(t)

	_, err := Connect(context.Background(), Config{APIKey: testAPIKey, Dial: f.dialArgs()})
	if err == nil {
		t.Fatal("Connect succeeded against a 401 endpoint, want error")
	}
	if !strings.Contains(err.Error(), "dial failed") {
		t.Errorf("error = %v, want it to mention the failed dial", err)
	}
	if got, want := f.authHeader(t), "Bearer "+testAPIKey; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// TestConnectServerRejectsSession covers the protocol-level auth
// failure: the upgrade succeeds but the first server event is an error.
func TestConnectServerRejectsSession(t *testing.T) {
	f := (&fakeGrok{
		firstReply: func(*fakeSession, map[string]any) any {
			return map[string]any{
				"type":  "error",
				"error": map[string]any{"message": "invalid api key"},
			}
		},
	}).start(t)

	onError := make(chan error, 1)
	_, err := Connect(context.Background(), Config{
		APIKey:  testAPIKey,
		Dial:    f.dialArgs(),
		OnError: func(err error) { onError <- err },
	})
	if err == nil {
		t.Fatal("Connect succeeded against a rejecting server, want error")
	}
	if !strings.Contains(err.Error(), "server rejected session: invalid api key") {
		t.Errorf("error = %v, want the server's rejection message", err)
	}
	// The rejection is reported through the return value only — no
	// event loop started, so OnError must stay silent.
	select {
	case err := <-onError:
		t.Errorf("OnError fired with %v, want no callback for a failed Connect", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := f.session(t).closeErr(t); err == nil {
		t.Error("server saw no disconnect, want the client to close the rejected session")
	}
}

// TestConnectServerRejectsSessionUntypedError covers the fallback when
// the error event carries no structured error object.
func TestConnectServerRejectsSessionUntypedError(t *testing.T) {
	f := (&fakeGrok{
		firstReply: func(*fakeSession, map[string]any) any {
			return map[string]any{"type": "error", "error": "flat string"}
		},
	}).start(t)

	_, err := Connect(context.Background(), Config{APIKey: testAPIKey, Dial: f.dialArgs()})
	if err == nil {
		t.Fatal("Connect succeeded against a rejecting server, want error")
	}
	if !strings.Contains(err.Error(), "flat string") {
		t.Errorf("error = %v, want the raw rejection payload", err)
	}
}

// TestConnectFirstReadFailure covers the server hanging up before it
// acknowledges the session.
func TestConnectFirstReadFailure(t *testing.T) {
	f := (&fakeGrok{
		firstReply: func(s *fakeSession, _ map[string]any) any {
			_ = s.conn.Close(websocket.StatusInternalError, "boom")
			return nil
		},
	}).start(t)

	_, err := Connect(context.Background(), Config{APIKey: testAPIKey, Dial: f.dialArgs()})
	if err == nil {
		t.Fatal("Connect succeeded after the server hung up, want error")
	}
	if !strings.Contains(err.Error(), "first read failed") {
		t.Errorf("error = %v, want it to name the failed first read", err)
	}
}

func TestConnectContextCancelledBeforeDial(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Connect(ctx, Config{APIKey: testAPIKey, Dial: f.dialArgs()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
}

// TestSessionConfigDefaults pins the session.update payload for a
// default session: server-side VAD, Eve, 24 kHz PCM both ways.
func TestSessionConfigDefaults(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	_, s := connectFake(t, f, Config{})

	want := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"voice": "Eve",
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": float64(24000)}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": float64(24000)}},
			},
			"turn_detection": map[string]any{
				"type":                "server_vad",
				"threshold":           0.7,
				"silence_duration_ms": float64(800),
				"prefix_padding_ms":   float64(300),
			},
		},
	}
	if !reflect.DeepEqual(s.sessionUpdate, want) {
		t.Errorf("session.update =\n%#v\nwant\n%#v", s.sessionUpdate, want)
	}
}

// TestSessionConfigManualCommit pins the explicit null that disables
// server VAD — omitting the key would leave the API on server_vad.
func TestSessionConfigManualCommit(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	_, s := connectFake(t, f, Config{ManualCommit: true})

	session, ok := s.sessionUpdate["session"].(map[string]any)
	if !ok {
		t.Fatalf("session.update has no session object: %#v", s.sessionUpdate)
	}
	td, present := session["turn_detection"]
	if !present {
		t.Fatal("turn_detection absent; the API would default to server_vad")
	}
	if td != nil {
		t.Errorf("turn_detection = %#v, want explicit null", td)
	}
}

func TestSessionConfigVoicePromptAndTools(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	_, s := connectFake(t, f, Config{
		Voice:        "Rex",
		SystemPrompt: "be brief",
		Tools: []Tool{{
			Type:        "function",
			Name:        "lookup",
			Description: "look something up",
			Parameters:  []byte(`{"type":"object"}`),
		}},
	})

	session := s.sessionUpdate["session"].(map[string]any)
	if got := session["voice"]; got != "Rex" {
		t.Errorf("voice = %v, want Rex", got)
	}
	if got := session["instructions"]; got != "be brief" {
		t.Errorf("instructions = %v, want the system prompt", got)
	}
	tools, ok := session["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one tool", session["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "lookup" || tool["type"] != "function" {
		t.Errorf("tool = %#v, want the configured function tool", tool)
	}
	if !reflect.DeepEqual(tool["parameters"], map[string]any{"type": "object"}) {
		t.Errorf("tool parameters = %#v, want the JSON Schema object", tool["parameters"])
	}
}

// TestSessionConfigOmitsUnsetFields guards against sending empty
// instructions or an empty tool list, which the API treats as meaningful.
func TestSessionConfigOmitsUnsetFields(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	_, s := connectFake(t, f, Config{})

	session := s.sessionUpdate["session"].(map[string]any)
	for _, key := range []string{"instructions", "tools"} {
		if _, present := session[key]; present {
			t.Errorf("session.%s present with nothing configured", key)
		}
	}
}

// TestConnectSessionReadyCallback covers the acknowledgement Connect
// blocks on being dispatched to the caller's callback.
func TestConnectSessionReadyCallback(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	ready := make(chan struct{}, 1)
	connectFake(t, f, Config{OnSessionReady: func() { ready <- struct{}{} }})

	select {
	case <-ready:
	case <-time.After(waitFor):
		t.Fatal("OnSessionReady never fired for session.updated")
	}
}

// TestCloseNormalClosure covers the documented teardown: a normal
// closure reaches the server and no error is reported to the caller.
func TestCloseNormalClosure(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	onError := make(chan error, 1)
	c, s := connectFake(t, f, Config{OnError: func(err error) { onError <- err }})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := websocket.CloseStatus(s.closeErr(t)); got != websocket.StatusNormalClosure {
		t.Errorf("close status = %v, want %v", got, websocket.StatusNormalClosure)
	}
	select {
	case err := <-onError:
		t.Errorf("OnError fired with %v after a deliberate Close", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, _ := connectFake(t, f, Config{})

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestSendAfterCloseFails is the negative case for the method surface:
// a closed session must reject sends rather than silently drop them.
func TestSendAfterCloseFails(t *testing.T) {
	f := (&fakeGrok{}).start(t)
	c, _ := connectFake(t, f, Config{})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.SendText(context.Background(), "hello", ModalitiesText); err == nil {
		t.Error("SendText after Close succeeded, want error")
	}
	if err := c.SendAudio(context.Background(), []byte{1, 2, 3}); err == nil {
		t.Error("SendAudio after Close succeeded, want error")
	}
}

// TestReadLoopErrorReportsToCaller covers an unexpected server hangup
// mid-session — distinct from the deliberate Close above.
func TestReadLoopErrorReportsToCaller(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	onError := make(chan error, 1)
	_, s := connectFake(t, f, Config{OnError: func(err error) { onError <- err }})

	if err := s.conn.Close(websocket.StatusInternalError, "server exploded"); err != nil {
		t.Fatalf("server close: %v", err)
	}
	select {
	case err := <-onError:
		if err == nil {
			t.Error("OnError fired with a nil error")
		}
	case <-time.After(waitFor):
		t.Fatal("OnError never fired after the server hung up")
	}
}

// TestContextCancellationEndsSession covers the documented lifetime:
// the event loop runs until Close or ctx cancellation.
func TestContextCancellationEndsSession(t *testing.T) {
	f := (&fakeGrok{}).start(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	onError := make(chan error, 1)
	c, err := Connect(ctx, Config{
		APIKey:  testAPIKey,
		Dial:    f.dialArgs(),
		OnError: func(err error) { onError <- err },
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	s := f.session(t)
	s.nextOfType(t, "session.update")

	cancel()

	select {
	case err := <-onError:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("OnError = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(waitFor):
		t.Fatal("the event loop did not report the cancelled context")
	}
	// Teardown after cancellation must still be clean and prompt.
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-closed:
	case <-time.After(waitFor):
		t.Fatal("Close hung after the context was cancelled")
	}
	if err := s.closeErr(t); err == nil {
		t.Error("server saw no disconnect after the session was torn down")
	}
}
