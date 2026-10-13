package claudia

// ApplyBrokerTaskWriteDefault gives an unrestricted Codex task_run the
// write access a Claude or Grok broker task already has.
//
// The Claude Task path runs unrestricted (no ToolPolicy) with
// --dangerously-skip-permissions, and Grok's runs with its own
// always-approve mode, so a `claudia broker task` seat on either can edit
// its workdir. `codex exec` with no --sandbox falls back to the user's
// config.toml and then to Codex's built-in read-only sandbox — on Colossus
// (codex-cli 0.160.0, no sandbox_mode in ~/.codex/config.toml) that meant
// a pick-remaining task landing on Codex could read but not write, and
// produced nothing (2026-10-12, progress-reports draft).
//
// The default applies only when the caller left every Codex sandbox field
// empty and asked for no tool restriction: an explicit SandboxMode,
// ApprovalPolicy or SandboxGitWrite, a ToolPolicy, or DisallowTools are
// the caller's own choice and are left alone. It sets workspace-write
// with approval "never", and SandboxGitWrite when the workdir's Git
// directory resolves (so the seat can commit, as Claude's can); a workdir
// outside Git still gets workspace-write.
//
// The direct (library) Task keeps Codex's read-only default; this is the
// broker seat contract, applied by the daemon after pick-remaining has
// chosen the provider.
func ApplyBrokerTaskWriteDefault(cfg *TaskConfig) {
	if cfg == nil || cfg.Provider != ProviderCodex {
		return
	}
	if cfg.ToolPolicy != nil || len(cfg.DisallowTools) > 0 {
		return
	}
	if cfg.SandboxMode != "" || cfg.ApprovalPolicy != "" || cfg.SandboxGitWrite {
		return
	}
	cfg.SandboxMode = codexSandboxWorkspaceWrite
	cfg.ApprovalPolicy = "never"
	if roots, err := codexGitWritableRoots(cfg.WorkDir); err == nil && len(roots) > 0 {
		cfg.SandboxGitWrite = true
	}
}

// ApplyBrokerGrantWriteDefault is the grant-seat twin of
// [ApplyBrokerTaskWriteDefault]. A `claudia broker grant --provider codex`
// seat runs `codex app-server`, whose session default is read-only
// (defaultCodexSandbox), so a Codex work seat could read its workdir but
// every write failed with "read-only filesystem, permission escalation
// disabled" (pimp-den-casks, 2026-10-13) while Claude and Grok seats wrote
// freely. The app-server already runs with approval "never"; this sets
// workspace-write, plus SandboxGitWrite when the workdir is in Git.
//
// It applies only to the CLI-runtime Codex seat (ProviderCodex) when the
// caller set no sandbox field and no DisallowTools; anything explicit is
// the caller's own choice. OMP seats (openai-codex) carry no Codex sandbox
// — the sidecar refuses sandbox fields — and are left alone.
func ApplyBrokerGrantWriteDefault(def *AgentDef) {
	if def == nil || def.Provider != ProviderCodex {
		return
	}
	if len(def.DisallowTools) > 0 {
		return
	}
	if def.SandboxMode != "" || def.SandboxGitWrite || len(def.SandboxWritableRoots) > 0 || def.SandboxNetworkAccess {
		return
	}
	def.SandboxMode = codexSandboxWorkspaceWrite
	if roots, err := codexGitWritableRoots(def.WorkDir); err == nil && len(roots) > 0 {
		def.SandboxGitWrite = true
	}
}
