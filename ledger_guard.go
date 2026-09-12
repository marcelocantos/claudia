// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// LedgerRefuseReason is the agent-facing explanation when a mutating tool
// call on bullseye.yaml is rejected (jevons 🎯T546).
const LedgerRefuseReason = "refusing mutation of bullseye.yaml — agents must not edit that file. File, status, achieve, and query go through jevons_target_file or bullseye MCP/CLI (`bullseye commit` / `bullseye query`). 🎯T546"

// IsBullseyeYAML reports whether path is the intent ledger file, regardless
// of directory. Used to refuse agent tool mutations (jevons 🎯T546).
func IsBullseyeYAML(path string) bool {
	if path == "" {
		return false
	}
	base := filepath.Base(strings.ReplaceAll(path, "\\", "/"))
	return strings.EqualFold(base, "bullseye.yaml")
}

// ClaudeLedgerDenyRules are the Claude Code permission deny rules that
// refuse mutation of bullseye.yaml even under bypassPermissions /
// --dangerously-skip-permissions (jevons 🎯T546 / 🎯T67). Edit rules
// cover Write/Edit/MultiEdit; Bash catches shell writes that name the
// ledger. Exported next to IsBullseyeYAML so a deleted rule fails the
// hermetic oracle rather than hiding behind skip-permissions.
func ClaudeLedgerDenyRules() []string {
	return []string{
		"Edit(**/bullseye.yaml)",
		"Edit(bullseye.yaml)",
		"Bash(*bullseye.yaml*)",
	}
}

// ClaudeLedgerSettingsJSON is the --settings payload applied on every
// Claude Session and Task spawn.
func ClaudeLedgerSettingsJSON() string {
	type perms struct {
		Deny []string `json:"deny"`
	}
	type settings struct {
		Permissions perms `json:"permissions"`
	}
	b, err := json.Marshal(settings{Permissions: perms{Deny: ClaudeLedgerDenyRules()}})
	if err != nil {
		return `{"permissions":{"deny":["Edit(**/bullseye.yaml)","Edit(bullseye.yaml)","Bash(*bullseye.yaml*)"]}}`
	}
	return string(b)
}

// ClaudeToolCallDeniedByLedger reports whether a Claude Write/Edit/Bash
// (or MultiEdit) invocation is refused by ClaudeLedgerDenyRules.
func ClaudeToolCallDeniedByLedger(tool, pathOrCommand string) bool {
	for _, rule := range ClaudeLedgerDenyRules() {
		if claudeDenyRuleMatches(rule, tool, pathOrCommand) {
			return true
		}
	}
	return false
}

func claudeDenyRuleMatches(rule, tool, input string) bool {
	name, pattern, ok := parseClaudePermissionRule(rule)
	if !ok {
		return false
	}
	if strings.EqualFold(name, "Edit") {
		switch strings.ToLower(strings.TrimSpace(tool)) {
		case "edit", "write", "multiedit", "notebookedit":
			return claudePathPatternMatches(pattern, input)
		}
		return false
	}
	if strings.EqualFold(name, "Bash") && strings.EqualFold(strings.TrimSpace(tool), "Bash") {
		return claudeCommandPatternMatches(pattern, input)
	}
	return false
}

func parseClaudePermissionRule(rule string) (name, pattern string, ok bool) {
	open := strings.Index(rule, "(")
	if open <= 0 || !strings.HasSuffix(rule, ")") {
		return "", "", false
	}
	return rule[:open], rule[open+1 : len(rule)-1], true
}

func claudePathPatternMatches(pattern, path string) bool {
	if pattern == "**/bullseye.yaml" || pattern == "bullseye.yaml" {
		return IsBullseyeYAML(path)
	}
	return false
}

func claudeCommandPatternMatches(pattern, command string) bool {
	if pattern == "*bullseye.yaml*" {
		return commandMentionsLedger(command)
	}
	return false
}

func appendClaudeLedgerSettings(args []string) []string {
	return append(args, "--settings", ClaudeLedgerSettingsJSON())
}

// permissionMutatesBullseye reports whether a session/request_permission
// params blob is a mutating tool call on bullseye.yaml (Cursor StrReplace
// / Write / Edit). Read and Grep are not this refuse.
func permissionMutatesBullseye(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	info := parsePermissionTool(params)
	if !info.mutating {
		return false
	}
	for _, p := range info.paths {
		if IsBullseyeYAML(p) {
			return true
		}
	}
	return false
}

type permissionTool struct {
	mutating bool
	paths    []string
}

func parsePermissionTool(params json.RawMessage) permissionTool {
	var p struct {
		ToolCall struct {
			Title     string         `json:"title"`
			Kind      string         `json:"kind"`
			ToolName  string         `json:"toolName"`
			RawInput  map[string]any `json:"rawInput"`
			Locations []struct {
				Path string `json:"path"`
			} `json:"locations"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return permissionTool{}
	}
	tc := p.ToolCall
	name := firstNonEmpty(tc.ToolName, tc.Title, tc.Kind)
	out := permissionTool{mutating: isMutatingLedgerTool(name, tc.Kind)}
	for _, loc := range tc.Locations {
		if loc.Path != "" {
			out.paths = append(out.paths, loc.Path)
		}
	}
	for _, key := range []string{"path", "file_path", "filePath", "target_file", "targetFile"} {
		if s, ok := tc.RawInput[key].(string); ok && s != "" {
			out.paths = append(out.paths, s)
		}
	}
	for _, key := range []string{"command", "cmd", "input"} {
		if s, ok := tc.RawInput[key].(string); ok && commandMentionsLedger(s) {
			out.paths = append(out.paths, "bullseye.yaml")
		}
	}
	return out
}

func commandMentionsLedger(s string) bool {
	return strings.Contains(strings.ToLower(s), "bullseye.yaml")
}

func isMutatingLedgerTool(name, kind string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "edit", "write", "delete", "execute":
		return true
	}
	switch n {
	case "strreplace", "write", "edit", "multiedit", "notebookedit",
		"write_file", "writefile", "search_replace", "searchreplace",
		"bash", "run_terminal_command", "shell":
		return true
	}
	if strings.Contains(n, "strreplace") || strings.Contains(n, "search_replace") {
		return true
	}
	return false
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// selectRejectPermissionOptionID picks a reject option from the offered
// list so Cursor/Grok do not error on a foreign optionId.
func selectRejectPermissionOptionID(params json.RawMessage) string {
	ids := permissionOptionIDs(params)
	if len(ids) == 0 {
		return "reject-once"
	}
	has := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		has[id] = struct{}{}
	}
	for _, want := range []string{"reject-once", "reject_once", "reject-always", "reject_always"} {
		if _, ok := has[want]; ok {
			return want
		}
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "reject") {
			return id
		}
	}
	return ids[0]
}

// permissionSelectedReply is the session/request_permission result body.
// Ledger mutations pick a reject option and attach LedgerRefuseReason so
// the agent is told to use the bullseye tool, not only that the call died.
func permissionSelectedReply(params json.RawMessage) map[string]any {
	optionID := selectPermissionOptionID(params)
	out := map[string]any{
		"outcome": map[string]any{
			"outcome":  "selected",
			"optionId": optionID,
		},
	}
	if permissionMutatesBullseye(params) {
		out["_meta"] = map[string]any{"reason": LedgerRefuseReason}
	}
	return out
}
