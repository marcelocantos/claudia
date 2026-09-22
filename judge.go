// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Judge mode (🎯T127): typed questions over a state, answered by a System
// One model (TypeSafe's Jev) with a probability for every answer.
//
// It is a third mode beside Task and Session, not a provider under them.
// Task and Session generate text; a judgment is a typed value with a
// distribution, and nothing in Task's event stream has a slot for one. The
// substitution that does make sense runs the other way — an LLM emulating
// Jev behind this API — which is why the mode, not the vendor, is the type.
//
// Every answer carries its full distribution, not only the pick. On the
// 2026-09-22 eval (30 ytt synopses) Jev's top choice said "material" for 25
// of them while P(material) ranked material against the rest at AUC 0.94:
// the pick was useless and the probability was the signal. Thresholds are
// therefore the caller's; this mode reports and never decides.
//
// API contract: https://docs.typesafe.ai/api.md

// DefaultJudgeEndpoint is TypeSafe's System One evaluation endpoint.
const DefaultJudgeEndpoint = "https://api.typesafe.ai/v1/systemone"

// DefaultJudgeModel is the alias the API resolves to its current Jev
// release. The resolved version comes back on every [JudgeResult].
const DefaultJudgeModel = "jev-latest"

// judgeKeyEnv is the variable TypeSafe's SDKs read, and the one line this
// package reads from judgeKeyFile.
const judgeKeyEnv = "TYPESAFE_API_KEY"

// judgeKeyFile is where the key lives when the environment has none,
// relative to the home directory.
const judgeKeyFile = ".typesafe/env"

// judgeEndpointEnv overrides the endpoint for a process that did not set
// JudgeConfig.Endpoint — the daemon, pointed at a fixture by its test.
const judgeEndpointEnv = "CLAUDIA_JEV_ENDPOINT"

// Limits the API documents for one question.
const (
	judgeMaxChoiceOptions = 255
	judgeMinScoreLevels   = 2
	judgeMaxScoreLevels   = 10
)

// Retry policy for 429 (rate limited) and 529 (overloaded), which the API
// says to retry with exponential backoff. Bounded: a service that stays
// overloaded is the caller's to hear about, not a loop to sit in.
const (
	judgeMaxRetries     = 3
	judgeRetryBase      = 500 * time.Millisecond
	judgeRetryCap       = 8 * time.Second
	judgeStatusOverload = 529
)

// judgeMaxBody bounds what is read from one response. A judgment is a few
// hundred bytes per question; this is headroom, not an estimate.
const judgeMaxBody = 8 << 20

// JudgeQuestionType is the primitive a question asks for.
type JudgeQuestionType string

const (
	// JudgeNoul asks whether a condition holds; the answer is P(yes).
	JudgeNoul JudgeQuestionType = "noul"
	// JudgeChoice picks one of a defined set; the answer is a distribution
	// over the set.
	JudgeChoice JudgeQuestionType = "choice"
	// JudgeScore places the state on ordered levels; the answer is the
	// probability-weighted position and a distribution over the levels.
	JudgeScore JudgeQuestionType = "score"
)

// JudgeQuestion is one typed question. Instructions, and every criterion,
// may be a string or any JSON-marshalable object or array: the API accepts
// structure there and refers to it by backticked field name.
//
// Set the criteria field that matches Type and leave the others empty:
// Options for a Choice (required), Levels for a Score (required, ordered
// lowest first), Yes/No for a Noul (optional).
type JudgeQuestion struct {
	Type         JudgeQuestionType
	Instructions any

	// Options maps each Choice option to its description; a nil value
	// means the option needs none. At most 255.
	Options map[string]any
	// Levels describes each Score level in order. 2 to 10 of them.
	Levels []any
	// Yes and No say what a Noul's yes and no mean.
	Yes, No any
}

// JudgeRequest is one evaluation: a state and the questions asked of it.
// Questions over the same state belong in one request — the API runs them
// in parallel and none sees another's answer — and a request per question
// pays the state's input tokens again for each.
type JudgeRequest struct {
	// State is what is judged: a string, or any JSON-marshalable value.
	State any
	// Questions maps an id of the caller's choosing to its question. The
	// id is not sent to the model; the question must carry its meaning.
	Questions map[string]JudgeQuestion
	// Model overrides JudgeConfig.Model for this request.
	Model string
}

// JudgeAnswer is one question's answer. Which fields are set depends on
// Type; Probabilities is set for Choice and Score and is the field a
// caller should threshold on.
type JudgeAnswer struct {
	Type JudgeQuestionType `json:"type"`

	// Noul is P(yes), 0 to 1. A value near 0.5 means yes and no are about
	// equally likely, not a condition that half holds.
	Noul float64 `json:"noul,omitempty"`

	// Choice is the highest-probability option. Do not act on it alone:
	// read Probabilities.
	Choice string `json:"choice,omitempty"`

	// Score is the probability-weighted level index, which can fall
	// between levels.
	Score float64 `json:"score,omitempty"`
	// Legend maps each level index ("0", "1", …) back to its description.
	Legend map[string]string `json:"legend,omitempty"`

	// Probabilities maps every Choice option, or every Score level index,
	// to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence summarises how concentrated Probabilities is (Choice and
	// Score). It is not the probability that the answer is right.
	Confidence float64 `json:"confidence,omitempty"`
}

// JudgeResult is one evaluation's answers and what it cost.
type JudgeResult struct {
	// Model is the model that answered, as the API reports it: requesting
	// "jev-latest" comes back as a release such as "jev-1.13.0". A
	// threshold is tuned against a release, so record this with every
	// answer that is kept.
	Model string `json:"model"`
	// RequestedModel is what was asked for.
	RequestedModel string `json:"requested_model"`
	// Answers maps each question id to its answer.
	Answers map[string]JudgeAnswer `json:"answers"`
	// Usage is the API's token count. The API reports no cache fields, so
	// the cache counts are zero because they are not reported, not because
	// nothing was cached.
	Usage Usage `json:"usage"`
	// DurationMs is wall time for the evaluation, retries included.
	DurationMs float64 `json:"duration_ms"`
	// Attempts is how many HTTP requests it took (more than one after a
	// 429 or 529).
	Attempts int `json:"attempts"`
}

// ErrJudgeNoKey means no API key was found. It names both places it looks.
var ErrJudgeNoKey = errors.New("claudia: judge: no TypeSafe API key — set " + judgeKeyEnv +
	" in the environment, or write " + judgeKeyEnv + "=… to ~/" + judgeKeyFile)

// JudgeError is a refusal from the API, or from a daemon relaying one.
type JudgeError struct {
	// Status is the HTTP status: 401 bad key, 422 a request that failed
	// validation (Message names the field), 429 rate limited, 529
	// overloaded. Retries of 429 and 529 were exhausted before this is
	// returned.
	Status int
	// Message is the API's body, or a local reason when Status is 0.
	Message string
}

// judgeErrPrefix begins every error this mode returns.
const judgeErrPrefix = "claudia: judge: "

func (e *JudgeError) Error() string {
	if e.Status == 0 {
		return judgeErrPrefix + e.Message
	}
	return fmt.Sprintf("%sHTTP %d: %s", judgeErrPrefix, e.Status, e.Message)
}

// JudgeConfig configures a [Judge]. The zero value asks jev-latest at
// TypeSafe with the key from the environment or ~/.typesafe/env.
type JudgeConfig struct {
	// Model is the default model; DefaultJudgeModel when empty.
	Model string
	// APIKey is used instead of the environment and key file. Never
	// logged. Setting it makes every Ask direct: the daemon answers with
	// its own key, not the caller's.
	APIKey string
	// Endpoint replaces DefaultJudgeEndpoint (CLAUDIA_JEV_ENDPOINT when
	// empty). Setting it makes every Ask direct.
	Endpoint string
	// HTTPClient replaces http.DefaultClient. Setting it makes every Ask
	// direct.
	HTTPClient *http.Client
}

// Judge asks typed questions. Safe for concurrent use.
//
// When a claudia daemon runs, Ask goes through it and the daemon calls the
// API with its own key; with no daemon, a daemon too old to know Judge,
// CLAUDIA_NO_BROKER=1, SetDirect(true), or any transport field set on
// JudgeConfig, Ask calls the API from this process. Both paths run the same
// code and return the same result.
type Judge struct {
	cfg    JudgeConfig
	direct bool

	// sleep and getenv are seams for the retry and key-lookup tests.
	sleep  func(context.Context, time.Duration) error
	getenv func(string) string
	home   func() (string, error)
}

// NewJudge returns a Judge for cfg.
func NewJudge(cfg JudgeConfig) *Judge {
	return &Judge{cfg: cfg, sleep: sleepContext, getenv: os.Getenv, home: os.UserHomeDir}
}

// SetDirect makes every Ask call the API from this process, never the
// daemon. It must be called before Ask.
func (j *Judge) SetDirect(direct bool) { j.direct = direct }

// Ask evaluates req. Every question id in req comes back in
// JudgeResult.Answers, with an answer of the type it asked for, or Ask
// returns an error: a partial or mistyped response is refused rather than
// handed on with holes.
func (j *Judge) Ask(ctx context.Context, req JudgeRequest) (*JudgeResult, error) {
	if req.Model == "" {
		req.Model = j.cfg.Model
	}
	if req.Model == "" {
		req.Model = DefaultJudgeModel
	}
	body, err := encodeJudgeRequest(req)
	if err != nil {
		return nil, err
	}
	transportSet := j.cfg.APIKey != "" || j.cfg.Endpoint != "" || j.cfg.HTTPClient != nil
	if !j.direct && !transportSet {
		res, err := judgeViaBroker(ctx, body)
		if err == nil || !brokerFellThrough(err) {
			return res, err
		}
	}
	return j.askDirect(ctx, body)
}

// judgeQuestionWire is a question in the API's form.
type judgeQuestionWire struct {
	Type         JudgeQuestionType `json:"type"`
	Instructions any               `json:"instructions"`
	Criteria     any               `json:"criteria,omitempty"`
}

// judgeRequestWire is the API's request body. It is also what crosses the
// daemon socket, so the daemon sends exactly the bytes a direct caller
// would.
type judgeRequestWire struct {
	State     any                          `json:"state"`
	Model     string                       `json:"model"`
	Questions map[string]judgeQuestionWire `json:"questions"`
}

// encodeJudgeRequest validates req against the API's documented limits and
// renders the body. Refusing here gives the caller the field by name
// instead of a 422 after a network round trip.
func encodeJudgeRequest(req JudgeRequest) (json.RawMessage, error) {
	if req.State == nil {
		return nil, &JudgeError{Message: "state is required"}
	}
	if len(req.Questions) == 0 {
		return nil, &JudgeError{Message: "at least one question is required"}
	}
	w := judgeRequestWire{State: req.State, Model: req.Model, Questions: map[string]judgeQuestionWire{}}
	for _, id := range judgeSortedKeys(req.Questions) {
		q := req.Questions[id]
		if q.Instructions == nil || q.Instructions == "" {
			return nil, &JudgeError{Message: fmt.Sprintf("question %q: instructions are required", id)}
		}
		qw := judgeQuestionWire{Type: q.Type, Instructions: q.Instructions}
		misplaced := func(field string) error {
			return &JudgeError{Message: fmt.Sprintf("question %q: %s is not a %s criterion", id, field, q.Type)}
		}
		switch q.Type {
		case JudgeNoul:
			if q.Options != nil {
				return nil, misplaced("Options")
			}
			if q.Levels != nil {
				return nil, misplaced("Levels")
			}
			if q.Yes != nil || q.No != nil {
				crit := map[string]any{}
				if q.Yes != nil {
					crit["true"] = q.Yes
				}
				if q.No != nil {
					crit["false"] = q.No
				}
				qw.Criteria = crit
			}
		case JudgeChoice:
			if q.Levels != nil {
				return nil, misplaced("Levels")
			}
			if q.Yes != nil || q.No != nil {
				return nil, misplaced("Yes/No")
			}
			if len(q.Options) == 0 {
				return nil, &JudgeError{Message: fmt.Sprintf("question %q: a choice needs Options", id)}
			}
			if len(q.Options) > judgeMaxChoiceOptions {
				return nil, &JudgeError{Message: fmt.Sprintf("question %q: %d options; the API accepts at most %d",
					id, len(q.Options), judgeMaxChoiceOptions)}
			}
			// A nil description is sent as JSON null, which the API
			// documents as "no extra detail".
			qw.Criteria = q.Options
		case JudgeScore:
			if q.Options != nil {
				return nil, misplaced("Options")
			}
			if q.Yes != nil || q.No != nil {
				return nil, misplaced("Yes/No")
			}
			if len(q.Levels) < judgeMinScoreLevels || len(q.Levels) > judgeMaxScoreLevels {
				return nil, &JudgeError{Message: fmt.Sprintf("question %q: %d levels; a score takes %d to %d",
					id, len(q.Levels), judgeMinScoreLevels, judgeMaxScoreLevels)}
			}
			qw.Criteria = q.Levels
		default:
			return nil, &JudgeError{Message: fmt.Sprintf("question %q: unknown type %q (want noul, choice or score)", id, q.Type)}
		}
		w.Questions[id] = qw
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, &JudgeError{Message: "encode request: " + err.Error()}
	}
	return raw, nil
}

// judgeResponseWire is the API's response body.
type judgeResponseWire struct {
	Model   string                 `json:"model"`
	Answers map[string]JudgeAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// RunJudgeWire is one evaluation of a request already in wire form: the
// daemon's judge handler, and the direct path's own, so both answer from the
// same code. The key and endpoint come from this process's environment.
func RunJudgeWire(ctx context.Context, body json.RawMessage) (*JudgeResult, error) {
	return NewJudge(JudgeConfig{}).askDirect(ctx, body)
}

func (j *Judge) askDirect(ctx context.Context, body json.RawMessage) (*JudgeResult, error) {
	var asked judgeRequestWire
	if err := json.Unmarshal(body, &asked); err != nil {
		return nil, &JudgeError{Message: "decode request: " + err.Error()}
	}
	key := j.cfg.APIKey
	if key == "" {
		var err error
		if key, err = j.lookupKey(); err != nil {
			return nil, err
		}
	}
	endpoint := j.cfg.Endpoint
	if endpoint == "" {
		endpoint = j.getenv(judgeEndpointEnv)
	}
	if endpoint == "" {
		endpoint = DefaultJudgeEndpoint
	}
	client := j.cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	start := time.Now()
	var status int
	var respBody []byte
	attempts := 0
	for {
		attempts++
		var retryAfter time.Duration
		var err error
		status, respBody, retryAfter, err = judgePost(ctx, client, endpoint, key, body)
		if err != nil {
			return nil, err
		}
		retryable := status == http.StatusTooManyRequests || status == judgeStatusOverload
		if !retryable || attempts > judgeMaxRetries {
			break
		}
		wait := judgeRetryBase << (attempts - 1)
		if retryAfter > wait {
			wait = retryAfter
		}
		wait = min(wait, judgeRetryCap)
		if err := j.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, &JudgeError{Status: status, Message: strings.TrimSpace(string(respBody))}
	}

	var resp judgeResponseWire
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, &JudgeError{Status: status, Message: "decode response: " + err.Error()}
	}
	if err := checkJudgeAnswers(asked, resp); err != nil {
		return nil, err
	}
	return &JudgeResult{
		Model:          resp.Model,
		RequestedModel: asked.Model,
		Answers:        resp.Answers,
		Usage:          Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
		DurationMs:     float64(time.Since(start).Microseconds()) / 1000,
		Attempts:       attempts,
	}, nil
}

// judgePost sends one request. A transport failure is an error; any HTTP
// status is a result for the caller to judge.
func judgePost(ctx context.Context, client *http.Client, endpoint, key string, body []byte) (int, []byte, time.Duration, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, &JudgeError{Message: "build request: " + err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		// url.Error carries the URL, never the header, so the key does not
		// reach this message.
		return 0, nil, 0, fmt.Errorf("claudia: judge: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, judgeMaxBody+1))
	if err != nil {
		return 0, nil, 0, fmt.Errorf("claudia: judge: read response: %w", err)
	}
	if len(raw) > judgeMaxBody {
		return 0, nil, 0, &JudgeError{Status: resp.StatusCode, Message: fmt.Sprintf("response larger than %d bytes", judgeMaxBody)}
	}
	var retryAfter time.Duration
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		retryAfter = time.Duration(secs) * time.Second
	}
	return resp.StatusCode, raw, retryAfter, nil
}

// checkJudgeAnswers refuses a response that does not answer what was asked:
// a missing id, an answer of another type, or a distribution without the
// options the question defined. The API promises all three; this is the
// trust boundary where that promise is checked rather than assumed.
func checkJudgeAnswers(asked judgeRequestWire, resp judgeResponseWire) error {
	if resp.Model == "" {
		return &JudgeError{Status: http.StatusOK, Message: "response names no model"}
	}
	for _, id := range judgeSortedKeys(asked.Questions) {
		q := asked.Questions[id]
		a, ok := resp.Answers[id]
		if !ok {
			return &JudgeError{Status: http.StatusOK, Message: fmt.Sprintf("response has no answer for %q", id)}
		}
		if a.Type != q.Type {
			return &JudgeError{Status: http.StatusOK, Message: fmt.Sprintf("answer %q is %q; the question was %q", id, a.Type, q.Type)}
		}
		switch q.Type {
		case JudgeNoul:
			if a.Noul < 0 || a.Noul > 1 {
				return &JudgeError{Status: http.StatusOK, Message: fmt.Sprintf("answer %q: noul %v is outside 0..1", id, a.Noul)}
			}
		case JudgeChoice:
			// asked came back through JSON, so Criteria is a map here.
			opts, _ := q.Criteria.(map[string]any)
			for opt := range opts {
				if _, ok := a.Probabilities[opt]; !ok {
					return &JudgeError{Status: http.StatusOK, Message: fmt.Sprintf("answer %q: no probability for option %q", id, opt)}
				}
			}
		case JudgeScore:
			levels, _ := q.Criteria.([]any)
			for i := range levels {
				if _, ok := a.Probabilities[strconv.Itoa(i)]; !ok {
					return &JudgeError{Status: http.StatusOK, Message: fmt.Sprintf("answer %q: no probability for level %d", id, i)}
				}
			}
		}
	}
	return nil
}

// lookupKey reads the key from the environment, else from the key file.
// Only the one variable is read from the file; the file is never sourced,
// and the value is never logged or placed in an error.
func (j *Judge) lookupKey() (string, error) {
	if k := strings.TrimSpace(j.getenv(judgeKeyEnv)); k != "" {
		return k, nil
	}
	home, err := j.home()
	if err != nil || home == "" {
		return "", ErrJudgeNoKey
	}
	f, err := os.Open(filepath.Join(home, judgeKeyFile))
	if err != nil {
		return "", ErrJudgeNoKey
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != judgeKeyEnv {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if value != "" {
			return value, nil
		}
	}
	return "", ErrJudgeNoKey
}

// EncodeJudgeResultWire is a JudgeResult in its daemon-protocol form (judged).
func EncodeJudgeResultWire(res *JudgeResult) (json.RawMessage, error) {
	return json.Marshal(res)
}

// DecodeJudgeResultWire reverses [EncodeJudgeResultWire].
func DecodeJudgeResultWire(raw json.RawMessage) (*JudgeResult, error) {
	var res JudgeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("broker judge result: %w", err)
	}
	return &res, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func judgeSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
