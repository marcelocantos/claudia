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
			Title    string `json:"title"`
			Kind     string `json:"kind"`
			ToolName string `json:"toolName"`
			RawInput map[string]any `json:"rawInput"`
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
	return out
}

func isMutatingLedgerTool(name, kind string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "edit", "write", "delete":
		return true
	}
	switch n {
	case "strreplace", "write", "edit", "multiedit", "notebookedit",
		"write_file", "writefile", "search_replace", "searchreplace":
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
