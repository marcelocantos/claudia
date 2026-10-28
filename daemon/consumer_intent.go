// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/marcelocantos/claudia"
)

// mergeConsumerIntentStandDown records parked/reaped agents AND
// policy-held autos from the consumer's two durable state files
// (fleet/intent.json, fleet/policy-hold.json) in the claim ledger before
// boot resume. This is a best-effort early record for logging and for
// claims-ledger bookkeeping (resumeOrder's "left" reporting); it is NOT
// what actually gates a launch. The gate is consumerHoldLiveSnapshot,
// re-read fresh at the point of each decision (🎯T233.2): a consumer
// state dir can be configured well before resumeSeats actually runs (it
// waits on opts.resumeGate), and a snapshot cached only here would not
// see a writer that publishes or fixes either file in that window.
func (d *Daemon) mergeConsumerIntentStandDown() {
	if consumerIntentPath(d.opts) == "" && policyHoldPath(d.opts) == "" {
		return
	}
	held, failClosed := d.consumerHoldLiveSnapshot()
	if failClosed {
		d.log.Warn("consumer fleet intent and/or policy-hold snapshot unavailable or malformed; failing closed for AutoStart seats")
		return
	}
	if len(held) == 0 {
		return
	}
	names := make([]string, 0, len(held))
	for name := range held {
		names = append(names, name)
	}
	d.standDown(names...)
	d.log.Info("stood down seats from consumer fleet intent/policy-hold", "count", len(names), "grants", strings.Join(names, ","))
}

func consumerIntentPath(opts Options) string {
	return consumerStatePath(opts, claudia.ConsumerIntentRelPath)
}

// policyHoldPath is the Jevons-owned policy-hold file under the same
// consumer state dir as fleet/intent.json (🎯T233.2 owner decision
// 2026-10-28: TWO_FILES): one root, two files with distinct,
// non-overlapping meanings — genuine parked/reaped intent vs. transient
// policy-held autos.
func policyHoldPath(opts Options) string {
	return consumerStatePath(opts, claudia.PolicyHoldRelPath)
}

func consumerStatePath(opts Options, relPath string) string {
	root := strings.TrimSpace(opts.ConsumerStateDir)
	if root == "" {
		root = strings.TrimSpace(os.Getenv("CLAUDIA_CONSUMER_STATE_DIR"))
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, relPath)
}

// consumerHoldLiveSnapshot re-reads BOTH of the consumer's current state
// files fresh — fleet/intent.json (genuine parked/reaped) and
// fleet/policy-hold.json (transient policy-held autos) — never from a
// cached field, never from the claims ledger, so every caller sees the
// same live union regardless of how long it has been since boot or the
// last grant (🎯T233.2).
//
// The two files are NOT symmetric (owner decision 2026-10-28):
//
//   - fleet/intent.json missing or malformed: fail closed for every
//     AutoStart name — unchanged from before. This file's whole job is
//     naming stand-downs; the broker cannot tell "nothing parked" from
//     "couldn't read it" without the file saying so.
//   - fleet/policy-hold.json MISSING (no file at that path at all):
//     treated as "no transient holds right now" — NOT fail closed. A
//     consumer that has never needed to policy-hold anything, or whose
//     writer has not started yet, does not thereby block every AutoStart
//     seat in the fleet.
//   - fleet/policy-hold.json MALFORMED (present but invalid — bad JSON,
//     wrong version/kind, a rejected reason, a duplicate name, oversize,
//     …): fail closed for every AutoStart name, same as an
//     intent.json problem. A file that exists and claims to be the
//     schema but isn't is a writer bug, not silence, and is never read
//     as "nothing held".
//
// failClosed=true means held is nil and every name must be denied,
// whatever category it is — this registry has no "auto" vs "named/PO"
// distinction on a grant beyond its AutoStart bool, and every grant
// resumeSeats or Grant/Adopt considers here carries that bool, so a
// fail-closed verdict from a malformed intent.json or a malformed
// policy-hold.json denies ALL of them, named/PO included. An unconfigured
// path (no consumer state dir at all) returns an empty held set and
// failClosed=false: legacy behaviour, nothing enforced.
func (d *Daemon) consumerHoldLiveSnapshot() (held map[string]bool, failClosed bool) {
	intentPath := consumerIntentPath(d.opts)
	holdPath := policyHoldPath(d.opts)
	if intentPath == "" && holdPath == "" {
		return map[string]bool{}, false
	}
	stood, intentPresent, intentErr := claudia.ConsumerIntentSnapshot(intentPath)
	if intentErr != nil {
		d.log.Warn("consumer fleet intent not read", "path", intentPath, "err", intentErr)
	}
	if intentErr != nil || !intentPresent {
		// fleet/intent.json missing or invalid: unchanged fail-closed
		// policy, independent of fleet/policy-hold.json's state.
		return nil, true
	}

	policyHeld, policyPresent, policyErr := claudia.PolicyHoldSnapshot(holdPath)
	if policyErr != nil {
		// Malformed, not missing: the file exists and claims to be a
		// policy-hold snapshot but fails to parse as one. Fail closed —
		// never read as "nothing held".
		d.log.Warn("consumer policy-hold malformed; failing closed for AutoStart seats", "path", holdPath, "err", policyErr)
		return nil, true
	}
	if !policyPresent {
		// Missing, not malformed: no writer has published a
		// policy-hold.json at all (yet, or ever). That is "no transient
		// holds right now", not a reason to deny every AutoStart seat.
		policyHeld = map[string]string{}
	}

	out := make(map[string]bool, len(stood)+len(policyHeld))
	for name := range stood {
		out[name] = true
	}
	for name := range policyHeld {
		out[name] = true
	}
	return out, false
}

// consumerPolicyHold reports whether the consumer's CURRENT (freshly
// read, both files, union) state denies launching name right now. A
// claim refresh, reattach, or Grant/Adopt cannot revive a name either
// file still holds: the only way to lift a hold is for the consumer to
// rewrite the relevant file (🎯T233.2). failClosed is true when
// fleet/intent.json is missing/invalid, or fleet/policy-hold.json is
// present but malformed; in that case every name is denied. A MISSING
// fleet/policy-hold.json alone does not set failClosed — see
// consumerHoldLiveSnapshot.
func (d *Daemon) consumerPolicyHold(name string) (deny bool, failClosed bool) {
	held, failClosed := d.consumerHoldLiveSnapshot()
	if failClosed {
		return true, true
	}
	return held[name], false
}
