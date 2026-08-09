// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/broker/brokertest"
)

// epoch is a fixed start time so every assertion below is on an exact value,
// never on "roughly now".
var epoch = time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)

// TestClassifyResultLine is the classifier's oracle. It is the single place the
// wire shape of backpressure is interpreted, so it is table-tested in both
// directions: the lines that must yield a verdict, and the lines that must not
// (an over-broad classifier that called an auth failure "rate limited" would
// make the AIMD controller halve its cap on the wrong evidence).
func TestClassifyResultLine(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantOK     bool
		wantKind   broker.SignalKind
		wantStatus int
		wantRetry  time.Duration
	}{{
		name:       "429 by error type",
		line:       `{"type":"result","subtype":"error","is_error":true,"error":{"type":"rate_limit_error","status":429,"message":"rate limited"}}`,
		wantOK:     true,
		wantKind:   broker.SignalRateLimited,
		wantStatus: 429,
	}, {
		name:       "429 by status alone",
		line:       `{"type":"result","is_error":true,"error":{"type":"api_error","status":429}}`,
		wantOK:     true,
		wantKind:   broker.SignalRateLimited,
		wantStatus: 429,
	}, {
		name:       "429 carries retry_after",
		line:       `{"type":"result","is_error":true,"error":{"type":"rate_limit_error","status":429,"retry_after":2.5}}`,
		wantOK:     true,
		wantKind:   broker.SignalRateLimited,
		wantStatus: 429,
		wantRetry:  2500 * time.Millisecond,
	}, {
		name:       "529 overloaded is not a quota signal",
		line:       `{"type":"result","is_error":true,"error":{"type":"overloaded_error","status":529}}`,
		wantOK:     true,
		wantKind:   broker.SignalOverloaded,
		wantStatus: 529,
	}, {
		name:     "clean result is the additive-increase trigger",
		line:     `{"type":"result","subtype":"success","cost_usd":0.0123}`,
		wantOK:   true,
		wantKind: broker.SignalOK,
	}, {
		name:   "auth failure carries no capacity verdict",
		line:   `{"type":"result","subtype":"error","is_error":true,"error":{"type":"authentication_error","status":401}}`,
		wantOK: false,
	}, {
		name:   "assistant turn is not a verdict",
		line:   `{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5}}}`,
		wantOK: false,
	}, {
		name:   "garbage is not a verdict",
		line:   `not json at all`,
		wantOK: false,
	}, {
		name:   "blank line is not a verdict",
		line:   ``,
		wantOK: false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := broker.ClassifyResultLine([]byte(tc.line), epoch)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v (signal %+v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind=%v, want %v", got.Kind, tc.wantKind)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status=%d, want %d", got.Status, tc.wantStatus)
			}
			if got.RetryAfter != tc.wantRetry {
				t.Errorf("RetryAfter=%v, want %v", got.RetryAfter, tc.wantRetry)
			}
			if !got.At.Equal(epoch) {
				t.Errorf("At=%v, want the injected clock's %v", got.At, epoch)
			}
		})
	}
}

// TestScriptedBackpressureStampsInjectedClock proves the tape a policy
// simulator replays is timed by the ManualClock, not the wall clock: the same
// tape emitted around the same advances always yields the same timestamps.
func TestScriptedBackpressureStampsInjectedClock(t *testing.T) {
	clk := broker.NewManualClock(epoch)
	src := broker.NewScriptedBackpressure(clk,
		broker.Signal{Kind: broker.SignalOK},
		broker.Signal{Kind: broker.SignalRateLimited, Status: 429},
		broker.Signal{Kind: broker.SignalOK},
	)

	src.Emit()
	clk.Advance(30 * time.Second)
	src.Emit()
	clk.Advance(90 * time.Second)
	src.Emit()

	want := []struct {
		kind broker.SignalKind
		at   time.Time
	}{
		{broker.SignalOK, epoch},
		{broker.SignalRateLimited, epoch.Add(30 * time.Second)},
		{broker.SignalOK, epoch.Add(2 * time.Minute)},
	}
	for i, w := range want {
		sig, ok := <-src.Signals()
		if !ok {
			t.Fatalf("signal %d: channel closed early", i)
		}
		if sig.Kind != w.kind || !sig.At.Equal(w.at) {
			t.Fatalf("signal %d = %v@%v, want %v@%v", i, sig.Kind, sig.At, w.kind, w.at)
		}
	}
	if _, ok := <-src.Signals(); ok {
		t.Fatal("channel should close once the tape is exhausted")
	}
}

// TestJSONLBackpressureSkipsNonVerdicts proves the production source publishes
// only lines that carry a verdict, so a policy loop counting signals is not
// fooled by usage blocks interleaved with results.
func TestJSONLBackpressureSkipsNonVerdicts(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"result","subtype":"success"}`,
		`garbage`,
		`{"type":"result","is_error":true,"error":{"type":"rate_limit_error","status":429}}`,
	}, "\n")

	src := broker.NewJSONLBackpressure(broker.NewManualClock(epoch), strings.NewReader(stream))
	var got []broker.SignalKind
	for sig := range src.Signals() {
		got = append(got, sig.Kind)
	}
	want := []broker.SignalKind{broker.SignalOK, broker.SignalRateLimited}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestFakeClaude429ReachesPolicyAsSignal is the composition oracle: the
// behavioural fake manufactures the 429 referent, the production JSONL source
// classifies it, and policy sees a SignalRateLimited — end to end, headless,
// with zero API credit and no real claude binary. This is the loop T2.2's AIMD
// simulator will drive; if the fake's wire shape and the classifier ever drift
// apart, this test is what notices.
func TestFakeClaude429ReachesPolicyAsSignal(t *testing.T) {
	f := brokertest.Build(t)
	out, _ := f.Command(t, brokertest.Scenario{RateLimited: true}).Output()

	src := broker.NewJSONLBackpressure(broker.NewManualClock(epoch), strings.NewReader(string(out)))
	sig, ok := <-src.Signals()
	if !ok {
		t.Fatalf("fake claude 429 produced no signal; raw output:\n%s", out)
	}
	if sig.Kind != broker.SignalRateLimited {
		t.Fatalf("Kind=%v, want %v; raw output:\n%s", sig.Kind, broker.SignalRateLimited, out)
	}
	if sig.Status != 429 {
		t.Errorf("Status=%d, want 429", sig.Status)
	}
}

// TestFakeClaudeUsageBlocksAreParseable proves the canned usage blocks the fake
// emits are the shape T2.4's cost differential consumes, and that they carry no
// backpressure verdict.
func TestFakeClaudeUsageBlocksAreParseable(t *testing.T) {
	f := brokertest.Build(t)
	out, err := f.Command(t, brokertest.Scenario{
		Usage: []brokertest.UsageBlock{
			{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 900},
			{InputTokens: 80, OutputTokens: 12},
		},
		CostUSD: 0.0451,
	}).Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	total := 0
	for _, u := range brokertest.ParseUsage(t, out) {
		total += u.InputTokens + u.OutputTokens
	}
	if want := 1200 + 340 + 80 + 12; total != want {
		t.Fatalf("token total %d, want %d; raw output:\n%s", total, want, out)
	}

	src := broker.NewJSONLBackpressure(broker.NewManualClock(epoch), strings.NewReader(string(out)))
	for sig := range src.Signals() {
		if sig.Kind != broker.SignalOK {
			t.Fatalf("usage-only run produced %v, want only %v", sig.Kind, broker.SignalOK)
		}
	}
}
