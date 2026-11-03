// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// openLogLocked opens a fleet's event log for appending and loads its tail. A
// final line without a newline is a write a crash cut short, so it is cut off.
// Any other damage is an error. k.mu held.
func (k *FleetKernel) openLogLocked(fleet string) (*fleetLog, error) {
	path := filepath.Join(k.dir, fleet+".events.jsonl")
	log := &fleetLog{path: path}
	var good int64
	err := scanFleetLog(path, func(e *FleetEvent, end int64) bool {
		log.seq = e.Seq
		good = end
		log.recent = append(log.recent, *e)
		if len(log.recent) > fleetEventRing {
			log.recent = log.recent[1:]
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("fleet kernel: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fleet kernel: %w", err)
	}
	if err := file.Truncate(good); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("fleet kernel: %w", err)
	}
	log.file = file
	return log, nil
}

// scanFleetLog calls visit with each whole event and the offset just past it,
// until visit returns false. A missing file has no events.
func scanFleetLog(path string, visit func(e *FleetEvent, end int64) bool) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		offset += int64(len(line))
		var e FleetEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("damaged event log %s at offset %d: %w", path, offset, err)
		}
		if !visit(&e, offset) {
			return nil
		}
	}
}

// fleetEventBatch collects events up to the size one read may return.
type fleetEventBatch struct {
	events []FleetEvent
	size   int
}

// take reports whether the batch wants more after e.
func (b *fleetEventBatch) take(e *FleetEvent) bool {
	raw, err := json.Marshal(e)
	if err != nil {
		return true
	}
	if len(b.events) > 0 && b.size+len(raw) > fleetReadBytes {
		return false
	}
	b.events = append(b.events, *e)
	b.size += len(raw)
	return true
}

// recentAfter answers from memory, and reports false if seq is older than that.
func (l *fleetLog) recentAfter(seq uint64) ([]FleetEvent, bool) {
	if seq >= l.seq {
		return nil, true
	}
	if len(l.recent) == 0 || l.recent[0].Seq > seq+1 {
		return nil, false
	}
	var batch fleetEventBatch
	for i := range l.recent {
		if l.recent[i].Seq > seq && !batch.take(&l.recent[i]) {
			break
		}
	}
	return batch.events, true
}

// scanFleetEvents answers from the file for a reader further behind. A line a
// writer is still appending has no newline yet, so the scan stops before it.
func scanFleetEvents(path string, seq uint64) ([]FleetEvent, error) {
	var batch fleetEventBatch
	err := scanFleetLog(path, func(e *FleetEvent, _ int64) bool {
		if e.Seq <= seq {
			return true
		}
		return batch.take(e)
	})
	return batch.events, err
}
