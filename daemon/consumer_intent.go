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
func (d *Daemon) mergeConsumerIntentStandDown() {
	path := consumerIntentPath(d.opts)
	if path == "" {
		return
	}
	stood, err := claudia.StoodDownFromConsumerIntent(path)
	if err != nil {
		d.log.Warn("consumer fleet intent not read", "path", path, "err", err)
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
