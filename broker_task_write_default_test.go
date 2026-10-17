// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real refusal line shape from the 2026-10-12 progress-reports
// transcript: an assistant message with top-level error "rate_limit"
// and the spend-limit text.
const claudeSpendLimitLine = `{"type":"assistant","error":"rate_limit","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"You've hit your monthly spend limit · raise it at https://claude.ai/settings/usage?from=cc_cli_limit_message · your session limit resets 10:10pm (Australia/Melbourne)"}]},"session_id":"s"}`

func TestParseTaskLineClaudeSpendLimitIsError(t *testing.T) {
	evs := ParseTaskLine([]byte(claudeSpendLimitLine))
	if len(evs) != 1 || evs[0].Type != TaskEventError || !evs[0].IsError {
		t.Fatalf("events = %+v, want one TaskEventError", evs)
	}
	if !strings.HasPrefix(evs[0].ErrorMsg, "claude "+TaskErrorSpendLimit+": ") || !strings.Contains(evs[0].ErrorMsg, "monthly spend limit") {
		t.Fatalf("ErrorMsg = %q", evs[0].ErrorMsg)
	}
	other := ParseTaskLine([]byte(`{"type":"assistant","error":"authentication_failed","message":{"content":[{"type":"text","text":"Not logged in"}]}}`))
	if len(other) != 1 || other[0].ErrorMsg != "claude authentication_failed: Not logged in" {
		t.Fatalf("auth refusal = %+v", other)
	}
	ok := ParseTaskLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`))
	if len(ok) != 1 || ok[0].Type != TaskEventText {
		t.Fatalf("plain assistant = %+v", ok)
	}
}

// The spend-limit flag alone is not a veto: on 2026-10-13 it read true
// with session 95% left and Claude answered. It names the refusal only
// when a plan window is exhausted too.
func TestSpendLimitNamesExhaustionButDoesNotVetoRoom(t *testing.T) {
	now := time.Now()
	pct := func(v float64) *float64 { return &v }
	row := func(p Provider, session, weekly float64, limited bool) PlanUsage {
		u := PlanUsage{Provider: p, Status: PlanUsageAvailable, Windows: []PlanWindow{
			{Name: PlanWindowSession, RemainingPercent: pct(session)},
			{Name: PlanWindowWeekly, RemainingPercent: pct(weekly)},
		}}
		if limited {
			u.Spend = &PlanSpend{Enabled: true, LimitReached: true}
		}
		return u
	}
	roomy := row(ProviderClaude, 95, 90, true)
	if !HasAvailableTokens(roomy, now, nil) {
		t.Fatal("spend-limited claude with plan room refused")
	}
	usage := []PlanUsage{
		row(ProviderCursor, 50, 10, false),
		row(ProviderGrok, 50, 40, false),
		row(ProviderClaude, 0, 90, true),
		row(ProviderCodex, 50, 20, false),
	}
	if HasAvailableTokens(usage[2], now, nil) {
		t.Fatal("claude with an exhausted session admitted")
	}
	pick, err := PickByRemaining(usage, now, nil)
	if err != nil || pick.Provider != ProviderGrok {
		t.Fatalf("pick = %+v, %v; want grok", pick, err)
	}
	snap := ProjectFleetUsage(usage, now, now, nil)
	if c := snap.Providers[2]; c.Admit || !strings.HasPrefix(c.Reason, FleetReasonSpendLimit) {
		t.Fatalf("claude row = %+v, want admit=false reason=spend_limit", c)
	}
	snap = ProjectFleetUsage([]PlanUsage{roomy}, now, now, nil)
	if c := snap.Providers[2]; !c.Admit || strings.HasPrefix(c.Reason, FleetReasonSpendLimit) {
		t.Fatalf("roomy claude row = %+v, want admitted without spend_limit", c)
	}
}

func TestApplyBrokerGrantWriteDefault(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	def := AgentDef{Provider: ProviderCodex, WorkDir: repo}
	ApplyBrokerGrantWriteDefault(&def)
	if def.SandboxMode != codexSandboxWorkspaceWrite || !def.SandboxGitWrite {
		t.Fatalf("codex grant in git repo: got mode=%q git=%v", def.SandboxMode, def.SandboxGitWrite)
	}
	plain := AgentDef{Provider: ProviderCodex, WorkDir: t.TempDir()}
	ApplyBrokerGrantWriteDefault(&plain)
	if plain.SandboxMode != codexSandboxWorkspaceWrite || plain.SandboxGitWrite {
		t.Fatalf("codex grant outside git: got mode=%q git=%v", plain.SandboxMode, plain.SandboxGitWrite)
	}
	explicit := AgentDef{Provider: ProviderCodex, WorkDir: repo, SandboxMode: "read-only"}
	ApplyBrokerGrantWriteDefault(&explicit)
	if explicit.SandboxMode != "read-only" || explicit.SandboxGitWrite {
		t.Fatalf("explicit sandbox overridden: %+v", explicit)
	}
	restricted := AgentDef{Provider: ProviderCodex, WorkDir: repo, DisallowTools: []string{"Bash"}}
	ApplyBrokerGrantWriteDefault(&restricted)
	if restricted.SandboxMode != "" {
		t.Fatalf("DisallowTools seat widened: %+v", restricted)
	}
	for _, p := range []Provider{ProviderClaude, ProviderGrok, "openai-codex", ProviderCursor} {
		other := AgentDef{Provider: p, WorkDir: repo}
		ApplyBrokerGrantWriteDefault(&other)
		if other.SandboxMode != "" || other.SandboxGitWrite {
			t.Fatalf("%s grant got codex sandbox: %+v", p, other)
		}
	}
}

// Live: the def a `claudia broker grant --provider codex` seat gets writes
// a file in its workdir and commits — pimp-den-casks (2026-10-13) could not.
func TestCodexBrokerGrantWriteDefaultLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (this test spends API credit)")
	}
	if _, err := liveCodexBin(); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}
	repo := t109Repo(t)
	t109Git(t, repo, "config", "user.name", "grant")
	t109Git(t, repo, "config", "user.email", "grant@example.invalid")
	def := AgentDef{Name: "codex-grant-write", Provider: ProviderCodex, WorkDir: repo}
	ApplyBrokerGrantWriteDefault(&def)
	cfg := registryConfig(&def, false)
	cfg.TermLogPath = "-"
	agent, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	prompt := "Run exactly this shell command in the current directory and then reply with its output, nothing else:\n" +
		"printf 'WRITE-OK\\n' > draft.md && git add draft.md && git commit -q -m grant-write && echo GRANT-OK"
	if err := liveCodexTurn(t, agent, prompt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(repo, "draft.md"))
	if err != nil || !strings.Contains(string(b), "WRITE-OK") {
		t.Fatalf("draft.md = %q, %v; the codex grant seat did not write its workdir", b, err)
	}
	log, err := exec.Command("git", "-C", repo, "log", "--format=%s", "-1").Output()
	if err != nil || strings.TrimSpace(string(log)) != "grant-write" {
		t.Fatalf("HEAD subject = %q, %v; the codex grant seat could not commit", log, err)
	}
}
