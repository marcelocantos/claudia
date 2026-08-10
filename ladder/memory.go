// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// Recalled is the serialisable form of one learned rule — everything
// claudia knows about it, plus its body as opaque content.
//
// Field order here is the on-disk field order: yaml.v3 emits struct
// fields in declaration order, so this declaration IS the format. There
// are deliberately no maps anywhere in the shape, because a map is where
// unstable ordering gets in and a file that reorders itself between runs
// diffs as noise.
type Recalled struct {
	RuleID          string   `yaml:"rule_id"`
	Class           string   `yaml:"class,omitempty"`
	Description     string   `yaml:"description"`
	Stage           Stage    `yaml:"stage"`
	ConsistencyOnly bool     `yaml:"consistency_only,omitempty"`
	Evidence        Evidence `yaml:"evidence"`

	// Rule is the body, held as parsed YAML that claudia does not
	// interpret.
	//
	// Claudia can marshal what it does not understand but cannot
	// unmarshal it, so a body round-trips as opaque content and the
	// consumer's Build decodes it. That is the same asymmetry the layer
	// surfaces have: the trusted side cannot fail, the untrusted side
	// resolves and can.
	Rule yaml.Node `yaml:"rule"`
}

// Decode unmarshals the opaque rule body into a consumer's own type.
func (r *Recalled) Decode(into any) error {
	if err := r.Rule.Decode(into); err != nil {
		return fmt.Errorf("ladder: decoding rule %q: %w", r.RuleID, err)
	}
	return nil
}

// MarshalRules renders a rule set as deterministic YAML.
//
// Entries are sorted by rule ID and fields emitted in declaration order,
// so serialising the same rule set twice produces byte-identical output
// and a git diff shows what changed and nothing else.
func MarshalRules(rules []Recalled) ([]byte, error) {
	sorted := make([]Recalled, len(rules))
	copy(sorted, rules)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RuleID < sorted[j].RuleID })

	out, err := yaml.Marshal(sorted)
	if err != nil {
		return nil, fmt.Errorf("ladder: marshalling rules: %w", err)
	}
	return out, nil
}

// UnmarshalRules parses a rule set.
//
// It REFUSES malformed input rather than returning what it could
// salvage. A memory that starts empty because its file would not parse
// is indistinguishable from one that never learned anything, and the
// ladder would quietly revert to waking a model for everything while
// reporting perfect health.
func UnmarshalRules(data []byte) ([]Recalled, error) {
	var rules []Recalled
	if err := yaml.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("ladder: parsing rules: %w", err)
	}
	for i := range rules {
		if rules[i].RuleID == "" {
			return nil, fmt.Errorf("ladder: rule %d has no rule_id", i)
		}
		if rules[i].Stage == "" {
			return nil, fmt.Errorf("ladder: rule %q has no stage", rules[i].RuleID)
		}
	}
	return rules, nil
}

// Fingerprint identifies a rule set by its content.
//
// It is a hash of the deterministic serialisation, so it is stable
// across processes and machines — unlike a version counter, which is
// only meaningful within the run that produced it. Once memory outlives
// the process, a counter becomes actively misleading and a hash does
// not.
func Fingerprint(rules []Recalled) (string, error) {
	data, err := MarshalRules(rules)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:12]), nil
}

// recalledFrom converts an installed entry to its serialisable form.
func recalledFrom(e Entry) (Recalled, error) {
	r := Recalled{
		RuleID:          e.RuleID,
		Class:           e.Class,
		Description:     e.Description,
		Stage:           e.Stage,
		ConsistencyOnly: e.ConsistencyOnly,
		Evidence:        e.Evidence,
	}
	// Marshal then re-parse, which is how an arbitrary body becomes a
	// node claudia can carry without understanding.
	body, err := yaml.Marshal(e.Rule)
	if err != nil {
		return Recalled{}, fmt.Errorf("ladder: marshalling body of rule %q: %w", e.RuleID, err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(body, &node); err != nil {
		return Recalled{}, fmt.Errorf("ladder: re-parsing body of rule %q: %w", e.RuleID, err)
	}
	// yaml.Unmarshal wraps everything in a document node; carry the
	// content so the emitted form nests correctly.
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = *node.Content[0]
	}
	r.Rule = node
	return r, nil
}
