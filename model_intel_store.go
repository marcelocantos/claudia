// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func appendJSONL(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("model intel: mkdir: %w", err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("model intel: append %s: %w", name, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("model intel: encode %s: %w", name, err)
	}
	return nil
}

func readObservations(dir string) ([]ModelObservation, error) {
	return readJSONL[ModelObservation](dir, modelIntelObsFile)
}

func readIntelRuns(dir string) ([]ModelIntelRun, error) {
	return readJSONL[ModelIntelRun](dir, modelIntelRunsFile)
}

func readJSONL[T any](dir, name string) ([]T, error) {
	path := filepath.Join(dir, name)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("model intel: read %s: %w", name, err)
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return nil, fmt.Errorf("model intel: parse %s: %w", name, err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("model intel: scan %s: %w", name, err)
	}
	return out, nil
}

func appendObservations(dir string, obs []ModelObservation) error {
	for i := range obs {
		if err := appendJSONL(dir, modelIntelObsFile, obs[i]); err != nil {
			return err
		}
	}
	return nil
}

func appendIntelRun(dir string, run ModelIntelRun) error {
	return appendJSONL(dir, modelIntelRunsFile, run)
}
