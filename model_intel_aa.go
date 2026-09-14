// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const aaModelsFreeURL = "https://artificialanalysis.ai/api/v2/language/models/free"

type aaEnvelope struct {
	Data []aaModel `json:"data"`
}

type aaModel struct {
	Name        string          `json:"name"`
	Slug        string          `json:"slug"`
	Evaluations json.RawMessage `json:"evaluations"`
	Cost        json.RawMessage `json:"artificial_analysis_intelligence_index_cost"`
	IndexVer    json.RawMessage `json:"intelligence_index_version"`
}

func fetchAAModels(ctx context.Context, args *ModelIntelArgs) ([]byte, error) {
	if args != nil && args.FetchAA != nil {
		return args.FetchAA()
	}
	key := ""
	if args != nil {
		key = strings.TrimSpace(args.APIKey)
	}
	if key == "" {
		key = strings.TrimSpace(os.Getenv(modelIntelEnvAAKey))
	}
	if key == "" {
		return nil, fmt.Errorf("aa: %s is not set", modelIntelEnvAAKey)
	}
	client := http.DefaultClient
	if args != nil && args.HTTPClient != nil {
		client = args.HTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aaModelsFreeURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aa: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("aa: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aa: HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func observationsFromAA(raw []byte, now time.Time) ([]ModelObservation, error) {
	var env aaEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("aa: decode: %w", err)
	}
	if env.Data == nil {
		// Some responses are a bare array.
		if err := json.Unmarshal(raw, &env.Data); err != nil {
			return nil, fmt.Errorf("aa: decode data: %w", err)
		}
	}
	var out []ModelObservation
	for _, m := range env.Data {
		evals := aaEvalMap(m.Evaluations)
		rev := aaIndexVersion(m.IndexVer, evals)
		cost := aaCostUSD(m.Cost, evals)
		rawID := m.Slug
		if rawID == "" {
			rawID = m.Name
		}
		gen, effort := ParseModelSlug(m.Slug)
		if g2, e2 := ParseModelSlug(m.Name); g2 != "" {
			if gen == "" {
				gen = g2
			}
			if effort == ModelEffortUnspecified {
				effort = e2
			}
		}
		if row, ok := MatchCatalogGeneration(gen); ok {
			gen = row.Model
		}
		if gen == "" {
			continue
		}
		add := func(purpose ModelPurpose, key string) {
			v, ok := evals[key]
			if !ok {
				return
			}
			out = append(out, ModelObservation{
				ObservedAt: now,
				Source:     modelIntelSourceAA,
				SourceRev:  rev,
				Generation: gen,
				Effort:     effort,
				Purpose:    purpose,
				Value:      v,
				Unit:       modelIntelUnitIndex,
				CostUSD:    cost,
				RawID:      rawID,
			})
		}
		add(ModelPurposeGeneral, "artificial_analysis_intelligence_index")
		add(ModelPurposeCoding, "artificial_analysis_coding_index")
		add(ModelPurposeAgent, "artificial_analysis_agentic_index")
		if _, ok := evals["artificial_analysis_math_index"]; ok {
			add(ModelPurposeAnalysis, "artificial_analysis_math_index")
		} else {
			add(ModelPurposeAnalysis, "hle")
		}
		add(ModelPurposeBrowse, "aa_omniscience_accuracy")
	}
	return out, nil
}

func aaEvalMap(raw json.RawMessage) map[string]float64 {
	out := map[string]float64{}
	if len(raw) == 0 {
		return out
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return out
	}
	for k, v := range obj {
		switch n := v.(type) {
		case float64:
			out[k] = n
		}
	}
	return out
}

func aaIndexVersion(raw json.RawMessage, evals map[string]float64) string {
	if len(raw) > 0 {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
		var n float64
		if json.Unmarshal(raw, &n) == nil && n != 0 {
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%g", n), "0"), ".")
		}
	}
	if v, ok := evals["intelligence_index_version"]; ok && v != 0 {
		return fmt.Sprintf("%g", v)
	}
	return ""
}

func aaCostUSD(raw json.RawMessage, evals map[string]float64) float64 {
	if v, ok := evals["artificial_analysis_intelligence_index_cost"]; ok {
		return v
	}
	if len(raw) == 0 {
		return 0
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return 0
	}
	if n, ok := asFloat(obj["total_cost"]); ok {
		return n
	}
	if inner, ok := obj["cost_per_task"].(map[string]any); ok {
		if n, ok := asFloat(inner["total_cost"]); ok {
			return n
		}
	}
	return 0
}

func asFloat(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok
}
