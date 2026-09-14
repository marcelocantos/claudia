// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// ModelPurpose is the job quality is measured for (🎯T71).
// Empty in Resolve means the catalog path, not general — set
// [ModelPurposeGeneral] when the work is broad.
type ModelPurpose string

const (
	ModelPurposeGeneral  ModelPurpose = "general"
	ModelPurposeCoding   ModelPurpose = "coding"
	ModelPurposeAnalysis ModelPurpose = "analysis"
	ModelPurposeAgent    ModelPurpose = "agent"
	ModelPurposeBrowse   ModelPurpose = "browse"
)

// ModelEffort is a think / reasoning setting. It is not a model id.
type ModelEffort string

const (
	ModelEffortUnspecified ModelEffort = ""
	ModelEffortNone        ModelEffort = "none"
	ModelEffortLow         ModelEffort = "low"
	ModelEffortMedium      ModelEffort = "medium"
	ModelEffortAdaptive    ModelEffort = "adaptive"
	ModelEffortHigh        ModelEffort = "high"
	ModelEffortMax         ModelEffort = "max"
	ModelEffortXHigh       ModelEffort = "xhigh"
)

// ModelObservation is one published score (or cost) at one time.
// Generation and effort stay distinct; a later fetch appends, it does
// not overwrite.
type ModelObservation struct {
	ObservedAt time.Time    `json:"observed_at"`
	Source     string       `json:"source"`
	SourceRev  string       `json:"source_rev,omitempty"`
	Generation string       `json:"generation"`
	Effort     ModelEffort  `json:"effort,omitempty"`
	Purpose    ModelPurpose `json:"purpose"`
	Value      float64      `json:"value"`
	Unit       string       `json:"unit,omitempty"`
	CostUSD    float64      `json:"cost_usd,omitempty"`
	N          float64      `json:"n,omitempty"`
	RawID      string       `json:"raw_id,omitempty"`
}

// ModelIntelRun is one ingest attempt (success or skip/error).
type ModelIntelRun struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Source     string    `json:"source"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	Count      int       `json:"count"`
}

// ModelIntelArgs configures ingest and Resolve's intel read (🎯T71).
type ModelIntelArgs struct {
	// Dir is the store directory. Empty uses [DefaultModelIntelDir]
	// (CLAUDIA_MODEL_INTEL or StateDir/model-intel).
	Dir string
	// Now overrides the clock (tests).
	Now time.Time
	// HTTPClient is used for AA. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// APIKey overrides CLAUDIA_AA_API_KEY.
	APIKey string
	// FetchAA replaces the AA HTTP call (tests).
	FetchAA func() ([]byte, error)
	// Latest injects observations and skips the store (tests).
	Latest []ModelObservation
}

const (
	modelIntelDirName          = "model-intel"
	modelIntelObsFile          = "observations.jsonl"
	modelIntelRunsFile         = "ingest_runs.jsonl"
	modelIntelSourceAA         = "aa"
	modelIntelUnitIndex        = "aa_index"
	modelIntelEnvDir           = "CLAUDIA_MODEL_INTEL"
	modelIntelEnvAAKey         = "CLAUDIA_AA_API_KEY"
	DefaultModelIntelInterval  = 24 * time.Hour
	modelIntelDriftIndexPoints = 1.0
)

// DefaultModelIntelDir is where the daemon and Resolve share the series.
func DefaultModelIntelDir() (string, error) {
	if p := os.Getenv(modelIntelEnvDir); p != "" {
		return p, nil
	}
	state, err := broker.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, modelIntelDirName), nil
}

func intelDir(args *ModelIntelArgs) (string, error) {
	if args != nil && args.Dir != "" {
		return args.Dir, nil
	}
	return DefaultModelIntelDir()
}

func intelNow(args *ModelIntelArgs) time.Time {
	if args != nil && !args.Now.IsZero() {
		return args.Now
	}
	return time.Now()
}
