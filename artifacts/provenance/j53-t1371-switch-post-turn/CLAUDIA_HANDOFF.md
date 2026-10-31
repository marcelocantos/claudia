# Claudia handoff — J53 T1371 post-turn switch seed (sidecar)

**From:** jevons J53 worktree (timing fix committed separately)  
**To:** claudia-po — implement in **isolated Claudia repo/worktree**, release, then owner authorizes jevons `go.mod` pin bump.  
**Do not** rely on WIP left in shared clone `/Users/marcelo/work/github.com/marcelocantos/claudia` — preserve that diff; claudia-po owns commit/release.

## Problem

Pending in-place model switch (`maintain.ts` `switchModel`) ran compaction with phase **`pre_turn`** while the held source assistant reply (`STORED`) was still in-flight in agent state. Destination snapshot re-opened the completed source prompt; next turn **re-answered** the held prompt and **glued** real probe text (e.g. `STOREDMARBLEWREN951`, worker `STORED` + `LIMESTONE827`).

Jevons defers `SwitchModel` until sidecar dated spool **`turn_end`**; **Claudia must seed switch from post-turn durable state** so answered prompts are not re-presented.

## Required Claudia change (`sidecar/maintain.ts`, `switchModel`)

Before switch compaction:

1. `this.history.clearInFlightAssistant()`
2. `this.persist(agent.state.messages)`
3. Run switch compaction with phase **`post_turn`** (not `pre_turn`)

## Exact patch (apply on Claudia release branch)

```diff
diff --git a/sidecar/maintain.ts b/sidecar/maintain.ts
--- a/sidecar/maintain.ts
+++ b/sidecar/maintain.ts
@@ -658,13 +658,18 @@ export class Maintenance {
       // Fail before folding when the chosen compactor has no plan auth (🎯T243).
       this.#compactorToken(compactor);
     }
+    // Pending T1371 switch lands between turns: fold from post-turn durable
+    // state so a source prompt that just received STORED is not re-presented.
+    this.history.clearInFlightAssistant();
+    this.persist(agent.state.messages);
+    const switchPhase: Phase = "post_turn";
     if (window > 0 && shouldCompact(this.contextTokens(agent.state.messages), window, settings)) {
       const durable = this.history.durableSnapshot();
       this.history.beginSwitchDurableBackup();
       const srcProvider = agent.state.model?.provider;
       let switchDiskCommitAttempted = false;
       try {
-        await this.run("switch", "pre_turn", agent.state.messages, {
+        await this.run("switch", switchPhase, agent.state.messages, {
           window,
           soft: true,
           deferPersist: true,
```

## Claudia test suggestion (claudia-po)

Hermetic in `sidecar/maintain.test.ts`: completed assistant turn with user prompt + `STORED` reply, pending switch, assert switch compaction uses **`post_turn`** and seeded messages do **not** treat the codeword prompt as unanswered (next user message gets reply **only** the new codeword, no `STORED` prefix).

## Jevons scope (worktree commit)

Defer pending switch via `switchpostturn.DeferApply`: sidecar + dated spool +
(in-flight **or** wired-terminal await until `boundTurnFromSidecarSpool` clears
await). Idle sidecar **without** spool history and `RecoverPendingSwitches`
apply immediately. Hermetic tests + journey runner post-switch oracle (reject
STORED-glued bodies). **No** `go.mod` Claudia pin bump until claudia-po release.
