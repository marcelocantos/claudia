// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
)

// Provider identifies the CLI/runtime backing a Task or Agent.
type Provider string

// PlanProvider maps a session backend to the subscription whose allowance it
// spends. The Oh My Pi sidecar uses protocol identifiers while the plan meter
// and model catalog use product identifiers. Keep that translation here so
// every host evaluates a seat against the same allowance.
func PlanProvider(provider Provider) Provider {
	switch provider {
	case "anthropic":
		return ProviderClaude
	case "openai-codex":
		return ProviderCodex
	case "xai-oauth":
		return ProviderGrok
	default:
		return provider
	}
}

// SubscriptionSeatProvider returns the runtime id of a subscription plan
// on the Oh My Pi sidecar. Hosts pass the selected plan provider through
// this function before starting or migrating a broker seat; legacy CLI
// identities stay readable through PlanProvider.
func SubscriptionSeatProvider(provider Provider) Provider {
	switch PlanProvider(provider) {
	case ProviderClaude:
		return "anthropic"
	case ProviderCodex:
		return "openai-codex"
	case ProviderGrok:
		return "xai-oauth"
	default:
		return provider
	}
}

const (
	// ProviderClaude is the Claude subscription seat on the Oh My Pi
	// sidecar and is the default when Provider is empty.
	ProviderClaude Provider = "claude"
	// ProviderCodex is the Codex subscription seat on the Oh My Pi sidecar.
	ProviderCodex Provider = "codex"
	// ProviderGrok is the Grok subscription seat on the Oh My Pi sidecar.
	// Distinct from the Realtime voice client in package
	// github.com/marcelocantos/claudia/grok.
	ProviderGrok Provider = "grok"
	// ProviderBedrock uses Anthropic Claude models via AWS Bedrock
	// ConverseStream (API path; no local claude CLI). Task mode only in v1.
	ProviderBedrock Provider = "bedrock"

	// ProviderOllama is local inference through an Ollama daemon. Its
	// cost is latency rather than money, which is why it reports no cost
	// capability instead of reporting a spend of zero.
	ProviderOllama Provider = "ollama"

	// ProviderCursor is the Cursor subscription seat on the Oh My Pi
	// sidecar. The Cursor agent CLI is not located and is not a fleet
	// seat transport.
	ProviderCursor Provider = "cursor"
)

// Capability reporting (Capability, CapabilityStatus, CapabilityError,
// the provider claim matrix) lives in capability.go.

// resolveCodexBin refuses. The subscription CLI is not located.
func resolveCodexBin() (string, error) {
	return "", fmt.Errorf("%w: codex", ErrLegacyCLISeatTransport)
}

// resolveGrokBin refuses. The subscription CLI is not located.
func resolveGrokBin() (string, error) {
	return "", fmt.Errorf("%w: grok", ErrLegacyCLISeatTransport)
}

// resolveCursorBin refuses. The subscription CLI is not located.
func resolveCursorBin() (string, error) {
	return "", fmt.Errorf("%w: cursor", ErrLegacyCLISeatTransport)
}
