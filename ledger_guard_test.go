// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestIsBullseyeYAML(t *testing.T) {
	if !IsBullseyeYAML("/Users/x/work/jevons/bullseye.yaml") {
		t.Fatal("abs path")
	}
	if !IsBullseyeYAML("bullseye.yaml") {
		t.Fatal("base")
	}
	if IsBullseyeYAML("agents-guide.md") || IsBullseyeYAML("") {
		t.Fatal("false positive")
	}
}

func TestSelectPermissionRejectsCursorStrReplaceOfLedger(t *testing.T) {
	params := json.RawMessage(`{
		"sessionId": "sid",
		"toolCall": {
			"toolCallId": "tc-1",
			"title": "StrReplace",
			"kind": "edit",
			"locations": [{"path": "/Users/marcelo/work/github.com/marcelocantos/jevons/bullseye.yaml"}],
			"rawInput": {"path": "/Users/marcelo/work/github.com/marcelocantos/jevons/bullseye.yaml"}
		},
		"options": [
			{"optionId": "allow-once"},
			{"optionId": "allow-always"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "reject-once" {
		t.Fatalf("got %q, want reject-once (🎯T546)", got)
	}
	reply := permissionSelectedReply(params)
	outcome, _ := reply["outcome"].(map[string]any)
	if outcome["optionId"] != "reject-once" {
		t.Fatalf("reply optionId = %v, want reject-once", outcome["optionId"])
	}
	meta, _ := reply["_meta"].(map[string]any)
	reason, _ := meta["reason"].(string)
	if !strings.Contains(reason, "jevons_target_file") || !strings.Contains(reason, "T546") {
		t.Fatalf("permission refuse must name the bullseye tool: %q", reason)
	}
}

func TestSelectPermissionAllowsCursorEditOfOtherFile(t *testing.T) {
	params := json.RawMessage(`{
		"toolCall": {
			"title": "StrReplace",
			"kind": "edit",
			"locations": [{"path": "/tmp/foo.go"}]
		},
		"options": [
			{"optionId": "allow-once"},
			{"optionId": "allow-always"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "allow-always" {
		t.Fatalf("got %q, want allow-always", got)
	}
}

func TestSelectPermissionRejectsBashWriteOfLedger(t *testing.T) {
	params := json.RawMessage(`{
		"toolCall": {
			"title": "Bash",
			"kind": "execute",
			"toolName": "run_terminal_command",
			"rawInput": {"command": "python -c \"open('bullseye.yaml','w').write('x')\""}
		},
		"options": [
			{"optionId": "allow_always"},
			{"optionId": "allow_always_bash"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "reject-once" {
		t.Fatalf("got %q, want reject-once (🎯T546 shell write)", got)
	}
	reply := permissionSelectedReply(params)
	meta, _ := reply["_meta"].(map[string]any)
	reason, _ := meta["reason"].(string)
	if !strings.Contains(reason, "T546") {
		t.Fatalf("shell ledger refuse must carry LedgerRefuseReason: %q", reason)
	}
}

func TestSelectPermissionAllowsBashWithoutLedger(t *testing.T) {
	params := json.RawMessage(`{
		"toolCall": {
			"title": "Bash",
			"kind": "execute",
			"rawInput": {"command": "ls"}
		},
		"options": [
			{"optionId": "allow_always"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "allow_always" {
		t.Fatalf("unrelated shell must stay allow-always: got %q", got)
	}
}

func TestSelectPermissionAllowsReadOfLedger(t *testing.T) {
	params := json.RawMessage(`{
		"toolCall": {
			"title": "Read",
			"kind": "read",
			"locations": [{"path": "/tmp/bullseye.yaml"}]
		},
		"options": [
			{"optionId": "allow-once"},
			{"optionId": "allow-always"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "allow-always" {
		t.Fatalf("Read of ledger must not hit the mutate refuse: got %q", got)
	}
}

// 🎯T67 / Fable F2: Claude Session/Task must emit a T546 deny rule.
// bypassPermissions / --dangerously-skip-permissions are not coverage.

func TestClaudeTaskArgsIncludeLedgerDeny(t *testing.T) {
	argv := claudeTaskArgs(taskRunRequest{Prompt: "summarise this"})
	if !slices.Contains(argv, "--dangerously-skip-permissions") {
		t.Fatal("task argv lost --dangerously-skip-permissions; T67 covers the deny rule, not a mode change")
	}
	assertClaudeArgvRefusesLedgerMutation(t, argv)
}

func TestClaudeSessionArgsIncludeLedgerDeny(t *testing.T) {
	argv := claudeAgentArgs(agentStartRequest{
		SessionID:       "t67-session",
		DisallowedTools: BaseDisallowedTools,
		Config:          Config{PermissionMode: "bypassPermissions"},
	})
	if !argvHolds(argv, "bypassPermissions") {
		t.Fatal("session argv lost bypassPermissions; T67 covers the deny rule, not a mode change")
	}
	assertClaudeArgvRefusesLedgerMutation(t, argv)
}

func TestClaudeBypassPermissionsIsNotLedgerCoverage(t *testing.T) {
	// A spawn that only has skip/bypass must fail the oracle. This is the
	// "deleting the rule fails" check: empty deny settings are not T546.
	if claudeArgvRefusesLedgerMutation([]string{
		"--dangerously-skip-permissions",
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", BaseDisallowedTools,
	}) {
		t.Fatal("bypass/skip-permissions alone must not count as T546 coverage")
	}
}

func TestClaudeLedgerSettingsRefuseWriteEditBash(t *testing.T) {
	rules := ClaudeLedgerDenyRules()
	if len(rules) == 0 {
		t.Fatal("ClaudeLedgerDenyRules is empty; deleting the rule must fail this oracle")
	}
	cases := []struct {
		tool, input string
		want        bool
	}{
		{"Write", "/tmp/repo/bullseye.yaml", true},
		{"Edit", "bullseye.yaml", true},
		{"MultiEdit", `/Users/x/work/jevons/bullseye.yaml`, true},
		{"Bash", `python -c "open('bullseye.yaml','w').write('x')"`, true},
		{"Write", "/tmp/foo.go", false},
		{"Bash", "ls", false},
		{"Read", "/tmp/bullseye.yaml", false},
	}
	for _, tc := range cases {
		got := ClaudeToolCallDeniedByLedger(tc.tool, tc.input)
		if got != tc.want {
			t.Errorf("%s %q denied=%v, want %v (rules=%v)", tc.tool, tc.input, got, tc.want, rules)
		}
	}
}

func TestClaudeLedgerSettingsJSONComesFromDenyRules(t *testing.T) {
	raw := ClaudeLedgerSettingsJSON()
	if len(ClaudeLedgerDenyRules()) == 0 {
		t.Fatal("deny rules deleted")
	}
	for _, rule := range ClaudeLedgerDenyRules() {
		if !strings.Contains(raw, rule) {
			t.Errorf("settings JSON missing rule %q: %s", rule, raw)
		}
	}
}

func TestClaudeLedgerSettingsFlagBeforeDisallowedTools(t *testing.T) {
	task := claudeTaskArgs(taskRunRequest{Prompt: "the prompt"})
	session := claudeAgentArgs(agentStartRequest{
		SessionID:       "t67-order",
		DisallowedTools: BaseDisallowedTools,
		Config:          Config{PermissionMode: "bypassPermissions"},
	})
	for name, argv := range map[string][]string{"task": task, "session": session} {
		settingsAt := slices.Index(argv, "--settings")
		disallowAt := slices.Index(argv, "--disallowedTools")
		if settingsAt < 0 || disallowAt < 0 {
			t.Fatalf("%s argv missing --settings or --disallowedTools: %v", name, argv)
		}
		if settingsAt > disallowAt {
			t.Errorf("%s: --settings at %d comes after variadic --disallowedTools at %d: %v",
				name, settingsAt, disallowAt, argv)
		}
	}
}

func assertClaudeArgvRefusesLedgerMutation(t *testing.T, argv []string) {
	t.Helper()
	if !claudeArgvRefusesLedgerMutation(argv) {
		t.Fatalf("Claude spawn argv does not refuse bullseye.yaml mutation: %v", argv)
	}
}

func claudeArgvRefusesLedgerMutation(argv []string) bool {
	i := slices.Index(argv, "--settings")
	if i < 0 || i+1 >= len(argv) {
		return false
	}
	var settings struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if json.Unmarshal([]byte(argv[i+1]), &settings) != nil {
		return false
	}
	return ledgerDenyRulesCoverWriteEditBash(settings.Permissions.Deny)
}

func ledgerDenyRulesCoverWriteEditBash(rules []string) bool {
	var hasEdit, hasBash bool
	for _, rule := range rules {
		if !strings.Contains(rule, "bullseye.yaml") {
			continue
		}
		if strings.HasPrefix(rule, "Edit(") {
			hasEdit = true
		}
		if strings.HasPrefix(rule, "Bash(") {
			hasBash = true
		}
	}
	return hasEdit && hasBash
}
