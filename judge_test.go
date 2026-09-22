// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Hermetic oracles for Judge mode (🎯T127). The fixture server answers the
// documented POST /v1/systemone shape (https://docs.typesafe.ai/api.md); the
// response bodies below are the doc's own examples, with the eval's model
// release, so a decoder that drifts from the contract goes red here.

const fixtureKey = "ts-test-key-do-not-echo"

// jevFixture is a TypeSafe endpoint that records every request and answers
// from a queue (the last reply repeats once the queue drains).
type jevFixture struct {
	srv *httptest.Server

	mu      sync.Mutex
	bodies  [][]byte
	auths   []string
	replies []jevReply
}

type jevReply struct {
	status     int
	body       string
	retryAfter string
}

func newJevFixture(t *testing.T, replies ...jevReply) *jevFixture {
	t.Helper()
	f := &jevFixture{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		reply := f.replies[0]
		if len(f.replies) > 1 {
			f.replies = f.replies[1:]
		}
		f.mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "want POST application/json", http.StatusBadRequest)
			return
		}
		if reply.retryAfter != "" {
			w.Header().Set("Retry-After", reply.retryAfter)
		}
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *jevFixture) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

// judgeFor returns a Judge pointed at f, with retries recorded instead of
// slept.
func judgeFor(f *jevFixture, waits *[]time.Duration) *Judge {
	j := NewJudge(JudgeConfig{APIKey: fixtureKey, Endpoint: f.srv.URL})
	j.sleep = func(_ context.Context, d time.Duration) error {
		if waits != nil {
			*waits = append(*waits, d)
		}
		return nil
	}
	return j
}

// The three documented question shapes, in one request.
func triageRequest() JudgeRequest {
	return JudgeRequest{
		State: "Help! My payouts have been failing for 3 days.",
		Questions: map[string]JudgeQuestion{
			"is_urgent": {Type: JudgeNoul, Instructions: "Does this convey urgency?",
				Yes: "Explicitly time-sensitive", No: "No urgency expressed"},
			"department": {Type: JudgeChoice, Instructions: "Which team should handle this?",
				Options: map[string]any{"billing": "Payments, invoicing, refunds", "technical": "Bugs, outages, integrations", "sales": nil}},
			"frustration": {Type: JudgeScore, Instructions: "How frustrated is the customer?",
				Levels: []any{"Calm", "Frustrated", "Very angry"}},
		},
	}
}

const triageAnswer = `{
  "model": "jev-1.13.0",
  "answers": {
    "is_urgent": {"type": "noul", "noul": 0.95},
    "department": {"type": "choice", "choice": "billing",
      "probabilities": {"billing": 0.88, "technical": 0.12, "sales": 0.0}, "confidence": 0.81},
    "frustration": {"type": "score", "score": 1.05,
      "legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
      "probabilities": {"0": 0.0, "1": 0.95, "2": 0.05}, "confidence": 0.92}
  },
  "usage": {"input_tokens": 318, "output_tokens": 34}
}`

// TestJudgeRequestEncoding pins the body on the wire: every question type in
// the API's own shape, the default model, and the bearer key.
func TestJudgeRequestEncoding(t *testing.T) {
	f := newJevFixture(t, jevReply{status: 200, body: triageAnswer})
	if _, err := judgeFor(f, nil).Ask(context.Background(), triageRequest()); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	var got, want any
	if err := json.Unmarshal(reqs[0], &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(`{
	  "state": "Help! My payouts have been failing for 3 days.",
	  "model": "jev-latest",
	  "questions": {
	    "is_urgent": {"type": "noul", "instructions": "Does this convey urgency?",
	      "criteria": {"true": "Explicitly time-sensitive", "false": "No urgency expressed"}},
	    "department": {"type": "choice", "instructions": "Which team should handle this?",
	      "criteria": {"billing": "Payments, invoicing, refunds", "technical": "Bugs, outages, integrations", "sales": null}},
	    "frustration": {"type": "score", "instructions": "How frustrated is the customer?",
	      "criteria": ["Calm", "Frustrated", "Very angry"]}
	  }
	}`), &want)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("request body\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if f.auths[0] != "Bearer "+fixtureKey {
		t.Fatalf("Authorization = %q", f.auths[0])
	}
}

// TestJudgeDecodesDistributionsVersionAndUsage is the acceptance core: the
// full distribution for every answer, the resolved release, and the tokens.
func TestJudgeDecodesDistributionsVersionAndUsage(t *testing.T) {
	f := newJevFixture(t, jevReply{status: 200, body: triageAnswer})
	res, err := judgeFor(f, nil).Ask(context.Background(), triageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "jev-1.13.0" || res.RequestedModel != "jev-latest" {
		t.Fatalf("model = %q requested %q", res.Model, res.RequestedModel)
	}
	if res.Usage != (Usage{InputTokens: 318, OutputTokens: 34}) || res.Attempts != 1 {
		t.Fatalf("usage = %+v attempts %d", res.Usage, res.Attempts)
	}
	if a := res.Answers["is_urgent"]; a.Type != JudgeNoul || a.Noul != 0.95 {
		t.Fatalf("noul = %+v", a)
	}
	dept := res.Answers["department"]
	if dept.Choice != "billing" || dept.Confidence != 0.81 || len(dept.Probabilities) != 3 ||
		dept.Probabilities["technical"] != 0.12 {
		t.Fatalf("choice = %+v", dept)
	}
	if _, ok := dept.Probabilities["sales"]; !ok {
		t.Fatal("a zero-probability option was dropped from the distribution")
	}
	fr := res.Answers["frustration"]
	if fr.Score != 1.05 || fr.Legend["2"] != "Very angry" || fr.Probabilities["1"] != 0.95 || len(fr.Probabilities) != 3 {
		t.Fatalf("score = %+v", fr)
	}
	// The result survives the daemon wire intact.
	raw, err := EncodeJudgeResultWire(res)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeJudgeResultWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(back); string(b) != string(raw) {
		t.Fatalf("wire round trip changed the result:\n%s\n%s", raw, b)
	}
}

// TestJudgeModelOverride: request beats config beats the default.
func TestJudgeModelOverride(t *testing.T) {
	f := newJevFixture(t, jevReply{status: 200, body: triageAnswer})
	j := judgeFor(f, nil)
	j.cfg.Model = "jev-1.12.0"
	req := triageRequest()
	if _, err := j.Ask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Model = "jev-1.11.0"
	res, err := j.Ask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var first, second struct{ Model string }
	_ = json.Unmarshal(f.requests()[0], &first)
	_ = json.Unmarshal(f.requests()[1], &second)
	if first.Model != "jev-1.12.0" || second.Model != "jev-1.11.0" || res.RequestedModel != "jev-1.11.0" {
		t.Fatalf("models sent %q, %q; requested %q", first.Model, second.Model, res.RequestedModel)
	}
}

// TestJudgeRefusesMalformedRequestsLocally: the documented limits are
// checked before a round trip, and nothing is sent.
func TestJudgeRefusesMalformedRequestsLocally(t *testing.T) {
	many := map[string]any{}
	for i := range judgeMaxChoiceOptions + 1 {
		many[string(rune('a'+i%26))+strings.Repeat("x", i)] = nil
	}
	cases := map[string]JudgeRequest{
		"no state":         {Questions: map[string]JudgeQuestion{"q": {Type: JudgeNoul, Instructions: "?"}}},
		"no questions":     {State: "s"},
		"no instructions":  {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeNoul}}},
		"unknown type":     {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: "rank", Instructions: "?"}}},
		"choice no opts":   {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeChoice, Instructions: "?"}}},
		"choice too many":  {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeChoice, Instructions: "?", Options: many}}},
		"score one level":  {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeScore, Instructions: "?", Levels: []any{"x"}}}},
		"score 11 levels":  {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeScore, Instructions: "?", Levels: make([]any, 11)}}},
		"noul with opts":   {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeNoul, Instructions: "?", Options: map[string]any{"a": nil}}}},
		"choice with yes":  {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeChoice, Instructions: "?", Options: map[string]any{"a": nil}, Yes: "y"}}},
		"score with opts":  {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeScore, Instructions: "?", Levels: []any{"a", "b"}, Options: map[string]any{"a": nil}}}},
		"noul with levels": {State: "s", Questions: map[string]JudgeQuestion{"q": {Type: JudgeNoul, Instructions: "?", Levels: []any{"a", "b"}}}},
	}
	f := newJevFixture(t, jevReply{status: 200, body: triageAnswer})
	for name, req := range cases {
		_, err := judgeFor(f, nil).Ask(context.Background(), req)
		var je *JudgeError
		if !errors.As(err, &je) || je.Status != 0 {
			t.Errorf("%s: err = %v, want a local *JudgeError", name, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests reached the API for refused input", n)
	}
}

// TestJudgeRefusesResponsesThatDoNotAnswer: the trust boundary. Each
// response here is 200 OK and wrong.
func TestJudgeRefusesResponsesThatDoNotAnswer(t *testing.T) {
	cases := map[string]string{
		"missing answer": `{"model":"jev-1.13.0","answers":{"is_urgent":{"type":"noul","noul":0.9},
			"department":{"type":"choice","choice":"billing","probabilities":{"billing":1,"technical":0,"sales":0}}},"usage":{}}`,
		"wrong type":  strings.Replace(triageAnswer, `{"type": "noul", "noul": 0.95}`, `{"type": "score", "score": 0.95}`, 1),
		"option lost": strings.Replace(triageAnswer, `"technical": 0.12, `, ``, 1),
		"level lost":  strings.Replace(triageAnswer, `"probabilities": {"0": 0.0, "1": 0.95, "2": 0.05}`, `"probabilities": {"0": 0.0, "1": 0.95}`, 1),
		"noul > 1":    strings.Replace(triageAnswer, `"noul": 0.95`, `"noul": 1.5`, 1),
		"no model":    strings.Replace(triageAnswer, `"model": "jev-1.13.0",`, ``, 1),
		"not json":    `<html>gateway</html>`,
	}
	for name, body := range cases {
		f := newJevFixture(t, jevReply{status: 200, body: body})
		res, err := judgeFor(f, nil).Ask(context.Background(), triageRequest())
		var je *JudgeError
		if !errors.As(err, &je) || je.Status != 200 {
			t.Errorf("%s: res=%+v err=%v, want a *JudgeError with status 200", name, res, err)
		}
	}
}

// TestJudgeRetriesRateLimitsWithBackoff: 429 and 529 are retried with
// doubling waits, Retry-After raises a wait, and the retry count is bounded.
func TestJudgeRetriesRateLimitsWithBackoff(t *testing.T) {
	var waits []time.Duration
	f := newJevFixture(t,
		jevReply{status: 429, body: `{"error":"rate limited"}`},
		jevReply{status: 529, body: `{"error":"overloaded"}`, retryAfter: "3"},
		jevReply{status: 200, body: triageAnswer})
	res, err := judgeFor(f, &waits).Ask(context.Background(), triageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 3 || len(f.requests()) != 3 {
		t.Fatalf("attempts = %d, requests = %d", res.Attempts, len(f.requests()))
	}
	want := []time.Duration{judgeRetryBase, 3 * time.Second}
	if len(waits) != 2 || waits[0] != want[0] || waits[1] != want[1] {
		t.Fatalf("waits = %v, want %v", waits, want)
	}

	waits = nil
	f = newJevFixture(t, jevReply{status: 529, body: `{"error":"overloaded"}`})
	_, err = judgeFor(f, &waits).Ask(context.Background(), triageRequest())
	var je *JudgeError
	if !errors.As(err, &je) || je.Status != 529 || !strings.Contains(je.Message, "overloaded") {
		t.Fatalf("err = %v, want a 529 *JudgeError", err)
	}
	if n := len(f.requests()); n != judgeMaxRetries+1 {
		t.Fatalf("%d requests for a service that stays overloaded, want %d", n, judgeMaxRetries+1)
	}
	if len(waits) != judgeMaxRetries || waits[2] != 4*judgeRetryBase {
		t.Fatalf("waits = %v", waits)
	}
}

// TestJudgeDoesNotRetryRefusals: 401 and 422 are the caller's to fix, so
// they come back at once with the API's body.
func TestJudgeDoesNotRetryRefusals(t *testing.T) {
	for _, status := range []int{401, 422} {
		f := newJevFixture(t, jevReply{status: status, body: `{"detail":"questions.q.criteria: field required"}`})
		_, err := judgeFor(f, nil).Ask(context.Background(), triageRequest())
		var je *JudgeError
		if !errors.As(err, &je) || je.Status != status || !strings.Contains(je.Message, "field required") {
			t.Fatalf("%d: err = %v", status, err)
		}
		if n := len(f.requests()); n != 1 {
			t.Fatalf("%d: %d requests, want 1", status, n)
		}
	}
}

// TestJudgeKeyLookup: the environment wins, the key file is read for the
// one variable only (export and quotes tolerated), and a missing key names
// both places.
func TestJudgeKeyLookup(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{}
	j := NewJudge(JudgeConfig{})
	j.getenv = func(k string) string { return env[k] }
	j.home = func() (string, error) { return home, nil }

	_, err := j.lookupKey()
	if !errors.Is(err, ErrJudgeNoKey) {
		t.Fatalf("no key anywhere: err = %v", err)
	}
	if msg := ErrJudgeNoKey.Error(); !strings.Contains(msg, "TYPESAFE_API_KEY") || !strings.Contains(msg, "~/.typesafe/env") {
		t.Fatalf("ErrJudgeNoKey does not name both places: %q", msg)
	}

	if err := os.MkdirAll(filepath.Join(home, ".typesafe"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := "# TypeSafe\nOTHER=nope\nexport TYPESAFE_API_KEY=\"from-file\"\n"
	if err := os.WriteFile(filepath.Join(home, ".typesafe", "env"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := j.lookupKey(); err != nil || k != "from-file" {
		t.Fatalf("file key = %q, %v", k, err)
	}

	env["TYPESAFE_API_KEY"] = "from-env"
	if k, err := j.lookupKey(); err != nil || k != "from-env" {
		t.Fatalf("env key = %q, %v", k, err)
	}
}

// TestJudgeNoKeyIsRefusedBeforeAnyRequest: a direct Ask with no key sends
// nothing.
func TestJudgeNoKeyIsRefusedBeforeAnyRequest(t *testing.T) {
	f := newJevFixture(t, jevReply{status: 200, body: triageAnswer})
	j := NewJudge(JudgeConfig{Endpoint: f.srv.URL})
	j.getenv = func(string) string { return "" }
	j.home = func() (string, error) { return t.TempDir(), nil }
	if _, err := j.Ask(context.Background(), triageRequest()); !errors.Is(err, ErrJudgeNoKey) {
		t.Fatalf("err = %v, want ErrJudgeNoKey", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent without a key", n)
	}
}

// TestJudgeErrorsNeverCarryTheKey: neither a refusal nor a transport
// failure puts the key in the error a caller might log.
func TestJudgeErrorsNeverCarryTheKey(t *testing.T) {
	f := newJevFixture(t, jevReply{status: 401, body: `{"detail":"invalid api key"}`})
	_, err := judgeFor(f, nil).Ask(context.Background(), triageRequest())
	if err == nil || strings.Contains(err.Error(), fixtureKey) {
		t.Fatalf("401 error = %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	j := NewJudge(JudgeConfig{APIKey: fixtureKey, Endpoint: url})
	_, err = j.Ask(context.Background(), triageRequest())
	if err == nil || strings.Contains(err.Error(), fixtureKey) {
		t.Fatalf("transport error = %v", err)
	}
}

// TestJudgedWireCarriesEveryOutcome: the daemon's answer decodes to exactly
// what the direct path returns — the result, ErrJudgeNoKey, or a
// *JudgeError with the API's status.
func TestJudgedWireCarriesEveryOutcome(t *testing.T) {
	if _, err := decodeJudged(EncodeJudged(nil, ErrJudgeNoKey)); !errors.Is(err, ErrJudgeNoKey) {
		t.Fatalf("no key: %v", err)
	}
	_, err := decodeJudged(EncodeJudged(nil, &JudgeError{Status: 422, Message: "bad criteria"}))
	var je *JudgeError
	if !errors.As(err, &je) || je.Status != 422 || je.Message != "bad criteria" {
		t.Fatalf("refusal: %v", err)
	}
	_, err = decodeJudged(EncodeJudged(nil, errors.New(judgeErrPrefix+"dial tcp: refused")))
	if !errors.As(err, &je) || je.Status != 0 || err.Error() != judgeErrPrefix+"dial tcp: refused" {
		t.Fatalf("transport: %v", err)
	}
	want := &JudgeResult{Model: "jev-1.13.0", RequestedModel: "jev-latest", Answers: map[string]JudgeAnswer{"q": {Type: JudgeNoul, Noul: 0.4}}, Attempts: 1}
	got, err := decodeJudged(EncodeJudged(want, nil))
	if err != nil || got.Model != want.Model || got.Answers["q"].Noul != 0.4 {
		t.Fatalf("result: %+v, %v", got, err)
	}
}
