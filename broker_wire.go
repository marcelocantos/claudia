// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
)

// Wire codecs for the payloads the broker carries opaquely (🎯T2.10). The
// broker package cannot import this one, so the schema of an Event, a
// TaskEvent, a TaskConfig or a grant definition on the socket is decided
// here. Each mirror struct is checked against its source type by
// TestBrokerWireMirrorsAreComplete: a field added to Event without a wire
// slot fails the build rather than vanishing on the socket, which is the
// same rule 🎯T24 applies to Config.
//
// Event and TaskEvent deliberately carry `json:"-"` on most fields in their
// public form (consumers marshal Raw themselves), so the wire form is a
// separate struct rather than a change to the public JSON shape.

// eventWire is Event, field for field, with wire tags.
type eventWire struct {
	Type          string   `json:"type"`
	SessionID     string   `json:"session_id,omitempty"`
	TurnID        string   `json:"turn_id,omitempty"`
	MessageID     string   `json:"message_id,omitempty"`
	RecordID      string   `json:"record_id,omitempty"`
	Raw           []byte   `json:"raw,omitempty"`
	Text          string   `json:"text,omitempty"`
	StopReason    string   `json:"stop_reason,omitempty"`
	Usage         Usage    `json:"usage,omitzero"`
	ProgressType  string   `json:"progress_type,omitempty"`
	PreviewUpdate string   `json:"preview_update,omitempty"`
	ToolCallID    string   `json:"tool_call_id,omitempty"`
	ToolTitle     string   `json:"tool_title,omitempty"`
	ToolStatus    string   `json:"tool_status,omitempty"`
	Model         string   `json:"model,omitempty"`
	IsError       bool     `json:"is_error,omitempty"`
	FromProvider  Provider `json:"from_provider,omitempty"`
	ToProvider    Provider `json:"to_provider,omitempty"`
	FromModel     string   `json:"from_model,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	WarningCodes  []string `json:"warning_codes,omitempty"`
	StuckClass    string   `json:"stuck_class,omitempty"`
	Truncated     bool     `json:"truncated,omitempty"`
}

// EncodeEventWire is an Event in its daemon-protocol form (agent_event).
//
// A payload too large for one broker frame is bounded first (🎯T73): the
// event is relayed with its oversized strings elided and Truncated set,
// because the alternative is a line the wire refuses, and the wire is shared
// with every other request that consumer has in flight.
//
// The budget bounding aims for sits below the size the wire actually refuses,
// so the two are checked separately: a payload that is over budget but still
// frames is relayed whole rather than thrown away for the slack's sake.
func EncodeEventWire(ev Event) (json.RawMessage, error) {
	raw, err := json.Marshal(eventWire(ev))
	if err != nil || len(raw) <= maxWirePayloadBytes {
		return raw, err
	}
	bounded := boundEventForWire(ev)
	out, err := json.Marshal(eventWire(bounded))
	if err != nil || len(out) <= maxWireFrameBytes {
		return out, err
	}
	// Nothing in the payload was long enough to elide and it still will not
	// frame, so the size is in the shape. The choice left is between an event
	// without its payload and a frame the wire refuses — and refusing the
	// frame loses the event anyway, along with nothing else only because the
	// reader now survives it.
	bounded.Raw, bounded.Truncated = nil, true
	return json.Marshal(eventWire(bounded))
}

// DecodeEventWire reverses [EncodeEventWire].
func DecodeEventWire(raw json.RawMessage) (Event, error) {
	var w eventWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Event{}, fmt.Errorf("broker event: %w", err)
	}
	return Event(w), nil
}

// taskEventWire is TaskEvent with wire tags.
type taskEventWire struct {
	Type       TaskEventType `json:"type"`
	Content    string        `json:"content,omitempty"`
	ToolName   string        `json:"tool_name,omitempty"`
	ToolInput  string        `json:"tool_input,omitempty"`
	ToolID     string        `json:"tool_id,omitempty"`
	SessionID  string        `json:"session_id,omitempty"`
	DurationMs float64       `json:"duration_ms,omitempty"`
	CostUSD    float64       `json:"cost_usd,omitempty"`
	Usage      Usage         `json:"usage,omitzero"`
	IsError    bool          `json:"is_error,omitempty"`
	ErrorMsg   string        `json:"error_msg,omitempty"`
	Model      string        `json:"model,omitempty"`
	Truncated  bool          `json:"truncated,omitempty"`
}

// EncodeTaskEventWire is a TaskEvent in its daemon-protocol form (task_event).
// Oversized payloads are bounded exactly as in [EncodeEventWire].
func EncodeTaskEventWire(ev TaskEvent) (json.RawMessage, error) {
	raw, err := json.Marshal(taskEventWire(ev))
	if err != nil || len(raw) <= maxWirePayloadBytes {
		return raw, err
	}
	bounded := boundTaskEventForWire(ev)
	out, err := json.Marshal(taskEventWire(bounded))
	if err != nil || len(out) <= maxWireFrameBytes {
		return out, err
	}
	bounded.ToolInput, bounded.Truncated = "", true
	return json.Marshal(taskEventWire(bounded))
}

// DecodeTaskEventWire reverses [EncodeTaskEventWire].
func DecodeTaskEventWire(raw json.RawMessage) (TaskEvent, error) {
	var w taskEventWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return TaskEvent{}, fmt.Errorf("broker task event: %w", err)
	}
	return TaskEvent(w), nil
}

// taskConfigWire is TaskConfig with wire tags.
type taskConfigWire struct {
	ID             string   `json:"id,omitempty"`
	Name           string   `json:"name,omitempty"`
	Provider       Provider `json:"provider,omitempty"`
	WorkDir        string   `json:"workdir,omitempty"`
	Model          string   `json:"model,omitempty"`
	SandboxMode    string   `json:"sandbox_mode,omitempty"`
	ApprovalPolicy string   `json:"approval_policy,omitempty"`
	DisallowTools  []string `json:"disallow_tools,omitempty"`
	ClaudeID       string   `json:"claude_id,omitempty"`
	LastResult     string   `json:"last_result,omitempty"`
}

// EncodeTaskConfigWire is a TaskConfig in its daemon-protocol form (task_run).
func EncodeTaskConfigWire(cfg TaskConfig) (json.RawMessage, error) {
	return json.Marshal(taskConfigWire(cfg))
}

// DecodeTaskConfigWire reverses [EncodeTaskConfigWire].
func DecodeTaskConfigWire(raw json.RawMessage) (TaskConfig, error) {
	var w taskConfigWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return TaskConfig{}, fmt.Errorf("broker task config: %w", err)
	}
	return TaskConfig(w), nil
}

// GrantDefinition is what a grant carries: the persistent AgentDef, which is
// the whole Session Config except three fields. Config.GoalCompleteCheck is
// a func: it stays on the consumer's handle and the daemon calls back to it
// (configByCallback). PoolPolicy / PoolCap belong to Acquire, which is not
// brokered. RequireResume is the consumer Registry's verdict for this launch.
type GrantDefinition struct {
	AgentDef
	RequireResume bool `json:"require_resume,omitempty"`
}

// configNotOnGrantWire lists the Config fields a grant deliberately does not
// carry, with the reason. TestBrokerWireMirrorsAreComplete refuses any other
// omission.
var configNotOnGrantWire = map[string]string{
	"PoolPolicy": "Acquire pool policy; carried as grant.pool on an acquire, not on the definition",
	"PoolCap":    "Acquire pool cap; carried as grant.pool on an acquire, not on the definition",
	// The wait it bounds runs in the consumer's process, on its own
	// handle; the daemon's seat has its own (🎯T96). Sending it would
	// claim a control over the seat that the field does not have.
	"TurnSilenceBound": "bounds WaitForResponse on this handle, not the seat the daemon holds",
}

// configByCallback lists the Config fields a grant cannot carry as data but
// the daemon honours by calling back to the owning connection, with the
// message that does it. TestBrokerWireMirrorsAreComplete accepts these.
var configByCallback = map[string]string{
	"GoalCompleteCheck": "goal_check / goal_verdict: the daemon's Goal loop asks the owner (🎯T75.9)",
}

// configToGrantDef builds the grant from cfg, over the consumer's own
// definition when it has one (labels the Config cannot carry).
func configToGrantDef(name string, cfg Config, base *AgentDef) GrantDefinition {
	var labels AgentDef
	if base != nil {
		labels = cloneAgentDef(*base)
	}
	return GrantDefinition{
		AgentDef: AgentDef{
			Name:                 name,
			Parent:               labels.Parent,
			Purpose:              labels.Purpose,
			Role:                 labels.Role,
			Description:          labels.Description,
			TargetID:             labels.TargetID,
			Materialized:         labels.Materialized,
			WorkDir:              cfg.WorkDir,
			SessionID:            cfg.SessionID,
			Provider:             cfg.Provider,
			Model:                cfg.Model,
			DisallowTools:        cfg.DisallowTools,
			ConnectURL:           cfg.ConnectURL,
			ConnectPID:           cfg.ConnectPID,
			GrokConnect:          cfg.GrokConnect,
			SandboxMode:          cfg.SandboxMode,
			SandboxWritableRoots: cfg.SandboxWritableRoots,
			SandboxNetworkAccess: cfg.SandboxNetworkAccess,
			SandboxGitWrite:      cfg.SandboxGitWrite,
			Goal:                 cfg.Goal,
			MCPServers:           cfg.MCPServers,
			MCPExclusive:         cfg.MCPExclusive,
			PermissionMode:       cfg.PermissionMode,
			MCPConfig:            cfg.MCPConfig,
			ExtraArgs:            cfg.ExtraArgs,
			TermLogPath:          cfg.TermLogPath,
		},
		RequireResume: cfg.RequireResume,
	}
}

// Config is the Session Config a grant describes: what the daemon starts
// the seat with.
func (def GrantDefinition) Config() Config {
	cfg := registryConfig(&def.AgentDef, def.RequireResume)
	cfg.Name = def.Name
	return cfg
}

// EncodeGrantDefinition is a grant in its daemon-protocol form (grant.def).
func EncodeGrantDefinition(def GrantDefinition) (json.RawMessage, error) {
	return json.Marshal(def)
}

// DecodeGrantDefinition reverses [EncodeGrantDefinition].
func DecodeGrantDefinition(raw json.RawMessage) (GrantDefinition, error) {
	var w GrantDefinition
	if err := json.Unmarshal(raw, &w); err != nil {
		return GrantDefinition{}, fmt.Errorf("broker grant def: %w", err)
	}
	return w, nil
}

// predicatesWire is the portable half of ModelPredicates. Usage, Cache and
// Now are caller-local inputs; the daemon answers from its own snapshot and
// clock. Thresholds travel because they change the pick.
type predicatesWire struct {
	Mode             Capability      `json:"mode,omitempty"`
	Purpose          ModelPurpose    `json:"purpose,omitempty"`
	Skill            ModelPurpose    `json:"skill,omitempty"`
	Quality          ModelQuality    `json:"quality,omitempty"`
	Model            string          `json:"model,omitempty"`
	Effort           ModelEffort     `json:"effort,omitempty"`
	PreferPlan       bool            `json:"prefer_plan,omitempty"`
	PreferProvider   Provider        `json:"prefer_provider,omitempty"`
	ExcludeProviders []Provider      `json:"exclude_providers,omitempty"`
	RequireUsage     bool            `json:"require_usage,omitempty"`
	Thresholds       *PlanThresholds `json:"thresholds,omitempty"`
}

// predicatesNotOnWire lists the ModelPredicates fields the daemon supplies
// itself.
var predicatesNotOnWire = map[string]string{
	"Usage": "the daemon answers from its own snapshot",
	"Cache": "the daemon is the cache",
	"Intel": "the daemon reads StateDir/model-intel",
	"Now":   "the daemon reads its own clock",
}

// EncodePredicatesWire is the portable half of ModelPredicates in its
// daemon-protocol form (resolve).
func EncodePredicatesWire(p ModelPredicates) (json.RawMessage, error) {
	p = normalizePredicates(p)
	return json.Marshal(predicatesWire{
		Mode: p.Mode, Purpose: p.Purpose, Skill: p.Skill, Quality: p.Quality,
		Model: p.Model, Effort: p.Effort, PreferPlan: p.PreferPlan,
		PreferProvider: p.PreferProvider, ExcludeProviders: p.ExcludeProviders,
		RequireUsage: p.RequireUsage, Thresholds: p.Thresholds,
	})
}

// DecodePredicatesWire reverses [EncodePredicatesWire]; the fields the
// daemon supplies itself are zero.
func DecodePredicatesWire(raw json.RawMessage) (ModelPredicates, error) {
	var w predicatesWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return ModelPredicates{}, fmt.Errorf("broker predicates: %w", err)
	}
	purpose := w.Purpose
	if purpose == "" {
		purpose = w.Skill
	}
	return ModelPredicates{
		Mode: w.Mode, Purpose: purpose, Skill: w.Skill, Quality: w.Quality,
		Model: w.Model, Effort: w.Effort, PreferPlan: w.PreferPlan,
		PreferProvider: w.PreferProvider, ExcludeProviders: w.ExcludeProviders,
		RequireUsage: w.RequireUsage, Thresholds: w.Thresholds,
	}, nil
}

// pickWire is ModelPick with wire tags.
type pickWire struct {
	Provider Provider     `json:"provider"`
	Model    string       `json:"model"`
	Quality  ModelQuality `json:"quality,omitempty"`
	Purpose  ModelPurpose `json:"purpose,omitempty"`
	Effort   ModelEffort  `json:"effort,omitempty"`
	Access   ModelAccess  `json:"access,omitempty"`
	Band     PlanBand     `json:"band,omitempty"`
	CostUSD  float64      `json:"cost_usd,omitempty"`
	Reason   string       `json:"reason,omitempty"`
	Author   string       `json:"author,omitempty"`
}

// EncodePickWire is a ModelPick in its daemon-protocol form (resolved).
func EncodePickWire(p ModelPick) (json.RawMessage, error) { return json.Marshal(pickWire(p)) }

// DecodePickWire reverses [EncodePickWire].
func DecodePickWire(raw json.RawMessage) (ModelPick, error) {
	var w pickWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return ModelPick{}, fmt.Errorf("broker pick: %w", err)
	}
	return ModelPick(w), nil
}

// migrateArgsWire mirrors MigrateArgs on the wire (broker.MigrateRequest
// carries the same fields by name; this keeps the census honest).
type migrateArgsWire struct {
	Provider Provider `json:"provider"`
	Model    string   `json:"model,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Force    bool     `json:"force,omitempty"`
}
