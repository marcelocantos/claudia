package claudia

// ApplyBrokerGrantWriteDefault gives an unrestricted Codex grant write access.
// A Codex grant's safe default is read-only
// (🎯T37), so a Codex work seat could read its workdir but every write
// failed (pimp-den-casks, 2026-10-13) while Claude and Grok seats wrote
// freely. This sets workspace-write, plus SandboxGitWrite when the workdir
// is in Git. Since 🎯T178 the seat runs on the sidecar, and ompSandboxFor
// maps that onto its tools: Write and Bash, writes confined to the workdir
// and the repo's git dirs.
//
// It applies only to the bare Codex seat (ProviderCodex) when the caller
// set no sandbox field and no DisallowTools; anything explicit is the
// caller's own choice. Subscription-id seats (openai-codex) with no sandbox
// field run the sidecar's tools unconfined, as they always have, and are
// left alone.
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
