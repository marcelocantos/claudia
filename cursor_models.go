// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed cursor_models_fallback.json
var cursorModelsFallbackJSON []byte

// CursorModelSource names where a [CursorModelCatalog] came from.
type CursorModelSource string

const (
	// CursorModelSourceCache is a previously refreshed on-disk catalog.
	CursorModelSourceCache CursorModelSource = "cache"
	// CursorModelSourceEmbedded is the library-shipped fallback snapshot.
	CursorModelSourceEmbedded CursorModelSource = "embedded"
	// CursorModelSourceCLI is a fresh `agent models` / `--list-models` run.
	CursorModelSourceCLI CursorModelSource = "cli"
)

const cursorModelsCacheFile = "cursor-models.json"

// CursorModel is one Cursor Agent model id suitable for [Config.Model] /
// Task --model.
type CursorModel struct {
	// ID is the value passed to --model (e.g. "claude-opus-5-high").
	ID string `json:"id"`
	// Label is the human-readable name from the CLI list.
	Label string `json:"label,omitempty"`
	// Default is true when the CLI marks this id as the account default.
	Default bool `json:"default,omitempty"`
}

// CursorModelCatalog is an offline-first list of Cursor model ids.
// Prefer reading this for pickers; refresh rarely via [ListCursorModelsArgs.Refresh]
// or [ListCursorModelsArgs.MaxAge]. Session/Task selection remains
// [Config.Model] / TaskConfig.Model at process start.
type CursorModelCatalog struct {
	Models []CursorModel `json:"models"`
	// Source is cache, embedded, or cli.
	Source CursorModelSource `json:"source"`
	// FetchedAt is when the catalog was last obtained from the CLI
	// (zero for the baked-in embedded snapshot).
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	// CachePath is the on-disk cache file when Source is cache or after a
	// successful refresh. Empty when only the embedded snapshot was used.
	CachePath string `json:"cache_path,omitempty"`
	// Stale is true when Source is cache and MaxAge would have preferred a
	// refresh, but the CLI was not run (Refresh/MaxAge not requesting it,
	// or refresh failed and the cache was returned).
	Stale bool `json:"stale,omitempty"`
}

type cursorModelsCacheFileV1 struct {
	FetchedAt time.Time     `json:"fetched_at"`
	Models    []CursorModel `json:"models"`
}

// ListCursorModelsArgs configures [ListCursorModels].
// A nil args value means fully offline: return cache if present, else the
// embedded snapshot — never spawn the Cursor agent CLI.
type ListCursorModelsArgs struct {
	// Refresh forces a CLI refresh and rewrites the cache.
	Refresh bool
	// MaxAge triggers a CLI refresh when the cache is older than this.
	// Zero (the default) never auto-refreshes.
	MaxAge time.Duration
	// CachePath overrides the cache file location (tests / host-owned paths).
	// Empty uses $XDG_CACHE_HOME/claudia/cursor-models.json (or OS cache dir).
	CachePath string
	// Now overrides wall clock (tests). Zero uses time.Now.
	Now time.Time
	// RunModels, when set, replaces the `agent models` subprocess (tests).
	RunModels func(ctx context.Context) ([]byte, error)
}

// ListCursorModels returns Cursor model ids for pickers and Config.Model.
//
// Offline-first: by default this never runs the agent CLI. It returns the
// on-disk cache when present, otherwise the embedded snapshot shipped with
// this library. Pass Refresh: true (or a MaxAge) to update the cache from
// `agent models`.
func ListCursorModels(ctx context.Context, args *ListCursorModelsArgs) (*CursorModelCatalog, error) {
	if args == nil {
		args = &ListCursorModelsArgs{}
	}
	now := args.Now
	if now.IsZero() {
		now = time.Now()
	}
	cachePath := args.CachePath
	if cachePath == "" {
		var err error
		cachePath, err = defaultCursorModelsCachePath()
		if err != nil {
			return nil, err
		}
	}

	cached, err := readCursorModelsCache(cachePath)
	if err != nil {
		// Corrupt cache: treat as miss and continue offline / refresh.
		cached = nil
	}
	needRefresh := args.Refresh
	if cached != nil && args.MaxAge > 0 && now.Sub(cached.FetchedAt) > args.MaxAge {
		needRefresh = true
	}
	if !needRefresh {
		if cached != nil {
			return &CursorModelCatalog{
				Models:    cached.Models,
				Source:    CursorModelSourceCache,
				FetchedAt: cached.FetchedAt,
				CachePath: cachePath,
			}, nil
		}
		models, err := parseEmbeddedCursorModels()
		if err != nil {
			return nil, err
		}
		return &CursorModelCatalog{
			Models: models,
			Source: CursorModelSourceEmbedded,
		}, nil
	}

	raw, err := runCursorModelsCLI(ctx, args)
	if err != nil {
		if cached != nil {
			return &CursorModelCatalog{
				Models:    cached.Models,
				Source:    CursorModelSourceCache,
				FetchedAt: cached.FetchedAt,
				CachePath: cachePath,
				Stale:     true,
			}, fmt.Errorf("refresh cursor models: %w (returning stale cache)", err)
		}
		models, embErr := parseEmbeddedCursorModels()
		if embErr != nil {
			return nil, fmt.Errorf("refresh cursor models: %w", err)
		}
		return &CursorModelCatalog{
			Models: models,
			Source: CursorModelSourceEmbedded,
			Stale:  true,
		}, fmt.Errorf("refresh cursor models: %w (returning embedded snapshot)", err)
	}
	models, err := parseCursorModelsList(raw)
	if err != nil {
		return nil, err
	}
	if err := writeCursorModelsCache(cachePath, cursorModelsCacheFileV1{
		FetchedAt: now,
		Models:    models,
	}); err != nil {
		return &CursorModelCatalog{
			Models:    models,
			Source:    CursorModelSourceCLI,
			FetchedAt: now,
		}, fmt.Errorf("cursor models refreshed but cache write failed: %w", err)
	}
	return &CursorModelCatalog{
		Models:    models,
		Source:    CursorModelSourceCLI,
		FetchedAt: now,
		CachePath: cachePath,
	}, nil
}

func defaultCursorModelsCachePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cursor models cache: %w", err)
	}
	return filepath.Join(dir, "claudia", cursorModelsCacheFile), nil
}

func runCursorModelsCLI(ctx context.Context, args *ListCursorModelsArgs) ([]byte, error) {
	if args.RunModels != nil {
		return args.RunModels(ctx)
	}
	bin, err := resolveCursorBin()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "models")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s models: %s", bin, msg)
	}
	return stdout.Bytes(), nil
}

func readCursorModelsCache(path string) (*cursorModelsCacheFileV1, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc cursorModelsCacheFileV1
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("cursor models cache %s: %w", path, err)
	}
	if len(doc.Models) == 0 {
		return nil, nil
	}
	return &doc, nil
}

func writeCursorModelsCache(path string, doc cursorModelsCacheFileV1) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseEmbeddedCursorModels() ([]CursorModel, error) {
	var doc struct {
		Models []CursorModel `json:"models"`
	}
	if err := json.Unmarshal(cursorModelsFallbackJSON, &doc); err != nil {
		return nil, fmt.Errorf("embedded cursor models: %w", err)
	}
	if len(doc.Models) == 0 {
		return nil, fmt.Errorf("embedded cursor models: empty")
	}
	return doc.Models, nil
}

// parseCursorModelsList parses `agent models` / `agent --list-models` stdout.
// Lines look like: "auto - Auto (default)".
func parseCursorModelsList(raw []byte) ([]CursorModel, error) {
	var out []CursorModel
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Available") || strings.HasPrefix(line, "Tip:") {
			continue
		}
		id, label, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		id = strings.TrimSpace(id)
		label = strings.TrimSpace(label)
		if id == "" || strings.Contains(id, " ") {
			continue
		}
		def := false
		if strings.Contains(strings.ToLower(label), "(default)") {
			def = true
			label = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(label, "(default)", ""), "(Default)", ""))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, CursorModel{ID: id, Label: label, Default: def})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cursor models: no entries parsed")
	}
	return out, nil
}
