// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/marcelocantos/claudia"
)

// mergeConsumerIntentStandDown records parked/reaped agents from the
// consumer's durable fleet intent in the claim ledger before boot resume.
// This is a best-effort early record for logging and for claims-ledger
// bookkeeping (resumeOrder's "left" reporting); it is NOT what actually
// gates a launch. The gate is consumerIntentLiveSnapshot, re-read fresh at
// the point of each decision (🎯T233.2 follow-up): a consumer state dir
// can be configured well before resumeSeats actually runs (it waits on
// opts.resumeGate), and a snapshot cached only here would not see a
// writer that publishes or fixes its file in that window.
func (d *Daemon) mergeConsumerIntentStandDown() {
	path := consumerIntentPath(d.opts)
	if path == "" {
		return
	}
	stood, present, err := claudia.ConsumerIntentSnapshot(path)
	if err != nil || !present {
		if err != nil {
			d.log.Warn("consumer fleet intent not read; failing closed for AutoStart seats", "path", path, "err", err)
		} else {
			d.log.Warn("consumer fleet intent file missing; failing closed for AutoStart seats", "path", path)
		}
		return
	}
	if len(stood) == 0 {
		return
	}
	names := make([]string, 0, len(stood))
	for name := range stood {
		names = append(names, name)
	}
	d.standDown(names...)
	d.log.Info("stood down seats from consumer fleet intent", "path", path, "count", len(names), "grants", strings.Join(names, ","))
}

func consumerIntentPath(opts Options) string {
	root := strings.TrimSpace(opts.ConsumerStateDir)
	if root == "" {
		root = strings.TrimSpace(os.Getenv("CLAUDIA_CONSUMER_STATE_DIR"))
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, claudia.ConsumerIntentRelPath)
}

// consumerIntentLiveSnapshot re-reads the consumer's current fleet intent
// fresh — never from a cached field, never from the claims ledger — so
// every caller sees the same live fact regardless of how long it has been
// since boot or the last grant (🎯T233.2). failClosed is true when a
// consumer state dir is configured but the snapshot could not be read at
// all (missing file, invalid JSON, missing/null "agents" key, oversize);
// in that case held is nil and every name must be treated as denied. An
// unconfigured path (no consumer state dir at all) returns an empty held
// set and failClosed=false: legacy behaviour, nothing enforced.
func (d *Daemon) consumerIntentLiveSnapshot() (held map[string]bool, failClosed bool) {
	path := consumerIntentPath(d.opts)
	if path == "" {
		return map[string]bool{}, false
	}
	stood, present, err := claudia.ConsumerIntentSnapshot(path)
	if err != nil || !present {
		if err != nil {
			d.log.Warn("consumer fleet intent not read", "path", path, "err", err)
		}
		return nil, true
	}
	return stood, false
}

// consumerPolicyHold reports whether the consumer's CURRENT (freshly read)
// fleet intent denies launching name right now. A claim refresh,
// reattach, or Grant/Adopt cannot revive a name the consumer's file still
// parks: the only way to lift a hold is for the consumer to rewrite the
// file (🎯T233.2). failClosed is true when a consumer state dir is
// configured but the snapshot could not be read at all; in that case
// every name is denied, not just ones the (unreadable) file might have
// named.
func (d *Daemon) consumerPolicyHold(name string) (deny bool, failClosed bool) {
	held, failClosed := d.consumerIntentLiveSnapshot()
	if failClosed {
		return true, true
	}
	return held[name], false
}
