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
//
// 🎯T233.2: when a consumer state dir is configured but the intent file is
// missing or invalid, this fails closed for AutoStart resume rather than
// silently treating "could not read" as "nothing held" — resumeSeats
// checks consumerIntentFailClosed and resumes no AutoStart grant until a
// valid snapshot appears. A configured, valid file still only stands down
// the names it names; everything else resumes exactly as before.
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
		d.mu.Lock()
		d.consumerIntentFailClosed = true
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	d.consumerIntentFailClosed = false
	d.mu.Unlock()
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

// consumerPolicyHold re-reads the consumer's current fleet intent fresh
// (never from the cached claims ledger) and reports whether it currently
// denies launching name. A claim refresh, reattach, or Grant/Adopt cannot
// revive a name the consumer's file still parks: the only way to lift a
// hold is for the consumer to rewrite the file (🎯T233.2). failClosed is
// true when a consumer state dir is configured but the snapshot could not
// be read at all; in that case every name is denied, not just ones the
// (unreadable) file might have named.
func (d *Daemon) consumerPolicyHold(name string) (deny bool, failClosed bool) {
	path := consumerIntentPath(d.opts)
	if path == "" {
		return false, false
	}
	stood, present, err := claudia.ConsumerIntentSnapshot(path)
	if err != nil || !present {
		if err != nil {
			d.log.Warn("consumer fleet intent not read", "path", path, "err", err)
		}
		return true, true
	}
	return stood[name], false
}
