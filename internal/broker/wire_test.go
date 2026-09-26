// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file is the class-1 half of 🎯T2.1's declared oracle (docs/broker-oracles.md):
// golden message vectors pinning the wire format byte-for-byte, plus the census
// tests wire.go names — the ones that fail the build when a new field or message
// type is added without deciding what the broker does with it.
//
// The golden vectors are the mutation target: renaming any json tag on any
// message changes the bytes and turns TestGoldenWireVectors red, which is what
// makes "the wire format is stable" a checked claim rather than an intention.

// updateGolden rewrites the vectors instead of comparing against them. It is a
// separate flag rather than an env var so a test run cannot set it by accident.
var updateGolden = flag.Bool("update-golden", false, "rewrite the golden wire vectors")

// goldenDir is where the vectors live.
const goldenDir = "testdata/wire"

// A fixed instant for every vector carrying a timestamp. Vectors must not move
// when the suite runs, so nothing here reads a clock.
var goldenAt = time.Date(2026, 6, 10, 14, 30, 0, 0, time.UTC)

// requestVector is one pinned client → broker message. parsed, when set, is
// what ParseRequest must hand back — it differs from msg exactly where the
// broker normalises a defaulted field, and stating both is how the vector pins
// the normalisation as well as the bytes.
type requestVector struct {
	msg    *Request
	parsed *Request
}

// requestVectors is every client → broker message, in canonical form.
var requestVectors = map[string]requestVector{
	"spawn_minimal": {
		msg: &Request{
			ID: "r1", Type: TypeSpawn,
			Spawn: &SpawnRequest{Mode: ModeSession, WorkDir: "/w"},
		},
		// An omitted provider and intent come back filled in: the broker
		// resolves the default once, on receipt, so nothing downstream has to
		// re-implement "empty means claude".
		parsed: &Request{
			ID: "r1", Type: TypeSpawn,
			Spawn: &SpawnRequest{
				Provider: ProviderClaude, Mode: ModeSession,
				Intent: IntentAuto, WorkDir: "/w",
			},
		},
	},
	"spawn_full": {
		msg: &Request{
			ID: "r2", Type: TypeSpawn,
			Spawn: &SpawnRequest{
				Provider: ProviderClaude,
				Mode:     ModeTask,
				Model:    "claude-opus-5",
				Intent:   IntentBatch,
				WorkDir:  "/w/repo",
			},
		},
	},
	"release_stop": {
		msg: &Request{
			ID: "r3", Type: TypeRelease,
			Release: &ReleaseRequest{SessionID: "s-1", Disposition: DispositionStop},
		},
	},
	"release_reuse": {
		msg: &Request{
			ID: "r4", Type: TypeRelease,
			Release: &ReleaseRequest{SessionID: "s-1", Disposition: DispositionReuse},
		},
	},
	"status": {msg: &Request{ID: "r5", Type: TypeStatus, Status: &StatusRequest{}}},
	"tail":   {msg: &Request{ID: "r6", Type: TypeTail, Tail: &TailRequest{}}},

	// Grant protocol (🎯T2.9 / 🎯T2.10). Embedded claudia payloads are
	// opaque here; their field sets are pinned in the claudia package.
	"release_detach": {
		msg: &Request{
			ID: "g0", Type: TypeRelease,
			Release: &ReleaseRequest{Name: "jv-worker-1", Disposition: DispositionDetach},
		},
	},
	"usage":         {msg: &Request{ID: "g1", Type: TypeUsage, Usage: &UsageRequest{}}},
	"usage_refresh": {msg: &Request{ID: "g1", Type: TypeUsage, Usage: &UsageRequest{Refresh: true}}},
	"resolve": {
		msg: &Request{
			ID: "g2", Type: TypeResolve,
			Resolve: &ResolveRequest{Predicates: json.RawMessage(`{"mode":"task","prefer_plan":true}`)},
		},
	},
	"judge": {
		msg: &Request{
			ID: "j1", Type: TypeJudge,
			Judge: &JudgeRequest{Request: json.RawMessage(`{"state":"payouts failing","model":"jev-latest","questions":{"urgent":{"type":"noul","instructions":"Urgent?"}}}`)},
		},
	},
	"task_run": {
		msg: &Request{
			ID: "g3", Type: TypeTaskRun,
			TaskRun: &TaskRunRequest{Task: json.RawMessage(`{"provider":"grok","workdir":"/w"}`), Prompt: "summarise"},
		},
	},
	"task_run_raw_log": {
		msg: &Request{
			ID: "g3", Type: TypeTaskRun,
			TaskRun: &TaskRunRequest{Task: json.RawMessage(`{"provider":"claude","workdir":"/w"}`), Prompt: "summarise", RawLog: true},
		},
	},
	"task_cancel": {msg: &Request{ID: "g4", Type: TypeTaskCancel, TaskCancel: &TaskCancelRequest{RunID: "run-1"}}},
	"grant_pool": {
		msg: &Request{
			ID: "g5p", Type: TypeGrant,
			Grant: &GrantRequest{Name: "sid:3f1c", Def: json.RawMessage(`{"name":"sid:3f1c","workdir":"/w"}`), Pool: &PoolGrant{Policy: "spawn", Cap: 2}},
		},
	},
	"release_detach_force":     {msg: &Request{ID: "g6f", Type: TypeRelease, Release: &ReleaseRequest{Name: "jv-worker-1", Disposition: DispositionDetach, Force: true}}},
	"release_reuse_keep_alive": {msg: &Request{ID: "g6k", Type: TypeRelease, Release: &ReleaseRequest{Name: "sid:3f1c", Disposition: DispositionReuse, KeepAliveSeconds: 600}}},
	"grant": {
		msg: &Request{
			ID: "g5", Type: TypeGrant,
			Grant: &GrantRequest{
				Name:     "jv-worker-1",
				Def:      json.RawMessage(`{"name":"jv-worker-1","workdir":"/w","provider":"claude"}`),
				Adopt:    true,
				Fallback: true,
			},
		},
	},
	// A pre-🎯T72 send carries no mode; the bytes are unchanged and the
	// parsed form comes back normalised to submit, the same way an
	// omitted provider comes back as claude.
	"send": {
		msg:    &Request{ID: "g6", Type: TypeSend, Send: &SendRequest{Name: "jv-worker-1", Text: "hello"}},
		parsed: &Request{ID: "g6", Type: TypeSend, Send: &SendRequest{Name: "jv-worker-1", Text: "hello", Mode: SendModeSubmit}},
	},
	"send_submit":    {msg: &Request{ID: "g6", Type: TypeSend, Send: &SendRequest{Name: "jv-worker-1", Text: "hello", Mode: SendModeSubmit}}},
	"send_steer":     {msg: &Request{ID: "g6", Type: TypeSend, Send: &SendRequest{Name: "jv-worker-1", Text: "also check the tests", Mode: SendModeSteer}}},
	"send_interrupt": {msg: &Request{ID: "g6", Type: TypeSend, Send: &SendRequest{Name: "jv-worker-1", Text: "stop and summarise", Mode: SendModeInterrupt}}},
	"send_queue":     {msg: &Request{ID: "g6", Type: TypeSend, Send: &SendRequest{Name: "jv-worker-1", Text: "after this turn", Mode: SendModeQueue}}},
	"interrupt":      {msg: &Request{ID: "g7", Type: TypeInterrupt, Interrupt: &NamedRequest{Name: "jv-worker-1"}}},
	"set_model":      {msg: &Request{ID: "g8", Type: TypeSetModel, SetModel: &SetModelRequest{Name: "jv-worker-1", Model: "sonnet"}}},
	"migrate":        {msg: &Request{ID: "g9", Type: TypeMigrate, Migrate: &MigrateRequest{Name: "jv-worker-1", Provider: "grok", Model: "grok-4", Reason: "hot"}}},
	"agent_info":     {msg: &Request{ID: "g10", Type: TypeAgentInfo, AgentInfo: &NamedRequest{Name: "jv-worker-1"}}},
	"term_subscribe": {msg: &Request{ID: "g11", Type: TypeTermSubscribe, TermSubscribe: &NamedRequest{Name: "jv-worker-1"}}},
	"resize":         {msg: &Request{ID: "g12", Type: TypeResize, Resize: &ResizeRequest{Name: "jv-worker-1", Cols: 120, Rows: 40}}},
	"grants":         {msg: &Request{ID: "g13", Type: TypeGrants, Grants: &GrantsRequest{}}},
	"close_goal":     {msg: &Request{ID: "g14", Type: TypeCloseGoal, CloseGoal: &NamedRequest{Name: "jv-worker-1"}}},
	"rewind":         {msg: &Request{ID: "g15", Type: TypeRewind, Rewind: &RewindRequest{Name: "jv-worker-1", Turns: 2}}},
	"goal_verdict": {msg: &Request{ID: "g16", Type: TypeGoalVerdict,
		GoalVerdict: &GoalVerdictRequest{Name: "jv-worker-1", CheckID: "c-1", Complete: true, Answered: true}}},
}

// responseVectors is every broker → client message, in canonical form.
var responseVectors = map[string]*Response{
	"spawned": {
		ID: "r1", Type: TypeSpawned,
		Spawned: &SpawnResponse{SessionID: "s-1", PID: 4242, Warm: false},
	},
	"released": {
		ID: "r3", Type: TypeReleased,
		Released: &ReleaseResponse{SessionID: "s-1", Disposition: DispositionStop},
	},
	"status_result_empty": {
		ID: "r5", Type: TypeStatusResult,
		Status: &StatusResponse{ProtocolVersion: Version, Sessions: []SessionStatus{}},
	},
	"status_result_one": {
		ID: "r5", Type: TypeStatusResult,
		Status: &StatusResponse{
			ProtocolVersion: Version,
			ActiveAgents:    1,
			Sessions: []SessionStatus{{
				SessionID: "s-1",
				Provider:  ProviderClaude,
				Mode:      ModeSession,
				Model:     "claude-opus-5",
				Intent:    IntentAuto,
				WorkDir:   "/w",
				PID:       4242,
			}},
		},
	},
	"tailing":       {ID: "r6", Type: TypeTailing, Tailing: &TailResponse{}},
	"event_spawn":   {Type: TypeEvent, Event: &EventMessage{Kind: EventSpawn, SessionID: "s-1", At: goldenAt}},
	"event_release": {Type: TypeEvent, Event: &EventMessage{Kind: EventRelease, SessionID: "s-1", At: goldenAt}},
	"event_reclaim": {Type: TypeEvent, Event: &EventMessage{Kind: EventReclaim, SessionID: "s-1", At: goldenAt}},
	"error_unsupported_version": {
		ID: "r7", Type: TypeError,
		Error: &ErrorMessage{
			Code:              CodeUnsupportedVersion,
			Message:           "wire version 2 is not supported",
			Field:             "v",
			Value:             "2",
			SupportedVersions: []int{Version},
		},
	},
	"error_unknown_field": {
		ID: "r8", Type: TypeError,
		Error: &ErrorMessage{
			Code:    CodeUnknownField,
			Message: `spawn body has no field "turbo"`,
			Field:   "turbo",
		},
	},
	"error_not_owner": {
		ID: "r9", Type: TypeError,
		Error: &ErrorMessage{Code: CodeNotOwner, Message: "session s-1 belongs to another connection", Field: "session_id", Value: "s-1"},
	},

	// Grant protocol.
	"released_detach": {
		ID: "g0", Type: TypeReleased,
		Released: &ReleaseResponse{Name: "jv-worker-1", Disposition: DispositionDetach},
	},
	"usage_result": {
		ID: "g1", Type: TypeUsageResult,
		Usage: &UsageResponse{FetchedAt: goldenAt, Backends: json.RawMessage(`[{"provider":"claude","status":"available"}]`)},
	},
	"usage_result_never": {
		ID: "g1", Type: TypeUsageResult,
		Usage: &UsageResponse{Backends: json.RawMessage(`[]`), Error: "not fetched yet"},
	},
	"resolved": {
		ID: "g2", Type: TypeResolved,
		Resolved: &ResolveResponse{Pick: json.RawMessage(`{"provider":"grok","model":"grok-4"}`)},
	},
	"judged": {
		ID: "j1", Type: TypeJudged,
		Judged: &JudgedResponse{Result: json.RawMessage(`{"model":"jev-1.13.0","requested_model":"jev-latest","answers":{"urgent":{"type":"noul","noul":0.95}},"usage":{"input_tokens":296,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"duration_ms":728,"attempts":1}`)},
	},
	"judged_refused": {ID: "j1", Type: TypeJudged, Judged: &JudgedResponse{Status: 422, Error: "questions.urgent.instructions: field required"}},
	"judged_no_key":  {ID: "j1", Type: TypeJudged, Judged: &JudgedResponse{NoKey: true, Error: "no TypeSafe API key"}},
	"task_started":   {ID: "g3", Type: TypeTaskStarted, TaskStarted: &TaskStartedResponse{RunID: "run-1"}},
	"task_event": {
		Type:      TypeTaskEvent,
		TaskEvent: &TaskEventMessage{RunID: "run-1", Event: json.RawMessage(`{"type":"text","content":"hi"}`)},
	},
	"task_raw": {
		Type:    TypeTaskRaw,
		TaskRaw: &TaskRawMessage{RunID: "run-1", Line: `{"type":"system","subtype":"init"}`},
	},
	"task_done":       {Type: TypeTaskDone, TaskDone: &TaskDoneMessage{RunID: "run-1"}},
	"task_done_error": {Type: TypeTaskDone, TaskDone: &TaskDoneMessage{RunID: "run-1", Error: "spawn failed"}},
	"task_cancelled":  {ID: "g4", Type: TypeTaskCancelled, TaskCancelled: &TaskCancelledResponse{RunID: "run-1"}},
	"granted": {
		ID: "g5", Type: TypeGranted,
		Granted: &GrantResponse{
			Name: "jv-worker-1", SessionID: "sid-1", Provider: ProviderClaude, Model: "opus",
			WindowID: "@7", JSONLPath: "/h/.claude/projects/-w/sid-1.jsonl", TermLogPath: "/h/.local/state/claudia/terms/-w/sid-1.term",
			AttachCommand: "tmux -L claudia attach -t @7",
		},
	},
	"granted_reclaimed": {
		ID: "g5", Type: TypeGranted,
		Granted: &GrantResponse{Name: "jv-worker-1", SessionID: "sid-1", Provider: "grok", Reclaimed: true, Replayed: 3, Lagged: true, ConnectURL: "ws://127.0.0.1:1/ws", ConnectPID: 99},
	},
	"agent_event": {
		Type:       TypeAgentEvent,
		AgentEvent: &AgentEventMessage{Name: "jv-worker-1", Event: json.RawMessage(`{"type":"assistant","text":"hi"}`)},
	},
	"agent_term":     {Type: TypeAgentTerm, AgentTerm: &AgentTermMessage{Name: "jv-worker-1", Data: []byte("\x1b[2J")}},
	"agent_gone":     {Type: TypeAgentGone, AgentGone: &AgentGoneMessage{Name: "jv-worker-1", Reason: "process exited"}},
	"agent_detached": {Type: TypeAgentDetached, AgentDetached: &AgentDetachedMessage{Name: "jv-worker-1", Reason: "consumer not reading"}},
	// A pre-🎯T72 daemon answers with the name alone; the vector is unchanged.
	"sent":           {ID: "g6", Type: TypeSent, Sent: &SentResponse{Name: "jv-worker-1"}},
	"sent_submit":    {ID: "g6", Type: TypeSent, Sent: &SentResponse{Name: "jv-worker-1", Mode: SendModeSubmit, Mechanism: "submit", PhaseBefore: "idle"}},
	"sent_steer":     {ID: "g6", Type: TypeSent, Sent: &SentResponse{Name: "jv-worker-1", Mode: SendModeSteer, Mechanism: "acp_session_prompt_supersede", PhaseBefore: "in_turn", SupersededTurnID: "t-9"}},
	"sent_interrupt": {ID: "g6", Type: TypeSent, Sent: &SentResponse{Name: "jv-worker-1", Mode: SendModeInterrupt, Mechanism: "interrupt+submit", PhaseBefore: "in_turn"}},
	"sent_queue":     {ID: "g6", Type: TypeSent, Sent: &SentResponse{Name: "jv-worker-1", Mode: SendModeQueue, Mechanism: "client_queue", PhaseBefore: "in_turn"}},
	"interrupted":    {ID: "g7", Type: TypeInterrupted, Interrupted: &NamedResponse{Name: "jv-worker-1"}},
	"model_set":      {ID: "g8", Type: TypeModelSet, ModelSet: &NamedResponse{Name: "jv-worker-1"}},
	"goal_check": {
		Type:      TypeGoalCheck,
		GoalCheck: &GoalCheckMessage{Name: "jv-worker-1", CheckID: "c-1", Goal: "ship T75", TurnText: "all done"},
	},
	"goal_verdict_noted": {ID: "g16", Type: TypeGoalVerdictNoted, GoalVerdictNoted: &NamedResponse{Name: "jv-worker-1"}},
	"rewound": {
		ID: "g15", Type: TypeRewound,
		Rewound: &RewindResponse{
			Name: "jv-worker-1", SessionID: "sid-1", Provider: ProviderClaude, WindowID: "@8",
			JSONLPath: "/h/.claude/projects/-w/sid-1.jsonl", TurnsRemoved: 2, LinesRemoved: 9, BytesRemoved: 4096,
			BackupPath: "/h/.claude/projects/-w/sid-1.jsonl.rewind-bak",
		},
	},
	"migrated": {
		ID: "g9", Type: TypeMigrated,
		Migrated: &MigrateResponse{Name: "jv-worker-1", SessionID: "sid-2", Provider: "grok", Model: "grok-4"},
	},
	"agent_info_result": {
		ID: "g10", Type: TypeAgentInfoResult,
		AgentInfo: &AgentInfoResponse{
			Name: "jv-worker-1", SessionID: "sid-1", Provider: ProviderClaude, Model: "claude-opus-5",
			Alive: true, PromptInFlight: false, Usage: json.RawMessage(`{"input_tokens":1,"output_tokens":2}`),
			WindowID: "@7",
		},
	},
	"agent_info_result_turn_caps": {
		ID: "g10", Type: TypeAgentInfoResult,
		AgentInfo: &AgentInfoResponse{
			Name: "jv-worker-1", SessionID: "sid-1", Provider: "cursor", Model: "gpt-5",
			Alive: true, PromptInFlight: true, ConnectURL: "ws://127.0.0.1:1/ws", ConnectPID: 99,
			TurnCaps: &TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: "breakpoint", BusyOnSecondSubmit: "supersede"},
		},
	},
	"granted_turn_caps": {
		ID: "g5", Type: TypeGranted,
		Granted: &GrantResponse{
			Name: "jv-worker-1", SessionID: "sid-1", Provider: ProviderClaude, Model: "opus", WindowID: "@7",
			TurnCaps: &TurnCaps{CanInterrupt: true, SteerPolicy: "queue_until_idle", BusyOnSecondSubmit: "queue"},
		},
	},
	"term_subscribed": {ID: "g11", Type: TypeTermSubscribed, TermSubscribed: &TermSubscribedResponse{Name: "jv-worker-1", History: []byte("$ ")}},
	"resized":         {ID: "g12", Type: TypeResized, Resized: &NamedResponse{Name: "jv-worker-1"}},
	"grants_result": {
		ID: "g13", Type: TypeGrantsResult,
		Grants: &GrantsResponse{Grants: []GrantStatus{{
			Name: "jv-worker-1", Provider: ProviderClaude, Model: "opus", SessionID: "sid-1",
			WorkDir: "/w", Purpose: "work", Parent: "jevons-po", Owned: true, Alive: true,
		}, {
			Name: "jv-worker-2", Provider: "grok", SessionID: "sid-2", WorkDir: "/w", Pending: 4,
		}}},
	},
	"grants_result_empty": {ID: "g13", Type: TypeGrantsResult, Grants: &GrantsResponse{Grants: []GrantStatus{}}},
	"grants_result_turn_caps": {
		ID: "g13", Type: TypeGrantsResult,
		Grants: &GrantsResponse{Grants: []GrantStatus{{
			Name: "jv-worker-1", Provider: "grok", SessionID: "sid-1", WorkDir: "/w", Owned: true, Alive: true,
			TurnCaps: &TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: "finish_slice", BusyOnSecondSubmit: "supersede"},
		}}},
	},
	"error_send_bad_mode": {
		ID: "g6", Type: TypeError,
		Error: &ErrorMessage{Code: CodeUnsupportedValue, Message: `mode "nudge" is not one of "submit", "steer", "interrupt", "queue"`, Field: "mode", Value: "nudge"},
	},
	"goal_closed":  {ID: "g14", Type: TypeGoalClosed, GoalClosed: &NamedResponse{Name: "jv-worker-1"}},
	"event_grant":  {Type: TypeEvent, Event: &EventMessage{Kind: EventGrant, Name: "jv-worker-1", At: goldenAt}},
	"event_detach": {Type: TypeEvent, Event: &EventMessage{Kind: EventDetach, Name: "jv-worker-1", At: goldenAt}},
	"event_usage":  {Type: TypeEvent, Event: &EventMessage{Kind: EventUsageUpdate, Detail: "claude", At: goldenAt}},
	"error_grant_held": {
		ID: "g5", Type: TypeError,
		Error: &ErrorMessage{Code: CodeGrantHeld, Message: "grant jv-worker-1 is owned by another connection", Field: "name", Value: "jv-worker-1"},
	},
	"error_not_available": {
		ID: "g1", Type: TypeError,
		Error: &ErrorMessage{Code: CodeNotAvailable, Message: "this broker has no daemon runtime behind it", Field: "type", Value: "usage"},
	},
}

// show renders a decoded message as the line it would encode to, because the
// default %+v of a Request prints body pointers rather than bodies.
func show(m interface{ Encode() ([]byte, error) }) string {
	line, err := m.Encode()
	if err != nil {
		return "unencodable: " + err.Error()
	}
	return string(line)
}

// goldenPath is the vector file for one named case.
func goldenPath(name string) string { return filepath.Join(goldenDir, name+".json") }

// checkGolden compares one encoded message against its vector, or rewrites the
// vector under -update-golden. Vectors are stored with a trailing newline
// because that is how they travel on the wire.
func checkGolden(t *testing.T, name string, got []byte) []byte {
	t.Helper()
	path := goldenPath(name)
	if *updateGolden {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return got
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden vector %s: %v (regenerate with -update-golden)", path, err)
	}
	want := strings.TrimSuffix(string(raw), "\n")
	if string(got) != want {
		t.Errorf("wire format changed for %s\n  got:  %s\n  want: %s", name, got, want)
	}
	return []byte(want)
}

// TestGoldenWireVectors pins the encoding of every message and proves Encode is
// the inverse of Parse for each. This is the test a renamed json tag breaks.
func TestGoldenWireVectors(t *testing.T) {
	for name, vec := range requestVectors {
		t.Run("request/"+name, func(t *testing.T) {
			line, err := vec.msg.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if line[len(line)-1] == '\n' {
				t.Fatal("Encode must not frame: the trailing newline is the transport's job")
			}
			pinned := checkGolden(t, name, line)

			back, err := ParseRequest(pinned)
			if err != nil {
				t.Fatalf("parse own vector: %v", err)
			}
			want := vec.parsed
			if want == nil {
				want = vec.msg
			}
			if !reflect.DeepEqual(back, want) {
				t.Errorf("round trip lost information\n  got:  %s\n  want: %s", show(back), show(want))
			}
		})
	}

	for name, resp := range responseVectors {
		t.Run("response/"+name, func(t *testing.T) {
			line, err := resp.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			pinned := checkGolden(t, name, line)

			back, err := ParseResponse(pinned)
			if err != nil {
				t.Fatalf("parse own vector: %v", err)
			}
			if !reflect.DeepEqual(back, resp) {
				t.Errorf("round trip lost information\n  got:  %s\n  want: %s", show(back), show(resp))
			}
		})
	}
}

// TestGoldenVectorsAreOneLine keeps the vectors honest about the framing the
// transport relies on: newline-delimited JSON only works if no message contains
// an interior newline.
func TestGoldenVectorsAreOneLine(t *testing.T) {
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no golden vectors — the pin is pinning nothing")
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(goldenDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if body := strings.TrimSuffix(string(raw), "\n"); strings.Contains(body, "\n") {
			t.Errorf("%s spans multiple lines; NDJSON framing requires exactly one", e.Name())
		}
	}
}

// TestEveryMessageTypeHasAVector stops a message type being added to the
// namespace without a pinned encoding. Without this, a new type could ship with
// no vector and the golden suite would still be green.
func TestEveryMessageTypeHasAVector(t *testing.T) {
	covered := map[MessageType]bool{}
	for _, v := range requestVectors {
		covered[v.msg.Type] = true
	}
	for _, r := range responseVectors {
		covered[r.Type] = true
	}
	all := append(RequestTypes(), ResponseTypes()...)
	for _, mt := range all {
		if !covered[mt] {
			t.Errorf("message type %q has no golden vector", mt)
		}
	}
}

// fieldDisposition is what the broker does with one SpawnRequest field.
type fieldDisposition string

const (
	// actedOn means the broker changes its behaviour because of the field.
	actedOn fieldDisposition = "acted-on"
	// recorded means the broker cannot act on it yet but stores it and reports
	// it back on the status snapshot, so a caller can confirm it was received.
	recorded fieldDisposition = "recorded-and-reported"
)

// spawnFieldDispositions declares, for every SpawnRequest field, which of the
// two honest outcomes it gets. There is deliberately no "ignored" option: a
// field the broker can neither act on nor report back must be refused with
// CodeUnsupportedValue instead of existing here (🎯T24 — a silent drop bit this
// repo twice in one day).
var spawnFieldDispositions = map[string]fieldDisposition{
	"Provider": actedOn,  // selects the binary; anything but claude is refused
	"Mode":     recorded, // task/session arg shaping lands with 🎯T2.3
	"Model":    actedOn,  // passed to the provider binary
	"Intent":   recorded, // becomes a priority tier at 🎯T2.6
	"WorkDir":  actedOn,  // the spawned process's directory
}

// TestEverySpawnRequestFieldIsHonouredOrRefused is the census wire.go promises.
// Adding a field to SpawnRequest fails the build until someone decides what the
// broker does with it, which is the only reliable way to stop a field being
// accepted on the wire and then quietly ignored.
func TestEverySpawnRequestFieldIsHonouredOrRefused(t *testing.T) {
	statusTags := map[string]bool{}
	for f := range reflect.TypeFor[SessionStatus]().Fields() {
		statusTags[jsonTagName(f)] = true
	}

	rt := reflect.TypeFor[SpawnRequest]()
	for f := range rt.Fields() {
		disp, ok := spawnFieldDispositions[f.Name]
		if !ok {
			t.Errorf("SpawnRequest.%s has no declared disposition: the broker must act on it, "+
				"or record and report it, or refuse it — never drop it silently (🎯T24)", f.Name)
			continue
		}
		// Both dispositions must be visible on the status snapshot, because
		// "the broker received what I set" is only checkable if it is echoed.
		if tag := jsonTagName(f); !statusTags[tag] {
			t.Errorf("SpawnRequest.%s (%s, json %q) is not echoed by SessionStatus, so a caller "+
				"cannot confirm it was honoured", f.Name, disp, tag)
		}
	}

	for name := range spawnFieldDispositions {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("spawnFieldDispositions names %s, which SpawnRequest no longer has", name)
		}
	}
}

// jsonTagName is the wire name of a struct field.
func jsonTagName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	return strings.Split(tag, ",")[0]
}

// TestUnknownFieldErrorShapeIsPinned guards the one place this package depends
// on encoding/json's error *wording*. If a Go release changes it, the field name
// stops reaching the caller — this fails loudly on that day rather than
// degrading the error quietly.
func TestUnknownFieldErrorShapeIsPinned(t *testing.T) {
	var target struct {
		Known string `json:"known"`
	}
	dec := json.NewDecoder(strings.NewReader(`{"turbo":1}`))
	dec.DisallowUnknownFields()
	err := dec.Decode(&target)
	if err == nil {
		t.Fatal("DisallowUnknownFields accepted an unknown field")
	}
	if !strings.HasPrefix(err.Error(), unknownFieldPrefix) {
		t.Fatalf("encoding/json changed its unknown-field wording to %q; unknownFieldPrefix is now wrong", err)
	}
	got, ok := unknownField(err)
	if !ok || got != "turbo" {
		t.Fatalf("unknownField(%q) = %q, %v; want \"turbo\", true", err, got, ok)
	}
}

// TestParseRequestRefusals is the refusal table: every way a client can get a
// request wrong, and the exact code it must come back as. The unknown-field
// cases are the 🎯T24 ones — each of those would be a silent drop if
// DisallowUnknownFields were removed.
func TestParseRequestRefusals(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		code  ErrorCode
		field string
	}{
		{"absent version", `{"type":"status"}`, CodeUnsupportedVersion, "v"},
		{"future version", `{"v":2,"type":"status"}`, CodeUnsupportedVersion, "v"},
		{"not json", `{`, CodeMalformed, ""},
		{"trailing data", `{"v":1,"type":"status"} {"v":1,"type":"status"}`, CodeMalformed, ""},
		{"unknown envelope field", `{"v":1,"type":"status","urgent":true}`, CodeUnknownField, "urgent"},
		{"unknown spawn field", `{"v":1,"type":"spawn","body":{"mode":"session","workdir":"/w","turbo":true}}`, CodeUnknownField, "turbo"},
		{"unknown status field", `{"v":1,"type":"status","body":{"only":"mine"}}`, CodeUnknownField, "only"},
		{"unknown tail field", `{"v":1,"type":"tail","body":{"since":"x"}}`, CodeUnknownField, "since"},
		{"unknown type", `{"v":1,"type":"teleport"}`, CodeUnknownType, "type"},
		{"response on request side", `{"v":1,"type":"spawned"}`, CodeNotARequest, "type"},
		{"tailing on request side", `{"v":1,"type":"tailing"}`, CodeNotARequest, "type"},
		{"spawn without mode", `{"v":1,"type":"spawn","body":{"workdir":"/w"}}`, CodeMissingField, "mode"},
		{"spawn without workdir", `{"v":1,"type":"spawn","body":{"mode":"session"}}`, CodeMissingField, "workdir"},
		{"spawn blank workdir", `{"v":1,"type":"spawn","body":{"mode":"session","workdir":"   "}}`, CodeMissingField, "workdir"},
		{"spawn bad mode", `{"v":1,"type":"spawn","body":{"mode":"repl","workdir":"/w"}}`, CodeUnsupportedValue, "mode"},
		{"spawn bad provider", `{"v":1,"type":"spawn","body":{"provider":"gpt","mode":"session","workdir":"/w"}}`, CodeUnsupportedValue, "provider"},
		{"spawn bad intent", `{"v":1,"type":"spawn","body":{"mode":"session","intent":"urgent","workdir":"/w"}}`, CodeUnsupportedValue, "intent"},
		{"release without session", `{"v":1,"type":"release","body":{"disposition":"stop"}}`, CodeMissingField, "session_id"},
		{"release without disposition", `{"v":1,"type":"release","body":{"session_id":"s"}}`, CodeMissingField, "disposition"},
		{"release bad disposition", `{"v":1,"type":"release","body":{"session_id":"s","disposition":"burn"}}`, CodeUnsupportedValue, "disposition"},
		{"send without name", `{"v":1,"type":"send","body":{"text":"hi"}}`, CodeMissingField, "name"},
		{"send bad mode", `{"v":1,"type":"send","body":{"name":"s","text":"hi","mode":"nudge"}}`, CodeUnsupportedValue, "mode"},
		{"send unknown field", `{"v":1,"type":"send","body":{"name":"s","text":"hi","steer":true}}`, CodeUnknownField, "steer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseRequest([]byte(tc.line))
			if err == nil {
				t.Fatalf("accepted %s; got %+v", tc.line, req)
			}
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("error is %T, not *ProtocolError: %v", err, err)
			}
			if pe.Code != tc.code {
				t.Errorf("code = %q, want %q (%v)", pe.Code, tc.code, err)
			}
			if tc.field != "" && pe.Field != tc.field {
				t.Errorf("field = %q, want %q", pe.Field, tc.field)
			}
		})
	}
}

// TestParseResponseRefusesWrongDirection is the mirror: a client must not be
// able to mistake an echoed request for an answer.
func TestParseResponseRefusesWrongDirection(t *testing.T) {
	for _, line := range []string{
		`{"v":1,"type":"spawn","body":{"mode":"session","workdir":"/w"}}`,
		`{"v":1,"type":"release","body":{"session_id":"s","disposition":"stop"}}`,
		`{"v":1,"type":"status"}`,
		`{"v":1,"type":"tail"}`,
	} {
		resp, err := ParseResponse([]byte(line))
		if err == nil {
			t.Fatalf("accepted request %s as a response: %+v", line, resp)
		}
		var pe *ProtocolError
		if !errors.As(err, &pe) || pe.Code != CodeNotAResponse {
			t.Errorf("%s: got %v, want %s", line, err, CodeNotAResponse)
		}
	}
}

// TestUnsupportedVersionAdvertisesWhatIsSupported checks the renegotiation hint
// survives the trip through the error body, so a newer client can adapt without
// a hardcoded table.
func TestUnsupportedVersionAdvertisesWhatIsSupported(t *testing.T) {
	_, err := ParseRequest([]byte(`{"v":99,"type":"status","id":"x"}`))
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProtocolError, got %v", err)
	}
	if !reflect.DeepEqual(pe.SupportedVersions, []int{Version}) {
		t.Errorf("SupportedVersions = %v, want %v", pe.SupportedVersions, []int{Version})
	}
	if pe.ID != "x" {
		t.Errorf("ID = %q, want %q: a refusal must be correlatable with its request", pe.ID, "x")
	}
	round := pe.Wire().Err()
	if round.Code != pe.Code || !reflect.DeepEqual(round.SupportedVersions, pe.SupportedVersions) {
		t.Errorf("ProtocolError did not survive the wire round trip: %+v vs %+v", round, pe)
	}
}

// TestSpawnRequestValidateNormalises pins the two defaults, so "empty means
// claude/auto" is a checked promise rather than a comment.
func TestSpawnRequestValidateNormalises(t *testing.T) {
	req := &SpawnRequest{Mode: ModeSession, WorkDir: "/w"}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if req.Provider != ProviderClaude {
		t.Errorf("Provider = %q, want %q", req.Provider, ProviderClaude)
	}
	if req.Intent != IntentAuto {
		t.Errorf("Intent = %q, want %q", req.Intent, IntentAuto)
	}
}

// TestSendRequestValidateNormalises pins "absent mode means submit" (🎯T72.3)
// so a pre-🎯T72 client keeps today's behaviour by contract, not by accident,
// and the mode reaches the daemon already decided.
func TestSendRequestValidateNormalises(t *testing.T) {
	req := &SendRequest{Name: "s", Text: "hi"}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if req.Mode != SendModeSubmit {
		t.Errorf("Mode = %q, want %q", req.Mode, SendModeSubmit)
	}
	for _, m := range SendModes() {
		req := &SendRequest{Name: "s", Text: "hi", Mode: m}
		if err := req.Validate(); err != nil {
			t.Errorf("mode %q refused: %v", m, err)
		}
		if req.Mode != m {
			t.Errorf("mode %q rewritten to %q", m, req.Mode)
		}
	}
}

// TestEverySendModeHasAVector stops a mode being added to SendModes without a
// pinned request and a pinned sent response, the same way message types are
// held to a vector.
func TestEverySendModeHasAVector(t *testing.T) {
	requested, answered := map[SendMode]bool{}, map[SendMode]bool{}
	for _, v := range requestVectors {
		if v.msg.Type == TypeSend && v.msg.Send.Mode != "" {
			requested[v.msg.Send.Mode] = true
		}
	}
	for _, r := range responseVectors {
		if r.Type == TypeSent && r.Sent.Mode != "" {
			answered[r.Sent.Mode] = true
		}
	}
	for _, m := range SendModes() {
		if !requested[m] {
			t.Errorf("send mode %q has no request vector", m)
		}
		if !answered[m] {
			t.Errorf("send mode %q has no sent-response vector", m)
		}
	}
	if len(requested) != len(SendModes()) || len(answered) != len(SendModes()) {
		t.Errorf("vectors name a mode outside SendModes: requests %v, responses %v", requested, answered)
	}
}

// TestEncodeRejectsUnknownType keeps the encoder from inventing a message the
// parser would refuse.
func TestEncodeRejectsUnknownType(t *testing.T) {
	if _, err := (&Request{Type: "teleport"}).Encode(); err == nil {
		t.Error("Request.Encode accepted an unknown type")
	}
	if _, err := (&Response{Type: "teleport"}).Encode(); err == nil {
		t.Error("Response.Encode accepted an unknown type")
	}
}
