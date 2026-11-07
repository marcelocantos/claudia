// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// Exhaustion failover (🎯T252).
//
// A grant made with pick=remaining delegated the choice of provider to
// Claudia. When that provider then refuses a turn because its plan is spent
// (cause rate_limit or spend_wall), the seat is not left to fail every later
// send on the same provider: the daemon moves it, in place and with its
// conversation, to the fullest admitted fleet provider the seat allows, and
// submits a short continuation. The refusing provider is excluded whatever
// the usage meter still says, since a meter can lag a provider's own "no".
//
// A seat whose consumer named its provider is the consumer's to move: its
// refusal is forwarded, typed, as before. So is a refusal of a send that
// carried a host RequestID, because Claudia never mints or rewrites a host's
// request identity, and the continuation would answer under none.

const (
	// maxExhaustionFailovers bounds the moves one host submit can cause.
	// Each move also excludes every provider that already refused, so the
	// fleet roster bounds it too; this keeps a run of refusals from walking
	// the whole roster on one prompt.
	maxExhaustionFailovers = 2
	// failoverTimeout bounds one move end to end: the pick, waiting for the
	// refused turn to close, the in-place switch and the continuation send.
	failoverTimeout = 2 * time.Minute
	// failoverSettle bounds the wait for the refused turn to close before the
	// switch, which refuses a turn in flight.
	failoverSettle = 30 * time.Second
	// failoverPoll is how often that wait looks.
	failoverPoll = 50 * time.Millisecond
	// ProgressProviderFailover is the ProgressType of the non-error system
	// event that replaces the refusal on the consumer's stream while the
	// seat moves.
	ProgressProviderFailover = "provider_failover"
)

// failoverNudge is the continuation submitted after a move. The refused
// prompt is already in the conversation, so it is not sent again.
const failoverNudge = "Claudia moved this seat from %s to %s (%s) because %s refused the last turn: %s. " +
	"The conversation is intact. Carry on with the work from where it stopped; do not start it over."

// failoverEpisode is one host submit's failover budget. d.mu guards it.
type failoverEpisode struct {
	// refused are the providers that refused a turn in this episode.
	refused []claudia.Provider
	// hops counts the moves made in this episode.
	hops int
	// hostRequestID is set when a send in this episode carried a host
	// RequestID.
	hostRequestID bool
	// submits counts host submits on the grant, across episodes. A move
	// that fails after a newer submit drops its refusal: forwarding it then
	// would end the newer turn as an error.
	submits uint64
}

// noteSend starts a new episode on a submit and records a host RequestID.
// A steer joins the open turn, so it keeps the episode's budget.
func (e *failoverEpisode) noteSend(req *broker.SendRequest) {
	switch {
	case len(req.Escalation) > 0, req.Mode == broker.SendModeSubmit, req.Mode == broker.SendModeInterrupt:
		*e = failoverEpisode{submits: e.submits + 1}
	case req.Mode == broker.SendModeQueue:
		return
	}
	if req.RequestID != "" {
		e.hostRequestID = true
	}
}

// isExhaustionRefusal reports a turn the provider refused because its plan
// is spent.
func isExhaustionRefusal(ev claudia.Event) bool {
	if ev.Type != "assistant" || !ev.IsError || !ev.IsTerminalStop() {
		return false
	}
	switch claudia.ProviderCause(ev.Reason) {
	case claudia.CauseRateLimit, claudia.CauseSpendWall:
		return true
	}
	return false
}

// takeExhaustionRefusal starts a move when ev is an exhaustion refusal on a
// delegated seat with budget left, and reports whether it did. The refusal
// is then not forwarded: the consumer sees the move and the continued turn
// instead, and sees the refusal itself only if the move fails.
func (d *Daemon) takeExhaustionRefusal(name string, ev claudia.Event) bool {
	if !isExhaustionRefusal(ev) {
		return false
	}
	def := d.reg.Def(name)
	if def == nil || def.ProviderPick != claudia.PickRemaining {
		return false
	}
	d.mu.Lock()
	g := d.grants[name]
	if g == nil || g.fleet || g.pool || g.proc == nil || g.failover.hostRequestID {
		d.mu.Unlock()
		return false
	}
	proc := g.proc
	from := claudia.PlanProvider(proc.Provider())
	if !slices.Contains(g.failover.refused, from) {
		g.failover.refused = append(g.failover.refused, from)
	}
	if g.failover.hops >= maxExhaustionFailovers {
		hops := g.failover.hops
		d.mu.Unlock()
		d.forwardEvent(name, failoverNote(fmt.Sprintf(
			"provider failover stopped: %s refused after %d moves on this prompt; the refusal stands", from, hops)))
		return false
	}
	g.failover.hops++
	exclude := slices.Clone(g.failover.refused)
	submits := g.failover.submits
	d.mu.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(d.ctx, failoverTimeout)
		defer cancel()
		if err := d.moveExhaustedSeat(ctx, name, proc, *def, from, exclude, ev); err != nil {
			d.mu.Lock()
			stale := d.grants[name] != g || g.failover.submits != submits
			observe := d.failoverDecision
			d.mu.Unlock()
			if observe != nil {
				defer observe(name, !stale)
			}
			if stale {
				d.log.Warn("exhaustion failover failed after a newer submit; dropping the old refusal", "grant", name, "from", from, "err", err)
				return
			}
			d.log.Warn("exhaustion failover failed; forwarding the refusal", "grant", name, "from", from, "err", err)
			d.forwardEvent(name, failoverNote(fmt.Sprintf("provider failover not possible: %v; the refusal stands", err)))
			d.forwardEvent(name, ev)
		}
	}()
	return true
}

// moveExhaustedSeat switches the seat off from and submits the continuation.
func (d *Daemon) moveExhaustedSeat(ctx context.Context, name string, proc *claudia.Agent, def claudia.AgentDef,
	from claudia.Provider, exclude []claudia.Provider, refusal claudia.Event) error {
	for _, p := range def.ExcludeProviders {
		exclude = append(exclude, claudia.PlanProvider(p))
	}
	var allowed []claudia.Provider
	if def.AllowedProviders != nil {
		allowed = []claudia.Provider{}
		for _, p := range def.AllowedProviders {
			allowed = append(allowed, claudia.PlanProvider(p))
		}
	}
	choice, err := d.pickByRemainingFor(true, func(p claudia.Provider) bool {
		return !slices.Contains(exclude, p) && (allowed == nil || slices.Contains(allowed, p))
	})
	if err != nil {
		return fmt.Errorf("no other provider is eligible (refused: %v): %w", exclude, err)
	}
	to := choice.Provider
	fromModel := proc.Model()
	model := sameShelfModel(ctx, fromModel, to)

	why := refusal.Reason
	d.forwardEvent(name, claudia.Event{
		Type:         "system",
		ProgressType: ProgressProviderFailover,
		SessionID:    proc.SessionID(),
		FromProvider: from,
		ToProvider:   to,
		FromModel:    fromModel,
		Model:        model,
		Reason:       why,
		Text:         fmt.Sprintf("%s refused the turn (%s); moving this seat to %s, conversation kept: %s", from, why, to, clip(refusal.Text)),
	})

	settle := time.NewTimer(failoverSettle)
	defer settle.Stop()
	poll := time.NewTicker(failoverPoll)
	defer poll.Stop()
	for proc.PromptInFlight() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-settle.C:
			return fmt.Errorf("the refused turn did not close within %s", failoverSettle)
		case <-poll.C:
		}
	}
	sw, err := d.reg.SwitchModel(ctx, name, to, model, false)
	if err != nil {
		return fmt.Errorf("switch to %s: %w", to, err)
	}
	d.log.Info("exhaustion failover", "grant", name, "from", from, "from_model", fromModel,
		"to", to, "model", sw.Model, "reason", why)
	nudge := fmt.Sprintf(failoverNudge, from, to, sw.Model, from, why)
	if _, err := proc.SendModeWithRequestID(nudge, claudia.DeliverySubmit, ""); err != nil {
		return fmt.Errorf("moved to %s, but the continuation was not submitted: %w", to, err)
	}
	return nil
}

// sameShelfModel is the session model on provider to at the quality of
// fromModel, so a move does not drop a frontier seat to a standard model.
// Empty leaves the choice to [claudia.Registry.SwitchModel] (standard).
func sameShelfModel(ctx context.Context, fromModel string, to claudia.Provider) string {
	quality := claudia.ModelQualityStandard
	if row, ok := claudia.MatchCatalogGeneration(fromModel); ok && row.Quality != "" {
		quality = row.Quality
	}
	pick, err := claudia.Resolve(ctx, claudia.ModelPredicates{
		Mode: claudia.CapabilitySession, Quality: quality,
		PreferPlan: true, PreferProvider: to,
		Usage: []claudia.PlanUsage{},
	})
	if err != nil || pick.Provider != to {
		return ""
	}
	return pick.Model
}

// failoverNote is a non-error system event: a consumer folding a turn does
// not end on it.
func failoverNote(text string) claudia.Event {
	return claudia.Event{Type: "system", ProgressType: ProgressProviderFailover, Text: text}
}

func clip(s string) string {
	const max = 240
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
