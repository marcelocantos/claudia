// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Measurement probe behind turnSilenceBound (🎯T96). It is not a gate
// and asserts nothing: it drives one live turn per backend around a
// long silent tool call and prints the longest gap inside it, which is
// the quantity the bound is set from. Run it by hand when the constant
// is questioned, the way the three Cursor mints behind
// cursorPromptSilenceBound were:
//
//	CLAUDIA_LIVE=1 CLAUDIA_GROK_LIVE=1 CLAUDIA_CURSOR_LIVE=1 \
//	  go test -run TestT96MeasureSilence -v -timeout 30m .
//
// Readings taken 2026-09-21 on a host at load ~290 are quoted in
// turnSilenceBound's own comment. Unset gates skip.

package claudia

import (
	"context"
	"os"
	"testing"
	"time"
)

func t96measure(t *testing.T, provider Provider, gate string) {
	t.Helper()
	if os.Getenv(gate) == "" {
		t.Skipf("%s not set", gate)
	}
	cfg := Config{Provider: provider, WorkDir: t.TempDir()}
	agent, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	type stamp struct {
		at   time.Time
		what string
	}
	stamps := make(chan stamp, 4096)
	tok := agent.SubscribeEvents(func(ev Event) {
		select {
		case stamps <- stamp{time.Now(), "event:" + ev.Type + "/" + ev.ProgressType}:
		default:
		}
	})
	defer agent.UnsubscribeEvents(tok)
	_, term := agent.SubscribeTerminal()
	go func() {
		for range term {
			select {
			case stamps <- stamp{time.Now(), "term"}:
			default:
			}
		}
	}()

	start := time.Now()
	prompt := "Run exactly this shell command with your shell tool and wait for it to finish: sleep 90 ; then reply with exactly: pong"
	if err := agent.Send(prompt); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	reply, err := agent.WaitForResponse(ctx)
	elapsed := time.Since(start)
	close(stamps)

	var (
		lastEvent, lastAny  = start, start
		maxEventGap, maxAny time.Duration
		atEvent, atAny      string
		events, terms       int
	)
	for s := range stamps {
		if s.what == "term" {
			terms++
		} else {
			events++
			if g := s.at.Sub(lastEvent); g > maxEventGap {
				maxEventGap, atEvent = g, s.what
			}
			lastEvent = s.at
		}
		if g := s.at.Sub(lastAny); g > maxAny {
			maxAny, atAny = g, s.what
		}
		lastAny = s.at
	}
	t.Logf("T96-MEASURE provider=%s turn=%s events=%d termchunks=%d maxEventGap=%s (before %s) maxActivityGap=%s (before %s) err=%v reply=%.40q",
		provider, elapsed.Round(time.Millisecond), events, terms,
		maxEventGap.Round(time.Millisecond), atEvent,
		maxAny.Round(time.Millisecond), atAny, err, reply)
}

func TestT96MeasureSilenceClaude(t *testing.T) { t96measure(t, ProviderClaude, "CLAUDIA_LIVE") }
func TestT96MeasureSilenceGrok(t *testing.T)   { t96measure(t, ProviderGrok, "CLAUDIA_GROK_LIVE") }
func TestT96MeasureSilenceCursor(t *testing.T) {
	t96measure(t, ProviderCursor, "CLAUDIA_CURSOR_LIVE")
}
