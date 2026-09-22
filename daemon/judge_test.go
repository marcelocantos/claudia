// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia"
)

// Judge through the daemon (🎯T127): the consumer's plain Ask is evaluated
// by the daemon with the daemon's key, and a consumer that configured its
// own transport stays direct.

const daemonJevAnswer = `{"model":"jev-1.13.0",
 "answers":{"urgent":{"type":"noul","noul":0.95},
  "dept":{"type":"choice","choice":"billing","probabilities":{"billing":0.88,"technical":0.12},"confidence":0.81}},
 "usage":{"input_tokens":296,"output_tokens":20}}`

// jevServer answers every request with daemonJevAnswer and records the
// bearer token each carried.
func jevServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, daemonJevAnswer)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

func judgeReq() claudia.JudgeRequest {
	return claudia.JudgeRequest{
		State: "Help! My payouts have been failing for 3 days.",
		Questions: map[string]claudia.JudgeQuestion{
			"urgent": {Type: claudia.JudgeNoul, Instructions: "Does this convey urgency?"},
			"dept":   {Type: claudia.JudgeChoice, Instructions: "Which team?", Options: map[string]any{"billing": nil, "technical": nil}},
		},
	}
}

// countDaemonJudges wraps the daemon's evaluator so a test can see which
// process ran an evaluation.
func countDaemonJudges(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := daemonRunJudge
	daemonRunJudge = func(ctx context.Context, body json.RawMessage) (*claudia.JudgeResult, error) {
		n.Add(1)
		return prev(ctx, body)
	}
	t.Cleanup(func() { daemonRunJudge = prev })
	return &n
}

func TestJudgeGoesThroughDaemon(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	srv, auth := jevServer(t)
	t.Setenv("CLAUDIA_JEV_ENDPOINT", srv.URL)
	t.Setenv("TYPESAFE_API_KEY", "daemon-key")
	ran := countDaemonJudges(t)

	res, err := claudia.NewJudge(claudia.JudgeConfig{}).Ask(context.Background(), judgeReq())
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Fatalf("daemon ran %d evaluations, want 1", ran.Load())
	}
	if auth.Load() != "Bearer daemon-key" {
		t.Fatalf("API saw %v", auth.Load())
	}
	if res.Model != "jev-1.13.0" || res.RequestedModel != "jev-latest" || res.Usage.InputTokens != 296 ||
		res.Answers["dept"].Probabilities["technical"] != 0.12 || res.Answers["urgent"].Noul != 0.95 {
		t.Fatalf("result via daemon = %+v", res)
	}
}

func TestJudgeWithOwnTransportStaysDirect(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	srv, auth := jevServer(t)
	ran := countDaemonJudges(t)

	res, err := claudia.NewJudge(claudia.JudgeConfig{APIKey: "caller-key", Endpoint: srv.URL}).Ask(context.Background(), judgeReq())
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 || auth.Load() != "Bearer caller-key" || res.Model != "jev-1.13.0" {
		t.Fatalf("daemon ran %d, API saw %v, model %q", ran.Load(), auth.Load(), res.Model)
	}

	j := claudia.NewJudge(claudia.JudgeConfig{})
	j.SetDirect(true)
	t.Setenv("CLAUDIA_JEV_ENDPOINT", srv.URL)
	t.Setenv("TYPESAFE_API_KEY", "env-key")
	if _, err := j.Ask(context.Background(), judgeReq()); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 {
		t.Fatalf("SetDirect(true) still went through the daemon")
	}
}

func TestJudgeDaemonWithoutKeyReportsNoKey(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	srv, _ := jevServer(t)
	t.Setenv("CLAUDIA_JEV_ENDPOINT", srv.URL)
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("HOME", t.TempDir())
	ran := countDaemonJudges(t)

	_, err := claudia.NewJudge(claudia.JudgeConfig{}).Ask(context.Background(), judgeReq())
	if !errors.Is(err, claudia.ErrJudgeNoKey) || ran.Load() != 1 {
		t.Fatalf("err = %v after %d daemon evaluations, want ErrJudgeNoKey from the daemon", err, ran.Load())
	}
}
