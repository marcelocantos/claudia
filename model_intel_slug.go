// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"strings"
	"unicode"
)

// ParseModelSlug splits a board or vendor id into generation and effort.
// "claude-opus-5-max" → ("claude-opus-5", max). Unknown suffixes stay on
// the generation and effort is unspecified.
func ParseModelSlug(raw string) (generation string, effort ModelEffort) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", ModelEffortUnspecified
	}
	if i := strings.LastIndex(s, "("); i >= 0 && strings.HasSuffix(s, ")") {
		inner := strings.TrimSpace(s[i+1 : len(s)-1])
		if e, ok := parseEffortToken(inner); ok {
			effort = e
			s = strings.TrimSpace(s[:i])
		}
	}
	s = strings.ReplaceAll(s, " ", "-")
	parts := strings.Split(s, "-")
	parts = compactParts(parts)
	if n := len(parts); n >= 2 {
		if e, ok := parseEffortToken(parts[n-2] + "-" + parts[n-1]); ok {
			return strings.Join(parts[:n-2], "-"), e
		}
	}
	if n := len(parts); n >= 1 {
		if e, ok := parseEffortToken(parts[n-1]); ok {
			return strings.Join(parts[:n-1], "-"), e
		}
	}
	if effort != ModelEffortUnspecified {
		return s, effort
	}
	return s, ModelEffortUnspecified
}

func parseEffortToken(tok string) (ModelEffort, bool) {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case "none", "off", "minimal":
		return ModelEffortNone, true
	case "low":
		return ModelEffortLow, true
	case "medium", "mid", "default":
		return ModelEffortMedium, true
	case "adaptive":
		return ModelEffortAdaptive, true
	case "high", "thinking-high":
		return ModelEffortHigh, true
	case "max":
		return ModelEffortMax, true
	case "xhigh", "x-high", "extra-high", "extrahigh", "extra":
		return ModelEffortXHigh, true
	default:
		return "", false
	}
}

func compactParts(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normModelID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// catalogAliases maps a normalised research generation onto a spawnable
// catalog Model. Only rows we can start belong here.
var catalogAliases = map[string]string{
	"claudesonnet5":  "claude-sonnet-5",
	"claudesonnet46": "claude-sonnet-5",
	"claudesonnet45": "claude-sonnet-5",
	"claudesonnet4":  "claude-sonnet-5",
	"claudeopus5":    "claude-opus-5",
	"claudeopus46":   "claude-opus-5",
	"claudeopus45":   "claude-opus-5",
	"claudefable5":   "claude-fable-5",
	"claudefable51":  "claude-fable-5",
	"claudehaiku45":  "claude-haiku-4-5",
	"claudehaiku4":   "claude-haiku-4-5",
	"grok46":         "grok-4.6",
	"grok45":         "grok-4.5",
	"grok4":          "grok-4",
	"gpt5codex":      "gpt-5-codex",
	"gpt54codex":     "gpt-5-codex",
	"gpt53codex":     "gpt-5-codex",
	"composer25":     "composer-2.5",
	"composer2":      "composer-2.5",
}

func catalogByModel(model string) (CatalogModel, bool) {
	for _, row := range ModelCatalog() {
		if row.Model != "" && row.Model == model {
			return row, true
		}
	}
	return CatalogModel{}, false
}

// MatchCatalogGeneration maps a research generation onto a catalog row.
func MatchCatalogGeneration(generation string) (CatalogModel, bool) {
	if generation == "" {
		return CatalogModel{}, false
	}
	if row, ok := catalogByModel(generation); ok {
		return row, true
	}
	if alias, ok := catalogAliases[normModelID(generation)]; ok {
		return catalogByModel(alias)
	}
	return CatalogModel{}, false
}
