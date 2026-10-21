// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ClaimsFile is the broker claim ledger beside grants.json in the state dir.
const ClaimsFile = "claims.json"

const claimsReadVersion = 1

type claimsReadDoc struct {
	Version int                       `json:"version"`
	Seats   map[string]claimsReadSeat `json:"seats"`
}

type claimsReadSeat struct {
	StoodDown time.Time `json:"stood_down,omitzero"`
}

// StoodDownSeats returns grant names the claim ledger records as stood down
// by a consumer (park or reap). A missing file is an empty set.
func StoodDownSeats(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc claimsReadDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Version != claimsReadVersion {
		return nil, fmt.Errorf("%s: version %d, want %d", path, doc.Version, claimsReadVersion)
	}
	out := make(map[string]bool, len(doc.Seats))
	for name, seat := range doc.Seats {
		if !seat.StoodDown.IsZero() {
			out[name] = true
		}
	}
	return out, nil
}
