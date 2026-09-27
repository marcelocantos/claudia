// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// unreadableItemError is a legacy Keychain item that is not plan JSON.
// The writer before the data file could leave one: security -i split an
// oversized -X line and applied only its first fragment.
type unreadableItemError struct {
	raw []byte
	err error
}

func (e *unreadableItemError) Error() string {
	return fmt.Sprintf("omp: keychain item is not the plan blob: %v", e.err)
}

func (e *unreadableItemError) Unwrap() error { return e.err }

// salvageItem keeps each provider record in a damaged legacy item that
// still decodes whole. security -w prints binary data as hex, so a value
// that is all hex is decoded first.
func salvageItem(raw []byte) Item {
	if b, err := hex.DecodeString(string(raw)); err == nil {
		raw = b
	}
	item := Item{Records: map[string]Record{}}
	for _, id := range PlanIDs {
		at := bytes.Index(raw, []byte(`"`+id+`":{`))
		if at < 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw[at+len(id)+3:]))
		var rec Record
		if err := dec.Decode(&rec); err != nil {
			continue
		}
		if rec.AccessToken == "" && rec.RefreshToken == "" {
			continue
		}
		item.Records[id] = rec
	}
	return item
}
