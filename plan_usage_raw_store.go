// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// 🎯T84: the last several payloads per provider, on disk, so a question
// about what the server actually said is answered from the store rather
// than from the network.
//
// [planRecorder] already carries one body home on the reading, which is
// what a consumer persists (jevons 🎯T683). That covers the caller who
// asked. It does not cover the caller who did not: `go run ./cmd/usage`
// prints percentages and forgets, and the daemon's monitor keeps only the
// parsed snapshot. So the body is also written here, beside the request
// floor, where any process on this host can read it back.
//
// Retention prefers the interesting payload. A response carrying keys no
// parser maps is the one that answers "when did the surface change?" —
// Anthropic's per-model weekly windows travelled in every response for
// weeks while the product reported no such figure existed — so a payload
// with unmapped keys outlives one that parsed cleanly.

const (
	// PlanRawPayloadsPerProvider is the ring depth per provider: enough to
	// see a change and what preceded it, small enough that the store stays
	// a few tens of kilobytes.
	PlanRawPayloadsPerProvider = 8

	planRawStoreDir = "raw"
)

// PlanRawPayload is one retained vendor response.
type PlanRawPayload struct {
	Provider  Provider  `json:"provider"`
	FetchedAt time.Time `json:"fetched_at"`
	// HTTPStatus is the status the body arrived with.
	HTTPStatus int `json:"http_status"`
	// Body is the response, already bounded by PlanRawBodyLimit.
	Body string `json:"body"`
	// UnmappedKeys names the JSON paths in Body that this package's parser
	// for the provider does not read. It is computed only for a 2xx body:
	// an error page is not a surface change, and treating one as novel
	// would let a refusal evict the readings worth keeping.
	UnmappedKeys []string `json:"unmapped_keys,omitempty"`
}

type planRawDoc struct {
	// Payloads are oldest first.
	Payloads []PlanRawPayload `json:"payloads"`
}

// planRawMu serialises this process's read-modify-write of one file.
// Across processes the file races, and a lost payload costs one slot of
// history — not worth a lock file on a path this cold.
var planRawMu sync.Mutex

func planRawPath(dir string, p Provider) (string, error) {
	root := filepath.Join(dir, planRawStoreDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(root, string(p)+".json"), nil
}

// RecordPlanRawPayload appends one response to provider p's ring under
// dir. An empty dir or body records nothing. Failures are silent: losing
// a diagnostic payload must never fail a plan-usage read.
func RecordPlanRawPayload(dir string, p Provider, at time.Time, status int, body string) {
	if dir == "" || strings.TrimSpace(body) == "" {
		return
	}
	if len(body) > PlanRawBodyLimit {
		body = body[:PlanRawBodyLimit]
	}
	path, err := planRawPath(dir, p)
	if err != nil {
		return
	}
	entry := PlanRawPayload{Provider: p, FetchedAt: at, HTTPStatus: status, Body: body}
	if status >= 200 && status < 300 {
		entry.UnmappedKeys = unmappedPlanKeys(p, []byte(body))
	}

	planRawMu.Lock()
	defer planRawMu.Unlock()
	doc := readPlanRawDoc(path)
	doc.Payloads = append(evictForPlanRawPayload(doc.Payloads), entry)
	_ = writePlanRawDoc(path, doc)
}

// evictForPlanRawPayload makes room for one more entry. The oldest
// cleanly-parsed payload goes first; only when every retained payload
// carries unmapped keys does the ring give up its oldest outright.
func evictForPlanRawPayload(in []PlanRawPayload) []PlanRawPayload {
	if len(in) < PlanRawPayloadsPerProvider {
		return in
	}
	drop := 0
	for i, e := range in {
		if len(e.UnmappedKeys) == 0 {
			drop = i
			break
		}
	}
	return append(in[:drop:drop], in[drop+1:]...)
}

// ReadPlanRawPayloads returns provider p's retained payloads under dir,
// oldest first. It issues no request; that is the point.
func ReadPlanRawPayloads(dir string, p Provider) []PlanRawPayload {
	if dir == "" {
		return nil
	}
	return readPlanRawDoc(filepath.Join(dir, planRawStoreDir, string(p)+".json")).Payloads
}

func readPlanRawDoc(path string) planRawDoc {
	var doc planRawDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		return planRawDoc{}
	}
	// A corrupt file starts again rather than taking plan usage down; the
	// store is evidence, never a dependency.
	if err := json.Unmarshal(raw, &doc); err != nil {
		return planRawDoc{}
	}
	return doc
}

func writePlanRawDoc(path string, doc planRawDoc) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// planParserShapes names the struct each provider's parser unmarshals
// into. The known key paths are read off these types rather than from a
// hand-kept list, so a field the parser stops reading stops counting as
// mapped in the same commit.
var planParserShapes = map[Provider]any{
	ProviderClaude: claudeOAuthUsage{},
	ProviderCodex:  codexWhamUsage{},
	ProviderGrok:   grokBillingConfig{},
	ProviderCursor: cursorPeriodUsage{},
}

// unmappedPlanKeys returns the JSON paths in body that p's parser does
// not read, shallowest first and without their children — a whole
// unmapped object is reported once, by its own name.
//
// A provider with no registered shape, or a body that is not JSON,
// returns nothing: unmapped is a claim about a parser, and an absent
// parser supports no claim.
func unmappedPlanKeys(p Provider, body []byte) []string {
	shape, ok := planParserShapes[p]
	if !ok {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	known := map[string]bool{}
	collectMappedPaths(reflect.TypeOf(shape), "", known)
	seen := map[string]bool{}
	var out []string
	collectBodyPaths(v, "", func(path string) bool {
		if known[path] {
			return true // keep walking into a mapped subtree
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
		return false // an unmapped node is named once, not enumerated
	})
	sort.Strings(out)
	return out
}

var jsonRawMessageType = reflect.TypeOf(json.RawMessage(nil))

// collectMappedPaths walks a parser's struct type and records the JSON
// path of every field it declares. A slice contributes "p[]" and its
// element's paths below that; a json.RawMessage is a leaf, since the
// parser takes the whole value.
func collectMappedPaths(t reflect.Type, prefix string, out map[string]bool) {
	if t == nil {
		return
	}
	if t == jsonRawMessageType {
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		collectMappedPaths(t.Elem(), prefix, out)
	case reflect.Slice, reflect.Array:
		elem := prefix + "[]"
		out[elem] = true
		collectMappedPaths(t.Elem(), elem, out)
	case reflect.Map:
		// Any key here is read by the parser.
		out[prefix+".*"] = true
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			out[path] = true
			collectMappedPaths(f.Type, path, out)
		}
	}
}

// collectBodyPaths walks decoded JSON and offers each path to visit,
// which reports whether to descend.
func collectBodyPaths(v any, prefix string, visit func(path string) bool) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if visit(path) {
				collectBodyPaths(val[k], path, visit)
			}
		}
	case []any:
		path := prefix + "[]"
		if !visit(path) {
			return
		}
		for _, e := range val {
			collectBodyPaths(e, path, visit)
		}
	}
}
