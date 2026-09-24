// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// SpoolDir is ~/.jevons/spool, or JEVONS_SPOOL_DIR when set (tests).
func SpoolDir() string {
	if p := strings.TrimSpace(os.Getenv("JEVONS_SPOOL_DIR")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".jevons", "spool")
}

// SpoolRecord is one sidecar spool line. Each names its seat.
type SpoolRecord struct {
	TS       string          `json:"ts"`
	Seat     string          `json:"seat"`
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	CallID   string          `json:"call_id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
}

// SeatHasHistory reports whether the dated spool holds any record for seat.
// Fail-closed resume reads this, not a vendor JSONL the sidecar does not write.
func SeatHasHistory(dir, seat string) bool {
	recs, err := ReadSeat(dir, seat)
	return err == nil && len(recs) > 0
}

// ReadSeat returns every record for seat, older dates first.
func ReadSeat(dir, seat string) ([]SpoolRecord, error) {
	if dir == "" || seat == "" {
		return nil, nil
	}
	names, err := listDayFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SpoolRecord
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var rec SpoolRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if rec.Seat == seat {
				out = append(out, rec)
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

func listDayFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "events-") && strings.HasSuffix(n, ".log") {
			names = append(names, n)
		}
	}
	// Directory order from ReadDir is sorted. Older dates first.
	return names, nil
}
