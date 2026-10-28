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
		d.log.Warn("consumer fleet intent and/or policy-hold snapshot unavailable; failing closed for AutoStart seats")
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
// consumer state dir as fleet/intent.json (🎯T233.2 final owner schema):
// one root, two files with distinct, non-overlapping meanings — genuine
// parked/reaped intent vs. transient policy-held autos.
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
// last grant (🎯T233.2 final owner schema). failClosed is true when a
// consumer state dir is configured but EITHER file could not be read at
// all (missing, invalid, or schema-violating); in that case held is nil
// and every name must be treated as denied — a writer that gets one file
// right and the other wrong is not "half safe". An unconfigured path (no
// consumer state dir at all) returns an empty held set and
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
	policyHeld, policyPresent, policyErr := claudia.PolicyHoldSnapshot(holdPath)
	if policyErr != nil {
		d.log.Warn("consumer policy-hold not read", "path", holdPath, "err", policyErr)
	}
	if !intentPresent || !policyPresent {
		return nil, true
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
// rewrite the relevant file (🎯T233.2). failClosed is true when a
// consumer state dir is configured but either snapshot could not be read
// at all; in that case every name is denied, not just ones a readable
// file might have named.
func (d *Daemon) consumerPolicyHold(name string) (deny bool, failClosed bool) {
	held, failClosed := d.consumerHoldLiveSnapshot()
	if failClosed {
		return true, true
	}
	return held[name], false
}
