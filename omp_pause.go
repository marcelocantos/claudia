// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"log/slog"

	"github.com/marcelocantos/claudia/omp"
)

// SetOMPAgentPause engages or releases the Oh My Pi process-global pause gate
// (🎯T966). One OpPause/OpResume per live sidecar connection is enough: the
// sidecar holds every OMP seat in that process.
func SetOMPAgentPause(paused bool) error {
	op := omp.OpPause
	if !paused {
		op = omp.OpResume
	}
	var sent bool
	var lastErr error
	ompSeats.Range(func(k, _ any) bool {
		c := k.(*ompControl)
		if sent {
			return true
		}
		if err := c.send(omp.Message{Op: op}); err != nil {
			lastErr = err
			slog.Warn("omp fleet pause wire failed", "seat", c.seat, "op", op, "err", err)
			return true
		}
		sent = true
		return true
	})
	if !sent && lastErr != nil {
		return lastErr
	}
	return nil
}
