// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

// ModelAccess is how Claudia reaches a model (🎯T61.3).
type ModelAccess string

const (
	// ModelAccessPlan is a subscription-plan harness (Claude Code, Codex, …).
	ModelAccessPlan ModelAccess = "plan"
	// ModelAccessDirect is an HTTP/API path with no harness (Bedrock, Ollama).
	ModelAccessDirect ModelAccess = "direct"
)

// ModelQuality is a capability floor. On the catalog path it is a
// generation shelf. When Resolve is given a Purpose it is a floor on
// that purpose's intel series (🎯T71).
type ModelQuality string

const (
	ModelQualityFrontier ModelQuality = "frontier"
	ModelQualityStandard ModelQuality = "standard"
	ModelQualityEconomy  ModelQuality = "economy"
)

// CatalogModel is one known (provider, model) row.
type CatalogModel struct {
	Provider Provider
	Model    string
	Access   ModelAccess
	Quality  ModelQuality
	Task     bool
	Session  bool
}

// ModelCatalog is the built-in list Resolve searches. Empty Model means
// the provider default.
func ModelCatalog() []CatalogModel {
	return []CatalogModel{
		{Provider: ProviderClaude, Model: "claude-sonnet-5", Access: ModelAccessPlan, Quality: ModelQualityStandard, Task: true, Session: true},
		{Provider: ProviderClaude, Model: "claude-opus-5", Access: ModelAccessPlan, Quality: ModelQualityFrontier, Task: true, Session: true},
		{Provider: ProviderClaude, Model: "claude-fable-5", Access: ModelAccessPlan, Quality: ModelQualityFrontier, Task: true, Session: true},
		{Provider: ProviderClaude, Model: "claude-haiku-4-5", Access: ModelAccessPlan, Quality: ModelQualityEconomy, Task: true, Session: true},
		{Provider: ProviderGrok, Model: "grok-4.5", Access: ModelAccessPlan, Quality: ModelQualityStandard, Task: true, Session: true},
		{Provider: ProviderGrok, Model: "grok-4.6", Access: ModelAccessPlan, Quality: ModelQualityFrontier, Task: true, Session: true},
		{Provider: ProviderGrok, Model: "grok-4", Access: ModelAccessPlan, Quality: ModelQualityEconomy, Task: true, Session: true},
		{Provider: ProviderCodex, Model: "gpt-5-codex", Access: ModelAccessPlan, Quality: ModelQualityStandard, Task: true, Session: true},
		{Provider: ProviderCursor, Model: "composer-2.5", Access: ModelAccessPlan, Quality: ModelQualityStandard, Task: true, Session: true},
		{Provider: ProviderBedrock, Model: "", Access: ModelAccessDirect, Quality: ModelQualityStandard, Task: true},
		{Provider: ProviderOllama, Model: "", Access: ModelAccessDirect, Quality: ModelQualityEconomy, Task: true},
	}
}
