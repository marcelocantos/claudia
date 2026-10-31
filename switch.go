// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// switchModelTimeout bounds a switch end to end. It covers folding a long
// conversation with the old model when it does not fit the new one.
const switchModelTimeout = 15 * time.Minute

// SwitchModel moves this seat to provider and model in place, between turns,
// keeping its conversation (🎯T195). Nothing is summarised and no new seat is
// started: the seat's own agent takes the new model.
//
// A sidecar seat can change provider: its history is converted per provider
// on every request, and a conversation too large for the new model's window
// is folded first by the model that wrote it. A native CLI seat (Claude tmux,
// Codex app-server, Grok ACP) can change model only within its own provider,
// and is gated by [CapabilityModelSwitch].
//
// An empty provider keeps the seat's own. It refuses an empty model, a seat
// that is not running, and a turn in flight. On success [Agent.Provider] and
// [Agent.Model] name the new model, a model_switch Event is published, and a
// seat launched by a [Registry] has its definition rewritten to match.
func (a *Agent) SwitchModel(provider Provider, model string, opts ...SwitchOptions) error {
	opt := mergeSwitchOptions(opts)
	if err := opt.Validate(); err != nil {
		return err
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("SwitchModel: model must be non-empty")
	}
	from := a.Provider()
	if provider == "" {
		provider = from
	}
	if !useOMP(Config{Provider: from, OMP: a.startCfg.OMP}) {
		if err := CheckCapability(from, CapabilityModelSwitch); err != nil {
			return err
		}
	}
	<-a.ready
	if a.readyErr != nil {
		return fmt.Errorf("agent not ready: %w", a.readyErr)
	}
	a.mu.Lock()
	alive := a.alive
	fromModel := a.model
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("agent process not running")
	}
	if a.PromptInFlight() {
		return fmt.Errorf("SwitchModel: turn in flight; wait for the current response or Interrupt first")
	}
	if a.ops.switchModel == nil {
		return unsupportedCapability(from, CapabilityModelSwitch, "provider did not supply a switchModel operation")
	}
	if err := a.ops.switchModel(a, provider, model, opt); err != nil {
		return err
	}
	a.mu.Lock()
	a.provider, a.model = provider, model
	record := a.onSwitched
	a.mu.Unlock()
	if a.brokerGrant == "" {
		// A broker-held handle hears the daemon's own event on its stream;
		// publishing here too would double it.
		a.publishEvent(Event{
			Type:         "system",
			ProgressType: ProgressModelSwitch,
			SessionID:    a.SessionID(),
			Model:        model,
			FromProvider: from,
			ToProvider:   provider,
			FromModel:    fromModel,
			Text:         fmt.Sprintf("model switch: %s/%s -> %s/%s", from, fromModel, provider, model),
		})
	}
	if record != nil {
		if err := record(); err != nil {
			return fmt.Errorf("switched to %s/%s, but recording the seat failed: %w", provider, model, err)
		}
	}
	return nil
}


// SyncSessionMCPServers updates the running seat's session-scoped MCP list
// before an in-place switch reloads the sidecar (jevons T612). The
// registry row is already restamped; this pushes the same list into the
// live OMP host-tools snapshot the switch load carries.
func (a *Agent) SyncSessionMCPServers(servers []MCPServer) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.startCfg.MCPServers = slices.Clone(servers)
	sync := a.ops.syncSessionMCPServers
	a.mu.Unlock()
	if sync != nil {
		sync(a, servers)
	}
}

// sameProviderSwitch refuses a provider change on a backend that can only
// change model within its own provider.
func sameProviderSwitch(a *Agent, provider Provider) error {
	if from := a.Provider(); provider != from {
		return unsupportedCapability(from, CapabilityModelSwitch,
			fmt.Sprintf("this backend cannot move to provider %s; only a sidecar seat changes provider", provider))
	}
	return nil
}
