// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// 🎯T24 — every caller-supplied request field on every provider path is
// either consumed or refused with a typed *CapabilityError. Reflection
// enumerates the fields; this table is only the disposition. Adding a
// field to TaskConfig or Config without deciding what each path does
// with it fails TestProviderPathsHonourOrRefuseEveryRequestField.

type fieldFate string

const (
	fateConsumed fieldFate = "consumed"
	fateRefused  fieldFate = "refused"
	fateLocal    fieldFate = "local"
	fateIgnored  fieldFate = "ignored"
)

type fieldDecl struct {
	fate   fieldFate
	reason string
}

func (d fieldDecl) ok() bool {
	switch d.fate {
	case fateConsumed:
		return true
	case fateRefused, fateLocal, fateIgnored:
		return d.reason != ""
	default:
		return false
	}
}

var taskProviders = []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderBedrock, ProviderOllama}
var sessionProviders = []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderBedrock, ProviderOllama}

// taskFieldFates is what each Task path does with each TaskConfig field.
// A missing entry is a silent drop waiting to happen.
var taskFieldFates = map[Provider]map[string]fieldDecl{
	ProviderClaude: {
		"ID":             {fateLocal, "caller-assigned id; never sent to the process"},
		"Name":           {fateLocal, "human label; never sent to the process"},
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateConsumed, ""},
		"Model":          {fateConsumed, ""},
		"SandboxMode":    {fateRefused, "claudeTaskArgs emits no sandbox flag"},
		"ApprovalPolicy": {fateRefused, "claudeTaskArgs emits no approval flag"},
		"DisallowTools":  {fateConsumed, ""},
		"ClaudeID":       {fateConsumed, ""},
		"LastResult":     {fateLocal, "rehydration seed for Task.LastResult; never sent to the process"},
	},
	ProviderCodex: {
		"ID":             {fateLocal, "caller-assigned id; never sent to the process"},
		"Name":           {fateLocal, "human label; never sent to the process"},
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateConsumed, ""},
		"Model":          {fateConsumed, ""},
		"SandboxMode":    {fateConsumed, ""},
		"ApprovalPolicy": {fateConsumed, ""},
		"DisallowTools":  {fateRefused, "codex exec has no per-tool disallow flag"},
		"ClaudeID":       {fateConsumed, ""},
		"LastResult":     {fateLocal, "rehydration seed for Task.LastResult; never sent to the process"},
	},
	ProviderGrok: {
		"ID":             {fateLocal, "caller-assigned id; never sent to the process"},
		"Name":           {fateLocal, "human label; never sent to the process"},
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateConsumed, ""},
		"Model":          {fateConsumed, ""},
		"SandboxMode":    {fateRefused, "grokTaskArgs emits no sandbox flag"},
		"ApprovalPolicy": {fateRefused, "grokTaskArgs emits no approval flag"},
		"DisallowTools":  {fateRefused, "DisallowTools is not translated onto --deny / --disallowed-tools"},
		"ClaudeID":       {fateConsumed, ""},
		"LastResult":     {fateLocal, "rehydration seed for Task.LastResult; never sent to the process"},
	},
	ProviderBedrock: {
		"ID":             {fateLocal, "caller-assigned id; never sent to the API"},
		"Name":           {fateLocal, "human label; never sent to the API"},
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateIgnored, "ConverseStream is an HTTP call; there is no process directory"},
		"Model":          {fateConsumed, ""},
		"SandboxMode":    {fateRefused, "ConverseStream has no sandbox setting"},
		"ApprovalPolicy": {fateRefused, "ConverseStream has no approval setting"},
		"DisallowTools":  {fateRefused, "claudia sends no Bedrock toolConfig"},
		"ClaudeID":       {fateRefused, "ConverseStream is stateless; a session id would start cold"},
		"LastResult":     {fateLocal, "rehydration seed for Task.LastResult; never sent to the API"},
	},
	ProviderOllama: {
		"ID":             {fateLocal, "caller-assigned id; never sent to the API"},
		"Name":           {fateLocal, "human label; never sent to the API"},
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateIgnored, "/api/generate is an HTTP call; there is no process directory"},
		"Model":          {fateConsumed, ""},
		"SandboxMode":    {fateRefused, "/api/generate has no sandbox setting"},
		"ApprovalPolicy": {fateRefused, "/api/generate has no approval setting"},
		"DisallowTools":  {fateRefused, "/api/generate runs no tools"},
		"ClaudeID":       {fateRefused, "/api/generate carries no conversation state"},
		"LastResult":     {fateLocal, "rehydration seed for Task.LastResult; never sent to the API"},
	},
}

// sessionFieldFates is what each Session path does with each Config field.
// Paths whose CapabilitySession claim is not supported are omitted — the
// audit asserts the whole Start fails closed instead of per-field.
var sessionFieldFates = map[Provider]map[string]fieldDecl{
	ProviderClaude: {
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateConsumed, ""},
		"SessionID":      {fateConsumed, ""},
		"RequireResume":  {fateConsumed, ""},
		"Model":          {fateConsumed, ""},
		"PermissionMode": {fateConsumed, ""},
		"SandboxMode":    {fateRefused, "SandboxMode is a Codex app-server field"},
		"MCPConfig":      {fateConsumed, ""},
		"DisallowTools":  {fateConsumed, ""},
		"ExtraArgs":      {fateConsumed, ""},
		"TermLogPath":    {fateLocal, "host-side PTY log path; not a provider argument"},
		"PoolPolicy":     {fateLocal, "Acquire policy; Start does not consult it"},
		"PoolCap":        {fateLocal, "Acquire cap; Start does not consult it"},
		"GrokConnect":    {fateIgnored, "Grok serve-mode switch; Claude Session is tmux"},
		"ConnectURL":     {fateIgnored, "Grok reattach URL; Claude Session has no serve endpoint"},
		"ConnectPID":     {fateIgnored, "Grok serve PID; Claude Session has no serve process"},
	},
	ProviderGrok: {
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateConsumed, ""},
		"SessionID":      {fateConsumed, ""},
		"RequireResume":  {fateConsumed, ""},
		"Model":          {fateConsumed, ""},
		"PermissionMode": {fateRefused, "Grok Session hardcodes ACP always-approve/yoloMode"},
		"SandboxMode":    {fateRefused, "SandboxMode is a Codex app-server field"},
		"MCPConfig":      {fateConsumed, ""},
		"DisallowTools":  {fateRefused, "Config.DisallowTools never reaches the ACP client"},
		"ExtraArgs":      {fateRefused, "fixed grok agent argv; caller ExtraArgs have nowhere to go"},
		"TermLogPath":    {fateLocal, "host-side log path; Grok ACP is not a PTY"},
		"PoolPolicy":     {fateLocal, "Acquire policy; Start does not consult it"},
		"PoolCap":        {fateLocal, "Acquire cap; Start does not consult it"},
		"GrokConnect":    {fateConsumed, ""},
		"ConnectURL":     {fateConsumed, ""},
		"ConnectPID":     {fateLocal, "recorded for Adopt/Alive; Start itself keys off ConnectURL / GrokConnect"},
	},
	ProviderCodex: {
		"Provider":       {fateLocal, "selects this path"},
		"WorkDir":        {fateConsumed, ""},
		"SessionID":      {fateConsumed, ""},
		"RequireResume":  {fateConsumed, ""},
		"Model":          {fateConsumed, ""},
		"PermissionMode": {fateRefused, "Codex sandbox/approval are not Claude PermissionMode"},
		"SandboxMode":    {fateConsumed, ""},
		"MCPConfig":      {fateIgnored, "app-server thread/start has no MCPConfig field"},
		"DisallowTools":  {fateRefused, "codex exec / app-server have no per-tool disallow flag"},
		"ExtraArgs":      {fateRefused, "typed app-server fields only"},
		"TermLogPath":    {fateLocal, "host-side log path; app-server is not a PTY"},
		"PoolPolicy":     {fateLocal, "Acquire policy; Start does not consult it"},
		"PoolCap":        {fateLocal, "Acquire cap; Start does not consult it"},
		"GrokConnect":    {fateIgnored, "Grok serve-mode switch; Codex Session is app-server stdio"},
		"ConnectURL":     {fateIgnored, "Grok reattach URL"},
		"ConnectPID":     {fateIgnored, "Grok serve PID"},
	},
}

func exportedFields(t reflect.Type) []string {
	var names []string
	for f := range t.Fields() {
		if f.IsExported() {
			names = append(names, f.Name)
		}
	}
	slices.Sort(names)
	return names
}

func taskPrecheck(provider Provider, req taskRunRequest) error {
	switch provider {
	case ProviderClaude:
		return claudeTaskPrecheck(req)
	case ProviderCodex:
		return codexTaskPrecheck(req)
	case ProviderGrok:
		return grokTaskPrecheck(req)
	case ProviderBedrock:
		return bedrockTaskPrecheck(req)
	case ProviderOllama:
		return ollamaTaskPrecheck(req)
	default:
		return unsupportedCapability(provider, CapabilityTask, "no task precheck")
	}
}

func setTaskField(req *taskRunRequest, field string) {
	switch field {
	case "WorkDir":
		req.WorkDir = "/tmp/claudia-t24-workdir"
	case "Model":
		req.Model = "t24-model"
	case "SandboxMode":
		req.SandboxMode = "read-only"
	case "ApprovalPolicy":
		req.ApprovalPolicy = "never"
	case "DisallowTools":
		req.DisallowTools = []string{"WebFetch"}
	case "ClaudeID":
		req.SessionID = "t24-session"
	}
}

func taskMaterialises(provider Provider, field string) bool {
	req := taskRunRequest{Prompt: "t24-prompt"}
	setTaskField(&req, field)
	switch provider {
	case ProviderClaude:
		return argvHolds(claudeTaskArgs(req), taskNeedle(field, req))
	case ProviderCodex:
		return argvHolds(codexTaskArgs(req), taskNeedle(field, req))
	case ProviderGrok:
		return argvHolds(grokTaskArgs(req), taskNeedle(field, req))
	case ProviderBedrock:
		in := buildBedrockConverseInput(req.Model, req.Prompt)
		switch field {
		case "Model":
			return in.ModelId != nil && *in.ModelId == req.Model
		default:
			return false
		}
	case ProviderOllama:
		return field == "Model" && req.Model != ""
	default:
		return false
	}
}

func taskNeedle(field string, req taskRunRequest) string {
	switch field {
	case "WorkDir":
		return req.WorkDir
	case "Model":
		return req.Model
	case "SandboxMode":
		return req.SandboxMode
	case "ApprovalPolicy":
		return req.ApprovalPolicy
	case "DisallowTools":
		return "WebFetch"
	case "ClaudeID":
		return req.SessionID
	default:
		return ""
	}
}

func argvHolds(argv []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, a := range argv {
		if a == needle || strings.Contains(a, needle) {
			return true
		}
	}
	return false
}

func isCapErr(err error) bool {
	var capErr *CapabilityError
	return errors.As(err, &capErr)
}

func auditTaskFates(fates map[Provider]map[string]fieldDecl) []string {
	var issues []string
	fields := exportedFields(reflect.TypeFor[TaskConfig]())
	for _, provider := range taskProviders {
		decls, ok := fates[provider]
		if !ok {
			issues = append(issues, string(provider)+": no Task dispositions")
			continue
		}
		for _, field := range fields {
			d, ok := decls[field]
			if !ok {
				issues = append(issues, string(provider)+" Task "+field+": no disposition — silent drop")
				continue
			}
			if !d.ok() {
				issues = append(issues, string(provider)+" Task "+field+": "+string(d.fate)+" needs a reason")
				continue
			}
			req := taskRunRequest{Prompt: "t24-prompt"}
			setTaskField(&req, field)
			err := taskPrecheck(provider, req)
			switch d.fate {
			case fateRefused:
				if !isCapErr(err) {
					issues = append(issues, string(provider)+" Task "+field+": declared refused, precheck = "+errString(err))
				}
			case fateConsumed:
				if err != nil {
					issues = append(issues, string(provider)+" Task "+field+": declared consumed, precheck refused: "+err.Error())
					continue
				}
				if field == "WorkDir" && (provider == ProviderClaude) {
					// Honouring is cmd.Dir, not argv. Codex/Grok put the
					// directory on the command line; Claude does not.
					continue
				}
				if !taskMaterialises(provider, field) {
					issues = append(issues, string(provider)+" Task "+field+": declared consumed, not in the materialised request")
				}
			case fateIgnored:
				if err != nil {
					issues = append(issues, string(provider)+" Task "+field+": declared ignored, precheck refused: "+err.Error())
				}
				if taskMaterialises(provider, field) {
					issues = append(issues, string(provider)+" Task "+field+": declared ignored, but the materialised request carries it")
				}
			case fateLocal:
				// Host bookkeeping — the provider path must not see it.
			}
		}
		for field := range decls {
			if !slices.Contains(fields, field) {
				issues = append(issues, string(provider)+" Task dispositions name "+field+", which TaskConfig no longer has")
			}
		}
	}
	return issues
}

func sessionStartRequest(field string) agentStartRequest {
	cfg := Config{PermissionMode: "bypassPermissions"}
	req := agentStartRequest{
		Config:          cfg,
		WorkDir:         "/tmp/claudia-t24-session",
		SessionID:       "t24-sid",
		DisallowedTools: "Agent",
	}
	switch field {
	case "WorkDir":
		req.WorkDir = "/tmp/claudia-t24-workdir"
	case "SessionID":
		req.SessionID = "t24-unique-sid"
		req.Config.SessionID = "t24-unique-sid"
	case "RequireResume":
		req.Config.RequireResume = true
		req.Resuming = true
	case "Model":
		req.Config.Model = "t24-model"
	case "PermissionMode":
		req.Config.PermissionMode = "plan"
	case "SandboxMode":
		req.Config.SandboxMode = "workspace-write"
	case "MCPConfig":
		req.Config.MCPConfig = "/tmp/claudia-t24-mcp.json"
	case "DisallowTools":
		req.Config.DisallowTools = []string{"WebFetch"}
		req.DisallowedTools = disallowedToolList([]string{"WebFetch"})
	case "ExtraArgs":
		req.Config.ExtraArgs = []string{"--t24-extra"}
	case "GrokConnect":
		req.Config.GrokConnect = true
	case "ConnectURL":
		req.Config.ConnectURL = "ws://127.0.0.1:9/ws"
	}
	return req
}

func sessionMaterialises(provider Provider, field string) bool {
	req := sessionStartRequest(field)
	switch provider {
	case ProviderCodex:
		p := codexThreadStartParams(req)
		switch field {
		case "WorkDir":
			return p.CWD == req.WorkDir
		case "Model":
			return p.Model == req.Config.Model
		case "SandboxMode":
			return p.Sandbox == req.Config.SandboxMode
		case "SessionID":
			return req.SessionID != ""
		case "RequireResume":
			return req.Config.RequireResume
		default:
			return false
		}
	case ProviderClaude:
		switch field {
		case "WorkDir":
			return req.WorkDir != ""
		case "RequireResume":
			return req.Config.RequireResume
		default:
			return argvHolds(claudeAgentArgs(req), sessionNeedle(field, req))
		}
	case ProviderGrok:
		plan := planGrokSession(req)
		switch field {
		case "WorkDir":
			return plan.WorkDir == req.WorkDir
		case "SessionID":
			return plan.PreferSessionID == req.SessionID
		case "RequireResume":
			return plan.RequireResume
		case "Model":
			return argvHolds(plan.Args, req.Config.Model)
		case "MCPConfig":
			return req.Config.MCPConfig != "" // path is forwarded into acpMCPServers
		case "GrokConnect", "ConnectURL":
			return plan.Connect
		default:
			return argvHolds(plan.Args, sessionNeedle(field, req))
		}
	default:
		return false
	}
}

func sessionNeedle(field string, req agentStartRequest) string {
	switch field {
	case "SessionID":
		return req.SessionID
	case "Model":
		return req.Config.Model
	case "PermissionMode":
		return req.Config.PermissionMode
	case "MCPConfig":
		return req.Config.MCPConfig
	case "DisallowTools":
		return "WebFetch"
	case "ExtraArgs":
		return "--t24-extra"
	default:
		return ""
	}
}

func sessionPrecheck(provider Provider, req agentStartRequest) error {
	switch provider {
	case ProviderClaude:
		return claudeSessionPrecheck(req)
	case ProviderGrok:
		return grokSessionPrecheck(req)
	case ProviderCodex:
		return codexSessionPrecheck(req)
	default:
		return nil
	}
}

func auditSessionFates(fates map[Provider]map[string]fieldDecl) []string {
	var issues []string
	fields := exportedFields(reflect.TypeFor[Config]())
	for _, provider := range sessionProviders {
		if err := CheckCapability(provider, CapabilitySession); err != nil {
			if _, named := fates[provider]; named {
				issues = append(issues, string(provider)+" Session is path-closed but has per-field dispositions")
			}
			cfg := Config{Provider: provider, WorkDir: os.TempDir()}
			startErr := func() error {
				_, e := Start(cfg)
				return e
			}()
			if !isCapErr(startErr) {
				issues = append(issues, string(provider)+" Session: CapabilitySession is not supported, but Start = "+errString(startErr))
			}
			continue
		}
		decls, ok := fates[provider]
		if !ok {
			issues = append(issues, string(provider)+": no Session dispositions")
			continue
		}
		for _, field := range fields {
			d, ok := decls[field]
			if !ok {
				issues = append(issues, string(provider)+" Session "+field+": no disposition — silent drop")
				continue
			}
			if !d.ok() {
				issues = append(issues, string(provider)+" Session "+field+": "+string(d.fate)+" needs a reason")
				continue
			}
			req := sessionStartRequest(field)
			err := sessionPrecheck(provider, req)
			switch d.fate {
			case fateRefused:
				if !isCapErr(err) {
					issues = append(issues, string(provider)+" Session "+field+": declared refused, precheck = "+errString(err))
				}
			case fateConsumed:
				if err != nil {
					issues = append(issues, string(provider)+" Session "+field+": declared consumed, precheck refused: "+err.Error())
					continue
				}
				if !sessionMaterialises(provider, field) {
					issues = append(issues, string(provider)+" Session "+field+": declared consumed, not in the materialised request")
				}
			case fateIgnored:
				if err != nil {
					issues = append(issues, string(provider)+" Session "+field+": declared ignored, precheck refused: "+err.Error())
				}
				if sessionMaterialises(provider, field) {
					issues = append(issues, string(provider)+" Session "+field+": declared ignored, but the materialised request carries it")
				}
			case fateLocal:
			}
		}
		for field := range decls {
			if !slices.Contains(fields, field) {
				issues = append(issues, string(provider)+" Session dispositions name "+field+", which Config no longer has")
			}
		}
	}
	return issues
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func requestFieldAudit(task map[Provider]map[string]fieldDecl, session map[Provider]map[string]fieldDecl) []string {
	return append(auditTaskFates(task), auditSessionFates(session)...)
}

func cloneTaskFates() map[Provider]map[string]fieldDecl {
	out := make(map[Provider]map[string]fieldDecl, len(taskFieldFates))
	for p, m := range taskFieldFates {
		cp := make(map[string]fieldDecl, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out[p] = cp
	}
	return out
}

// TestProviderPathsHonourOrRefuseEveryRequestField is the 🎯T24 census.
// Reflection over TaskConfig and Config, not a hand-maintained field list.
func TestProviderPathsHonourOrRefuseEveryRequestField(t *testing.T) {
	for _, issue := range requestFieldAudit(taskFieldFates, sessionFieldFates) {
		t.Error(issue)
	}
}

// TestDeclaringAConsumedFieldUnsupportedGoesRed is mutation (b): a field
// the path in fact honours, marked refused, must fail the audit.
func TestDeclaringAConsumedFieldUnsupportedGoesRed(t *testing.T) {
	task := cloneTaskFates()
	task[ProviderClaude]["Model"] = fieldDecl{fateRefused, "mutated declaration"}
	issues := requestFieldAudit(task, sessionFieldFates)
	if !containsIssue(issues, "claude Task Model: declared refused") {
		t.Fatalf("declaring Claude Task Model unsupported did not fail the audit: %v", issues)
	}
}

// TestReintroducingASilentDropGoesRed is mutation (a): a field the path
// cannot honour, if no longer refused, fails the audit. The live precheck
// is the production path; flipping the declaration to consumed is the
// same shape as deleting the precheck while leaving the claim in place.
func TestReintroducingASilentDropGoesRed(t *testing.T) {
	req := taskRunRequest{Prompt: "hi", DisallowTools: []string{"Bash"}}
	if err := grokTaskPrecheck(req); !isCapErr(err) {
		t.Fatalf("Grok Task DisallowTools was dropped: %v", err)
	}
	task := cloneTaskFates()
	task[ProviderGrok]["DisallowTools"] = fieldDecl{fateConsumed, ""}
	issues := requestFieldAudit(task, sessionFieldFates)
	if !containsIssue(issues, "grok Task DisallowTools") {
		t.Fatalf("declaring Grok Task DisallowTools consumed did not fail the audit: %v", issues)
	}
}

func containsIssue(issues []string, needle string) bool {
	for _, i := range issues {
		if strings.Contains(i, needle) {
			return true
		}
	}
	return false
}
