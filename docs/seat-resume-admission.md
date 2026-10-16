# Seat resume admission (T177 prerequisite)

All subscription seat names, including an omitted provider (Claude), dispatch
through the OMP sidecar. Provider aliases are preserved; normalizing the omitted
provider does not change the special read-only default of bare `codex`.

A generation freshly registered in the current Registry may start without history.
Every persisted subscription generation requires resume after registry reload,
regardless of provider spelling or the Materialized flag. MarkMaterialized is the
same host attestation for every sidecar provider; it no longer requires Claude CLI
JSONL for the Claude alias. Existing native Claude JSONL also requires resume,
so it cannot silently become an empty sidecar conversation.

Config.RequireResume travels through the existing broker grant and an optional
internal sidecar load field. The sidecar checks its canonical SeatStore for the
exact seat and session before constructing the engine. Missing, header-only,
terminal-receipt-only and usage-only stores refuse. Reconstructable conversation
history or validated pending accepted input qualifies; the existing receipt queue
recovers that input without a second replay mechanism.
Malformed and foreign-identity stores fail without repairing their bytes. A matching
already-loaded seat can be adopted without reminting; a different generation fails
with the identity-mismatch reason. No seat-wide spool evidence can authorize resume.
A ready response must explicitly acknowledge required-resume admission. An old
sidecar cannot satisfy it by reporting either a fresh launch or adoption on retry.

Non-default PermissionMode, DisallowTools, ExtraArgs, MCPConfig, native connect
endpoint settings and custom TermLogPath fail before transport with CapabilityError.
Empty/default bypassPermissions remains accepted for compatibility; it does not
promise interactive CLI approvals. MCPServers and the existing sandbox policy are
preserved. Pool configuration and implementation are deferred to the deletion slice.

Verification uses actual SDK/broker/sidecar sockets and SeatStore restoration with
fake credentials and no model prompts, plus configuration and lifecycle race tests.
Live-provider verification and the full paired gate remain outstanding; this
prerequisite does not achieve T177. The older request-field audit still probes
legacy constructor details outside the changed refusal fields until the backend
implementations are deleted; it is not evidence of full sidecar field coverage.
