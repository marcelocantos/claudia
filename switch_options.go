// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"strings"

	"github.com/marcelocantos/claudia/omp"
)

// CompactOn selects which provider/model performs switch-time compaction
// (🎯T243). Zero is CompactOnSource: fold with the seat's current model,
// soft summary only, preserving legacy behavior.
type CompactOn uint8

const (
	CompactOnSource CompactOn = iota
	CompactOnDestination
	CompactOnExplicit
)

// SwitchOptions configures an in-place Agent.SwitchModel / Registry.SwitchModel.
// Omit it or pass the zero value for legacy switch-time folding on the source model.
type SwitchOptions struct {
	CompactOn       CompactOn
	CompactProvider Provider // required when CompactOn is CompactOnExplicit
	CompactModel    string
}

func mergeSwitchOptions(opts []SwitchOptions) SwitchOptions {
	if len(opts) == 0 {
		return SwitchOptions{}
	}
	return opts[0]
}

// Validate rejects unknown compaction modes and incomplete explicit selection.
func (o SwitchOptions) Validate() error {
	switch o.CompactOn {
	case CompactOnSource, CompactOnDestination:
		return nil
	case CompactOnExplicit:
		if strings.TrimSpace(string(o.CompactProvider)) == "" || strings.TrimSpace(o.CompactModel) == "" {
			return fmt.Errorf("SwitchModel: CompactOnExplicit requires CompactProvider and CompactModel")
		}
		return nil
	default:
		return fmt.Errorf("SwitchModel: unknown CompactOn value")
	}
}

func compactOnWire(o SwitchOptions) string {
	switch o.CompactOn {
	case CompactOnDestination:
		return "destination"
	case CompactOnExplicit:
		return "explicit"
	default:
		return ""
	}
}

func applySwitchCompact(msg *omp.Message, o SwitchOptions) {
	if o.CompactOn == CompactOnSource || o.CompactOn == 0 {
		return
	}
	msg.CompactOn = compactOnWire(o)
	if o.CompactOn == CompactOnExplicit {
		msg.CompactProvider = ompProviderID(o.CompactProvider)
		msg.CompactModel = strings.TrimSpace(o.CompactModel)
	}
}

// SwitchOptionsFromWire parses broker/sidecar compact_on fields.
func SwitchOptionsFromWire(compactOn string, provider Provider, model string) (SwitchOptions, error) {
	switch compactOn {
	case "", "source":
		return SwitchOptions{}, nil
	case "destination":
		return SwitchOptions{CompactOn: CompactOnDestination}, nil
	case "explicit":
		o := SwitchOptions{CompactOn: CompactOnExplicit, CompactProvider: provider, CompactModel: model}
		return o, o.Validate()
	default:
		return SwitchOptions{}, fmt.Errorf("SwitchModel: unknown compact_on %q", compactOn)
	}
}
