// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const cursorModelsFixture = `Available models

auto - Auto (default)
gpt-5.3-codex - Codex 5.3
claude-opus-5-high - Claude Opus 5 1M

Tip: use --model <id> to switch.
`

func TestParseCursorModelsList(t *testing.T) {
	models, err := parseCursorModelsList([]byte(cursorModelsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(models), models)
	}
	if models[0].ID != "auto" || !models[0].Default || models[0].Label != "Auto" {
		t.Fatalf("auto = %+v", models[0])
	}
	if models[1].ID != "gpt-5.3-codex" || models[1].Default {
		t.Fatalf("codex = %+v", models[1])
	}
}

func TestListCursorModelsOfflineUsesEmbeddedWhenNoCache(t *testing.T) {
	dir := t.TempDir()
	cat, err := ListCursorModels(context.Background(), &ListCursorModelsArgs{
		CachePath: filepath.Join(dir, "missing.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cat.Source != CursorModelSourceEmbedded {
		t.Fatalf("Source = %q, want embedded", cat.Source)
	}
	if len(cat.Models) < 10 {
		t.Fatalf("embedded catalog too small: %d", len(cat.Models))
	}
	if cat.Models[0].ID == "" {
		t.Fatal("empty model id")
	}
}

func TestListCursorModelsUsesCacheWithoutCLI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-models.json")
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := writeCursorModelsCache(path, cursorModelsCacheFileV1{
		FetchedAt: now,
		Models:    []CursorModel{{ID: "cached-model", Label: "Cached"}},
	}); err != nil {
		t.Fatal(err)
	}
	cliCalls := 0
	cat, err := ListCursorModels(context.Background(), &ListCursorModelsArgs{
		CachePath: path,
		Now:       now.Add(48 * time.Hour),
		RunModels: func(context.Context) ([]byte, error) {
			cliCalls++
			return nil, errors.New("should not run")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cliCalls != 0 {
		t.Fatalf("CLI called %d times", cliCalls)
	}
	if cat.Source != CursorModelSourceCache || len(cat.Models) != 1 || cat.Models[0].ID != "cached-model" {
		t.Fatalf("catalog = %+v", cat)
	}
}

func TestListCursorModelsRefreshWritesCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-models.json")
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	cat, err := ListCursorModels(context.Background(), &ListCursorModelsArgs{
		Refresh:   true,
		CachePath: path,
		Now:       now,
		RunModels: func(context.Context) ([]byte, error) {
			return []byte(cursorModelsFixture), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cat.Source != CursorModelSourceCLI || !cat.FetchedAt.Equal(now) {
		t.Fatalf("catalog = %+v", cat)
	}
	if len(cat.Models) != 3 || cat.Models[0].ID != "auto" {
		t.Fatalf("models = %+v", cat.Models)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc cursorModelsCacheFileV1
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Models) != 3 || !doc.FetchedAt.Equal(now) {
		t.Fatalf("cache = %+v", doc)
	}

	again, err := ListCursorModels(context.Background(), &ListCursorModelsArgs{CachePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if again.Source != CursorModelSourceCache || again.Models[0].ID != "auto" {
		t.Fatalf("reread = %+v", again)
	}
}

func TestListCursorModelsMaxAgeTriggersRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-models.json")
	fetched := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := writeCursorModelsCache(path, cursorModelsCacheFileV1{
		FetchedAt: fetched,
		Models:    []CursorModel{{ID: "old"}},
	}); err != nil {
		t.Fatal(err)
	}
	now := fetched.Add(10 * 24 * time.Hour)
	cat, err := ListCursorModels(context.Background(), &ListCursorModelsArgs{
		CachePath: path,
		MaxAge:    7 * 24 * time.Hour,
		Now:       now,
		RunModels: func(context.Context) ([]byte, error) {
			return []byte("fresh - Fresh Model\n"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cat.Source != CursorModelSourceCLI || cat.Models[0].ID != "fresh" {
		t.Fatalf("catalog = %+v", cat)
	}
}

func TestListCursorModelsRefreshFailureFallsBackToCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-models.json")
	fetched := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := writeCursorModelsCache(path, cursorModelsCacheFileV1{
		FetchedAt: fetched,
		Models:    []CursorModel{{ID: "stale-ok"}},
	}); err != nil {
		t.Fatal(err)
	}
	cat, err := ListCursorModels(context.Background(), &ListCursorModelsArgs{
		Refresh:   true,
		CachePath: path,
		RunModels: func(context.Context) ([]byte, error) {
			return nil, errors.New("cli down")
		},
	})
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if cat == nil || cat.Source != CursorModelSourceCache || !cat.Stale || cat.Models[0].ID != "stale-ok" {
		t.Fatalf("catalog = %+v err=%v", cat, err)
	}
}
