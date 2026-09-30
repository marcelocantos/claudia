// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// SeatHasHistory reports whether the dated spool holds conversation
// records for seat. Sidecar handshake lines are not history (🎯T153):
// an adopt-miss ("seat is not loaded") plus a later ready used to pass
// RequireResume and resume onto an empty conversation. Fail-closed
// resume reads this, not a vendor JSONL the sidecar does not write.
// An unreadable spool reads as no history.
func SeatHasHistory(dir, seat string) bool {
	ok, err := SeatHasHistoryContext(context.Background(), dir, seat)
	return ok && err == nil
}

// SeatHasHistoryContext is SeatHasHistory bounded by ctx, and it says why
// it could not answer.
//
// It runs on every RequireResume launch of a sidecar seat, so it has to be
// cheap against a spool of gigabytes (🎯T161: 2.5 GB after six days). It
// used to decode every line of every day — agent snapshots included — for
// each seat launched, which took 30 s at light load and 140-175 s during a
// broker restart, past the 90 s launch bound it ignored. Now it reads the
// newest day first, decodes only lines that name the seat, and stops at the
// first conversation record. A seat with no history still reads every day,
// but as a byte search.
func SeatHasHistoryContext(ctx context.Context, dir, seat string) (bool, error) {
	if dir == "" || seat == "" {
		return false, nil
	}
	names, err := listDayFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	token := seatToken(seat)
	for i := len(names) - 1; i >= 0; i-- {
		found, err := dayHasHistory(ctx, filepath.Join(dir, names[i]), seat, token)
		if found || err != nil {
			return found, err
		}
	}
	return false, nil
}

// spoolCtxLines is how many lines a spool search reads between looks at its
// context: often enough to stop within milliseconds, rarely enough to cost
// nothing.
const spoolCtxLines = 256

// dayHasHistory searches one day file for a conversation record of seat.
// Lines of any length are read; the old 8 MiB line limit ended a day's
// search at the first long line and read as "no history".
func dayHasHistory(ctx context.Context, path, seat string, token []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for n := 0; ; n++ {
		if n%spoolCtxLines == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long := append([]byte(nil), line...)
			rest, rerr := r.ReadBytes('\n')
			line, err = append(long, rest...), rerr
		}
		if len(line) > 0 && (token == nil || bytes.Contains(line, token)) && conversationRecord(line, seat) {
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// conversationRecord reports whether line is a spool record of seat that is
// not sidecar bookkeeping. Only seat and type are decoded.
func conversationRecord(line []byte, seat string) bool {
	var rec struct {
		Seat string `json:"seat"`
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return false
	}
	return rec.Seat == seat && !sidecarBookkeeping(SpoolRecord{Type: rec.Type})
}

// seatToken is the seat's name as a JSON string, which every record of the
// seat contains whoever wrote it, so a line without it is skipped undecoded.
// A name whose JSON spelling differs between writers (control characters,
// quotes, backslashes, anything outside printable ASCII) gets no token and
// every line is decoded.
func seatToken(seat string) []byte {
	for i := 0; i < len(seat); i++ {
		if c := seat[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return nil
		}
	}
	return []byte(`"` + seat + `"`)
}

// sidecarBookkeeping is a handshake or protocol event, not conversation
// content. "error" covers both "seat is not loaded" and turn failures:
// a real conversation also writes accepted/text/turn.
func sidecarBookkeeping(rec SpoolRecord) bool {
	switch rec.Type {
	case "ready", "dropped", "error":
		return true
	default:
		return false
	}
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
