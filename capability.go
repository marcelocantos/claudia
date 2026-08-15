// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "fmt"

// Capability names a provider behaviour whose support level claudia
// reports. A Capability value is also what lands in the Capability field
// of [CapabilityError], so a caller can match a failure against the same
// name it queried.
//
// Capability values are the *names* of behaviours; [CapabilityStatus]
// values (CapabilitySupported / CapabilityUnsupported /
// CapabilityExperimental) are how far claudia supports them.
type Capability string

const (
	// CapabilityTask is one-shot prompt execution via [Task.Run].
	CapabilityTask Capability = "task"
	// CapabilitySession is a persistent agent via [Start].
	CapabilitySession Capability = "session"
	// CapabilityResume is continuing a prior provider session id.
	CapabilityResume Capability = "resume"
	// CapabilityRewind is [Agent.Rewind] turn-boundary rollback.
	CapabilityRewind Capability = "rewind"
	// CapabilityCost is monetary cost accounting in [Usage].CostUSD.
	// Token counts are reported separately and are not this capability.
	CapabilityCost Capability = "cost"
	// CapabilityTmuxAttach is a human-attachable tmux window
	// ([Agent.AttachCommand]).
	CapabilityTmuxAttach Capability = "tmux_attach"
	// CapabilityTerminalLog is the raw rendered-terminal byte log
	// ([Agent.TermLogPath], [Agent.SubscribeTerminal]).
	CapabilityTerminalLog Capability = "terminal_log"
	// CapabilityPermissionMode is Claude-style Config.PermissionMode
	// semantics. Codex sandbox/approval flags are NOT this capability:
	// they are Codex-native settings with different meanings.
	CapabilityPermissionMode Capability = "permission_mode"
	// CapabilityToolRestrictions is honouring TaskConfig.DisallowTools /
	// Config.DisallowTools so named tools cannot run.
	CapabilityToolRestrictions Capability = "tool_restrictions"
	// CapabilityImageInput is attaching images to a prompt.
	CapabilityImageInput Capability = "image_input"
	// CapabilityWebSearch is claudia binding the provider's web-search
	// switch. Where claudia does not bind it, the provider default
	// applies and claudia can neither guarantee nor forbid web access.
	CapabilityWebSearch Capability = "web_search"
	// CapabilitySandboxPolicy is honouring TaskConfig.SandboxMode /
	// TaskConfig.ApprovalPolicy — the Codex-native sandbox and approval
	// settings. Deliberately distinct from CapabilityPermissionMode:
	// these are Codex's own controls, not Claude's, and no translation
	// between the two has been proven.
	CapabilitySandboxPolicy Capability = "sandbox_policy"
	// CapabilityExtraArgs is passing Config.ExtraArgs through to the
	// provider process verbatim. A provider claudia does not launch as a
	// CLI with caller-supplied argv cannot honour it.
	CapabilityExtraArgs Capability = "extra_args"
)

// CapabilityStatus classifies how far claudia supports one [Capability]
// on one [Provider].
type CapabilityStatus string

const (
	// CapabilitySupported means claudia binds a public provider contract
	// for the behaviour and tests cover it.
	CapabilitySupported CapabilityStatus = "supported"
	// CapabilityUnsupported means the provider has no supported public
	// contract for the requested behaviour.
	CapabilityUnsupported CapabilityStatus = "unsupported"
	// CapabilityExperimental means the provider may eventually support the
	// behaviour, but claudia is deliberately failing closed until the public
	// contract is proven.
	CapabilityExperimental CapabilityStatus = "experimental"
)

// CapabilityError reports that a provider capability is unsupported or
// experimental in the current implementation.
type CapabilityError struct {
	Provider   Provider
	Capability Capability
	Status     CapabilityStatus
	Reason     string
}

func (e *CapabilityError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("%s provider %s capability is %s", e.Provider, e.Capability, e.Status)
	}
	return fmt.Sprintf("%s provider %s capability is %s: %s", e.Provider, e.Capability, e.Status, e.Reason)
}

func unsupportedCapability(provider Provider, capability Capability, reason string) *CapabilityError {
	return &CapabilityError{
		Provider:   provider,
		Capability: capability,
		Status:     CapabilityUnsupported,
		Reason:     reason,
	}
}

func experimentalCapability(provider Provider, capability Capability, reason string) *CapabilityError {
	return &CapabilityError{
		Provider:   provider,
		Capability: capability,
		Status:     CapabilityExperimental,
		Reason:     reason,
	}
}

type capabilityClaim struct {
	status CapabilityStatus
	reason string
}

// reportedCapabilities is the closed set every provider must make an
// explicit claim about. Adding a name here without adding a claim for
// every provider fails TestProviderCapabilityMatrixIsTotal — silence is
// not allowed to read as support.
func reportedCapabilities() []Capability {
	return []Capability{
		CapabilityTask,
		CapabilitySession,
		CapabilityResume,
		CapabilityRewind,
		CapabilityCost,
		CapabilityTmuxAttach,
		CapabilityTerminalLog,
		CapabilityPermissionMode,
		CapabilityToolRestrictions,
		CapabilityImageInput,
		CapabilityWebSearch,
		CapabilitySandboxPolicy,
		CapabilityExtraArgs,
	}
}

// providerCapabilityClaims is the single source of truth behind
// [ProviderCapabilityStatus], [ProviderCapabilityMatrix] and
// [CheckCapability]. Production fail-closed paths read it, so a claim
// edited here changes behaviour, not just documentation.
var providerCapabilityClaims = map[Provider]map[Capability]capabilityClaim{
	ProviderOllama: {
		CapabilityTask: {status: CapabilitySupported},
		CapabilitySession: {
			status: CapabilityUnsupported,
			reason: "Ollama's generate endpoint is one-shot; claudia binds no persistent session for it",
		},
		CapabilityResume: {
			status: CapabilityUnsupported,
			reason: "Ollama's generate endpoint carries no session id to resume",
		},
		CapabilityRewind: {
			status: CapabilityUnsupported,
			reason: "rewind needs a session with turn boundaries, and Ollama Task mode has none",
		},
		CapabilityCost: {
			status: CapabilityUnsupported,
			reason: "local inference has no price per token; its cost is latency, and reporting zero dollars would read as a measured spend",
		},
		CapabilityTmuxAttach: {
			status: CapabilityUnsupported,
			reason: "there is no interactive process to attach to",
		},
		CapabilityTerminalLog: {
			status: CapabilityUnsupported,
			reason: "there is no rendered terminal; the API path emits structured events only",
		},
		CapabilityPermissionMode: {
			status: CapabilityUnsupported,
			reason: "permission modes are a Claude Code concept with no Ollama counterpart",
		},
		CapabilityToolRestrictions: {
			status: CapabilityUnsupported,
			reason: "Ollama's generate endpoint runs no tools, so there is nothing to restrict; a request naming DisallowTools is refused rather than silently ignored",
		},
		CapabilityImageInput: {
			status: CapabilityUnsupported,
			reason: "claudia has no API for attaching images to a prompt on any provider",
		},
		CapabilityWebSearch: {
			status: CapabilityUnsupported,
			reason: "Ollama has no web-search switch for claudia to bind",
		},
		CapabilitySandboxPolicy: {
			status: CapabilityUnsupported,
			reason: sandboxPolicyIsCodexOnlyReason,
		},
		CapabilityExtraArgs: {
			status: CapabilityUnsupported,
			reason: "the Ollama path is an HTTP request, not a process claudia launches, so there is no argv to extend",
		},
	},
	ProviderClaude: {
		CapabilityTask:             {status: CapabilitySupported},
		CapabilitySession:          {status: CapabilitySupported},
		CapabilityResume:           {status: CapabilitySupported},
		CapabilityRewind:           {status: CapabilitySupported},
		CapabilityCost:             {status: CapabilitySupported},
		CapabilityTmuxAttach:       {status: CapabilitySupported},
		CapabilityTerminalLog:      {status: CapabilitySupported},
		CapabilityPermissionMode:   {status: CapabilitySupported},
		CapabilityToolRestrictions: {status: CapabilitySupported},
		CapabilityImageInput: {
			status: CapabilityUnsupported,
			reason: "claudia has no API for attaching images to a prompt on any provider",
		},
		CapabilityWebSearch: {status: CapabilitySupported},
		CapabilitySandboxPolicy: {
			status: CapabilityUnsupported,
			reason: sandboxPolicyIsCodexOnlyReason,
		},
		CapabilityExtraArgs: {status: CapabilitySupported},
	},
	ProviderCodex: {
		CapabilityTask:    {status: CapabilitySupported},
		CapabilityResume:  {status: CapabilitySupported},
		CapabilitySession: {status: CapabilityExperimental, reason: codexSessionReason},
		CapabilityRewind:  {status: CapabilityUnsupported, reason: codexRewindReason},
		CapabilityCost: {
			status: CapabilityUnsupported,
			reason: "codex exec --json reports token usage but no monetary cost; Usage.CostUSD stays zero",
		},
		CapabilityTmuxAttach: {
			status: CapabilityUnsupported,
			reason: "claudia does not drive the Codex TUI in tmux; there is no attachable window",
		},
		CapabilityTerminalLog: {
			status: CapabilityUnsupported,
			reason: "Codex Task mode consumes a JSON stream, not a PTY, so there are no rendered terminal bytes to log",
		},
		CapabilityPermissionMode: {
			status: CapabilityUnsupported,
			reason: "Codex sandbox/approval flags are Codex-native and are not proven equivalent to Claude PermissionMode",
		},
		CapabilityToolRestrictions: {status: CapabilityUnsupported, reason: codexToolRestrictionsReason},
		CapabilityImageInput: {
			status: CapabilityUnsupported,
			reason: "claudia has no API for attaching images to a prompt on any provider",
		},
		CapabilityWebSearch: {
			status: CapabilityUnsupported,
			reason: "claudia does not bind Codex's --search flag, so web access is left at the Codex default and cannot be guaranteed or forbidden",
		},
		CapabilitySandboxPolicy: {status: CapabilitySupported},
		CapabilityExtraArgs: {
			status: CapabilityUnsupported,
			reason: "Config.ExtraArgs is a Session-mode field and Codex Session is not wired; the Codex Task path builds its argv from typed fields only",
		},
	},
	ProviderGrok: {
		CapabilityTask:    {status: CapabilitySupported},
		CapabilityResume:  {status: CapabilitySupported},
		CapabilitySession: {status: CapabilitySupported},
		CapabilityRewind:  {status: CapabilityUnsupported, reason: grokRewindReason},
		CapabilityCost: {
			status: CapabilityUnsupported,
			reason: "grok streaming-json carries no cost events and the SuperGrok usage panel has no public API",
		},
		CapabilityTmuxAttach: {
			status: CapabilityUnsupported,
			reason: "Grok ACP runs process-local over stdio; there is no tmux window to attach",
		},
		CapabilityTerminalLog: {
			status: CapabilityUnsupported,
			reason: "Grok ACP is a JSON-RPC stream, not a PTY, so there are no rendered terminal bytes to log",
		},
		CapabilityPermissionMode: {
			status: CapabilityUnsupported,
			reason: "Grok Task mode hardcodes --permission-mode bypassPermissions; Claude PermissionMode values are not mapped",
		},
		CapabilityToolRestrictions: {status: CapabilityUnsupported, reason: grokToolRestrictionsReason},
		CapabilityImageInput: {
			status: CapabilityUnsupported,
			reason: "claudia has no API for attaching images to a prompt on any provider",
		},
		CapabilityWebSearch: {
			status: CapabilityUnsupported,
			reason: "claudia does not bind a Grok web-search switch, so web access is left at the Grok default",
		},
		CapabilitySandboxPolicy: {
			status: CapabilityUnsupported,
			reason: sandboxPolicyIsCodexOnlyReason,
		},
		CapabilityExtraArgs: {
			status: CapabilityUnsupported,
			reason: grokExtraArgsReason,
		},
	},
	ProviderBedrock: {
		CapabilityTask:    {status: CapabilitySupported},
		CapabilitySession: {status: CapabilityUnsupported, reason: bedrockSessionReason},
		CapabilityResume: {
			status: CapabilityUnsupported,
			reason: "Bedrock v1 is stateless ConverseStream; there is no provider-side session to resume",
		},
		CapabilityRewind: {status: CapabilityUnsupported, reason: bedrockRewindReason},
		CapabilityCost: {
			status: CapabilityUnsupported,
			reason: "ConverseStream reports token counts only; claudia does not price them",
		},
		CapabilityTmuxAttach: {
			status: CapabilityUnsupported,
			reason: "Bedrock v1 is an API path with no local process or tmux window",
		},
		CapabilityTerminalLog: {
			status: CapabilityUnsupported,
			reason: "Bedrock v1 is an API path with no PTY to log",
		},
		CapabilityPermissionMode: {
			status: CapabilityUnsupported,
			reason: "ConverseStream has no permission-mode concept",
		},
		CapabilityToolRestrictions: {
			status: CapabilityUnsupported,
			reason: "claudia does not expose Bedrock tool configuration, so DisallowTools cannot be honoured",
		},
		CapabilityImageInput: {
			status: CapabilityUnsupported,
			reason: "claudia has no API for attaching images to a prompt on any provider",
		},
		CapabilityWebSearch: {
			status: CapabilityUnsupported,
			reason: "ConverseStream has no claudia-bound web-search tool",
		},
		CapabilitySandboxPolicy: {
			status: CapabilityUnsupported,
			reason: sandboxPolicyIsCodexOnlyReason,
		},
		CapabilityExtraArgs: {
			status: CapabilityUnsupported,
			reason: "Bedrock v1 is an API call, not a process claudia launches, so there is no argv to extend",
		},
	},
}

// Reasons shared between the claim table and the fail-closed call sites
// that surface them, so the error a caller sees and the matrix a caller
// queries cannot drift apart.
const (
	codexSessionReason = "persistent Session mode requires the app-server live contract spike to complete"
	codexRewindReason  = "Codex rewind requires a public app-server fork/resume contract; private transcript truncation is forbidden"
	//nolint:lll // one sentence, kept whole for the error message.
	codexToolRestrictionsReason = "codex exec has no per-tool disallow flag; running the task would silently ignore DisallowTools and leave every tool enabled"
	grokRewindReason            = "Grok rewind requires a public ACP/session API; private session-file truncation is forbidden"
	// Unlike Codex, the Grok CLI is not missing the machinery: `grok
	// --deny <RULE>` gates tool invocations and `grok --disallowed-tools
	// <IDS>` strips tools from the agent's toolset outright. What is
	// missing is a translation claudia can stand behind, so the reason
	// says so rather than claiming a gap in Grok that does not exist.
	//nolint:lll // one sentence, kept whole for the error message.
	grokToolRestrictionsReason = "grok has --deny permission rules and --disallowed-tools toolset removal, but claudia translates DisallowTools into neither: Task hardcodes --permission-mode bypassPermissions, which grok resolves by appending a catch-all allow rule, and grok accepts tool names it does not recognise without complaint, so a name claudia failed to translate would be dropped exactly as silently as it is dropped now"
	//nolint:lll // one sentence, kept whole for the error message.
	grokToolRestrictionsUnwiredReason = "the Grok tool_restrictions claim was flipped to supported, but grokTaskArgs still emits no --deny or --disallowed-tools argument, so the restriction would be dropped; wire the translation before changing the claim"
	bedrockSessionReason              = "Bedrock v1 is Task-only (ConverseStream); Session/tmux is not supported"
	bedrockRewindReason               = "Bedrock v1 is Task-only and has no Session transcript to rewind"
	// SandboxMode and ApprovalPolicy name `codex exec` flags. Every other
	// provider used to take them, drop them on the floor and run anyway —
	// a caller who asked for a read-only sandbox got an unrestricted one
	// and no signal. Nothing here claims the other providers lack an
	// isolation story; it claims claudia has not proven a translation, so
	// it refuses rather than pretending.
	//nolint:lll // one sentence, kept whole for the error message.
	sandboxPolicyIsCodexOnlyReason = "SandboxMode and ApprovalPolicy are `codex exec` flags with no proven equivalent here; claudia refuses rather than running with the sandbox the caller asked for silently dropped"
	//nolint:lll // one sentence, kept whole for the error message.
	grokExtraArgsReason = "claudia drives Grok Session over ACP on a fixed `grok agent … stdio`/`serve` command line, so caller argv has nowhere to go"
	//nolint:lll // one sentence, kept whole for the error message.
	grokSessionToolRestrictionsUnwiredReason = "the Grok tool_restrictions claim was flipped to supported, but the Grok Session path still emits no --deny or --disallowed-tools argument, so the restriction would be dropped; wire the translation before changing the claim"
	//nolint:lll // one sentence, kept whole for the error message.
	grokPermissionModeReason = "Grok Session hardcodes ACP always-approve/yoloMode; a PermissionMode other than bypassPermissions would be dropped, leaving an agent more permissive than the caller asked for"
)

// capabilityRefusal is the error a provider path returns to a caller who
// set a request field that path cannot honour.
//
// It survives its own success condition. [CheckCapability] returns nil the
// moment a claim flips to supported, and a path that handed that nil back
// as its error would return (nil, nil) — no run, no error, and the caller
// left with exactly the behaviour they asked not to have. 399b1c8 found
// that shape by mutating a claim, so the fallback keeps the refusal alive
// with a reason naming what is still unwired: the claim and the code that
// materialises the request have to move together.
func capabilityRefusal(provider Provider, capability Capability, unwiredReason string) error {
	if err := CheckCapability(provider, capability); err != nil {
		return err
	}
	return unsupportedCapability(provider, capability, unwiredReason)
}

// providerCapabilityClaim resolves one claim, failing closed. An unknown
// provider or an unclaimed capability reports unsupported rather than
// inheriting Claude's answer — the whole point of the matrix is that
// silence never reads as parity.
func providerCapabilityClaim(provider Provider, capability Capability) capabilityClaim {
	if provider == "" {
		provider = ProviderClaude
	}
	if claims, ok := providerCapabilityClaims[provider]; ok {
		if claim, ok := claims[capability]; ok {
			return claim
		}
	}
	return capabilityClaim{
		status: CapabilityUnsupported,
		reason: fmt.Sprintf("claudia makes no %s capability claim for provider %q", capability, provider),
	}
}

// ProviderCapabilityStatus reports how far claudia supports capability on
// provider. An empty Provider means [ProviderClaude]. Unknown providers
// and unclaimed capabilities report [CapabilityUnsupported].
func ProviderCapabilityStatus(provider Provider, capability Capability) CapabilityStatus {
	return providerCapabilityClaim(provider, capability).status
}

// ProviderCapabilityReason returns the documented rationale behind
// [ProviderCapabilityStatus]. It is empty for supported capabilities.
func ProviderCapabilityReason(provider Provider, capability Capability) string {
	return providerCapabilityClaim(provider, capability).reason
}

// ProviderCapabilityMatrix returns provider's status for every capability
// claudia reports on. Callers that render a support table should use this
// rather than probing behaviour.
func ProviderCapabilityMatrix(provider Provider) map[Capability]CapabilityStatus {
	matrix := make(map[Capability]CapabilityStatus, len(reportedCapabilities()))
	for _, capability := range reportedCapabilities() {
		matrix[capability] = ProviderCapabilityStatus(provider, capability)
	}
	return matrix
}

// CheckCapability returns nil when provider supports capability outright,
// and a *[CapabilityError] carrying the documented reason otherwise. This
// is the gate production fail-closed paths call, so a caller who checks
// ahead of time gets exactly the error the operation would have returned.
func CheckCapability(provider Provider, capability Capability) error {
	claim := providerCapabilityClaim(provider, capability)
	if claim.status == CapabilitySupported {
		return nil
	}
	if provider == "" {
		provider = ProviderClaude
	}
	return &CapabilityError{
		Provider:   provider,
		Capability: capability,
		Status:     claim.status,
		Reason:     claim.reason,
	}
}

// capabilityGaps lists the capabilities provider does not support
// outright, in reporting order. Used by tests and by callers rendering a
// "what you lose by switching provider" summary.
func capabilityGaps(provider Provider) []Capability {
	var gaps []Capability
	for _, capability := range reportedCapabilities() {
		if ProviderCapabilityStatus(provider, capability) != CapabilitySupported {
			gaps = append(gaps, capability)
		}
	}
	return gaps
}

// providerCapabilities is the internal backend-wiring view: does this
// backend actually route the operation? It is deliberately narrower than
// the public matrix (no image/web-search entries, no experimental state)
// and is kept honest against it by
// TestProviderCapabilityMatrixMatchesBackendClaims.
type providerCapabilities struct {
	Task          bool
	Session       bool
	Resume        bool
	Rewind        bool
	Cost          bool
	Permissions   bool
	TmuxAttach    bool
	TerminalBytes bool
}

// backendCapabilityNames maps each providerCapabilities field to the
// public capability it claims, for the consistency ratchet.
func backendCapabilityNames(caps providerCapabilities) map[Capability]bool {
	return map[Capability]bool{
		CapabilityTask:           caps.Task,
		CapabilitySession:        caps.Session,
		CapabilityResume:         caps.Resume,
		CapabilityRewind:         caps.Rewind,
		CapabilityCost:           caps.Cost,
		CapabilityPermissionMode: caps.Permissions,
		CapabilityTmuxAttach:     caps.TmuxAttach,
		CapabilityTerminalLog:    caps.TerminalBytes,
	}
}

func claudeProviderCapabilities() providerCapabilities {
	return providerCapabilities{
		Task:          true,
		Session:       true,
		Resume:        true,
		Rewind:        true,
		Cost:          true,
		Permissions:   true,
		TmuxAttach:    true,
		TerminalBytes: true,
	}
}
