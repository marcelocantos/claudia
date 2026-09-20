// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 🎯T85: a vendor's rate limiter counts requests, not processes.
//
// The in-memory cache already stops a running broker from re-asking inside
// its TTL, but it is reborn empty. Restart the broker six times while
// debugging and the usage endpoint sees six fetches in as many minutes,
// which is exactly the burst shape that put Anthropic's meter into a
// multi-hour refusal on 2026-09-20. The throttle therefore lives on disk,
// beside the shared snapshot, so every process on this host shares one
// account of when the last request went out and when the next may.
//
// It is deliberately a floor, not a scheduler: it never makes a request,
// it only refuses one that is too early. A refusal is reported with a
// reason, never as a zero reading (jevons 🎯T677 / 🎯T681).

const (
	// PlanThrottleMinInterval is the floor between two requests to one
	// provider from this host. Five minutes matches the cache TTL and is
	// the cadence Claude's endpoint has served for weeks without refusing.
	PlanThrottleMinInterval = 5 * time.Minute

	// PlanThrottleFirstPenalty is how long a provider is left alone after
	// it refuses us. The endpoint sends no Retry-After, so a short ladder
	// would just feed the refusal; start well clear of it.
	PlanThrottleFirstPenalty = 15 * time.Minute

	// PlanThrottleMaxPenalty caps the doubling.
	PlanThrottleMaxPenalty = 2 * time.Hour

	// PlanThrottleForcedInterval is the floor an explicit refresh still
	// obeys. A refresh button that waived the floor outright would be a
	// second way to produce the burst this whole mechanism exists to
	// prevent — eight probes in two minutes is what a person clicking
	// reload looks like — so a forced refresh is prompt, not unlimited.
	PlanThrottleForcedInterval = time.Minute

	planThrottleFile = "throttle.json"
)

// PlanThrottleEntry is one provider's account of its own traffic.
type PlanThrottleEntry struct {
	LastAttempt   time.Time `json:"last_attempt"`
	NextAllowedAt time.Time `json:"next_allowed_at"`
	// Refusals counts consecutive rate-limit responses; it drives the
	// penalty doubling and resets on the first success.
	Refusals int `json:"refusals"`
	// LastStatus is the HTTP status of the last completed attempt, for
	// the human reading this file to understand why they are waiting.
	LastStatus int `json:"last_status,omitempty"`
}

type planThrottleDoc struct {
	Providers map[string]PlanThrottleEntry `json:"providers"`
}

// planThrottleMu serialises this process's own read-modify-write. Other
// processes race through the file itself; a lost update can only ever
// cost one extra request, which is why this is not worth a lock file.
var planThrottleMu sync.Mutex

// PlanThrottleDecision is the answer to "may I fetch this provider now?".
type PlanThrottleDecision struct {
	Allowed bool
	// RetryAt is when the caller may ask again. Zero when allowed.
	RetryAt time.Time
	// Reason is empty when allowed, else why the request was withheld.
	Reason string
}

func planThrottlePath(dirOverride string) (string, error) {
	dir, err := planCacheDir(dirOverride)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, planThrottleFile), nil
}

func readPlanThrottle(path string) planThrottleDoc {
	doc := planThrottleDoc{Providers: map[string]PlanThrottleEntry{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc
	}
	// A corrupt throttle must not stop the product working: the worst a
	// reset costs is one early request, whereas a hard error here would
	// take plan usage down entirely.
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Providers == nil {
		return planThrottleDoc{Providers: map[string]PlanThrottleEntry{}}
	}
	return doc
}

func writePlanThrottle(path string, doc planThrottleDoc) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CheckPlanThrottle reports whether provider p may be fetched at now.
// forced is the caller's explicit refresh: it shortens the interval floor
// to PlanThrottleForcedInterval, and never waives a penalty, because a
// penalty means the vendor has already said no and asking harder is how
// the refusal is extended.
func CheckPlanThrottle(dirOverride string, p Provider, now time.Time, forced bool) PlanThrottleDecision {
	path, err := planThrottlePath(dirOverride)
	if err != nil {
		// No state dir is not a licence to hammer, but it is also not a
		// reason to stop working: allow, and let the cache do its job.
		return PlanThrottleDecision{Allowed: true}
	}
	planThrottleMu.Lock()
	defer planThrottleMu.Unlock()
	e, ok := readPlanThrottle(path).Providers[string(p)]
	if !ok {
		return PlanThrottleDecision{Allowed: true}
	}
	if e.Refusals > 0 && now.Before(e.NextAllowedAt) {
		return PlanThrottleDecision{
			Allowed: false,
			RetryAt: e.NextAllowedAt,
			Reason: fmt.Sprintf(
				"%s refused the last %d usage request(s); holding off until %s so the refusal is not extended",
				p, e.Refusals, e.NextAllowedAt.UTC().Format(time.RFC3339)),
		}
	}
	floor := PlanThrottleMinInterval
	if forced {
		floor = PlanThrottleForcedInterval
	}
	if next := e.LastAttempt.Add(floor); now.Before(next) {
		return PlanThrottleDecision{
			Allowed: false,
			RetryAt: next,
			Reason: fmt.Sprintf(
				"last %s usage request was %s ago; the floor between requests from this host is %s",
				p, now.Sub(e.LastAttempt).Round(time.Second), floor),
		}
	}
	return PlanThrottleDecision{Allowed: true}
}

// RecordPlanAttempt writes the outcome of one request. status is the HTTP
// status, or 0 when the request never completed. A 429 (or 529) escalates
// the penalty; any other answered status clears it, because a provider
// that answered at all is not refusing us. A request that never completed
// leaves an outstanding penalty exactly where it was.
func RecordPlanAttempt(dirOverride string, p Provider, now time.Time, status int) {
	path, err := planThrottlePath(dirOverride)
	if err != nil {
		return
	}
	planThrottleMu.Lock()
	defer planThrottleMu.Unlock()
	doc := readPlanThrottle(path)
	e := doc.Providers[string(p)]
	e.LastAttempt = now
	e.LastStatus = status
	switch {
	case status == 429 || status == 529:
		e.Refusals++
		penalty := PlanThrottleFirstPenalty << (e.Refusals - 1)
		if penalty > PlanThrottleMaxPenalty || penalty <= 0 {
			penalty = PlanThrottleMaxPenalty
		}
		e.NextAllowedAt = now.Add(penalty)
	case status == 0:
		// Nothing answered: a DNS failure, a timeout, a missing
		// credential that stopped the request before it was built. That
		// is not the provider withdrawing its refusal, so an outstanding
		// penalty stands — clearing it here would let a flaky network
		// hand back the allowance the vendor took away.
		if e.Refusals == 0 {
			e.NextAllowedAt = now.Add(PlanThrottleMinInterval)
		}
	default:
		e.Refusals = 0
		e.NextAllowedAt = now.Add(PlanThrottleMinInterval)
	}
	doc.Providers[string(p)] = e
	_ = writePlanThrottle(path, doc)
}

// 🎯T85: what a withheld provider publishes instead.
//
// A provider the floor withheld has not changed its mind about anything;
// it simply was not asked. Publishing a hole for it blanks a working
// gauge every cycle the floor bites, and a blank gauge is what parked a
// live worker on 2026-09-20. So the last reading is kept beside the
// floor that withholds it, and carried forward with its ORIGINAL
// FetchedAt: age is then visible rather than forged.

const planLastReadingFile = "readings.json"

type planLastReadingDoc struct {
	Providers map[string]PlanUsage `json:"providers"`
}

var planLastReadingMu sync.Mutex

func planLastReadingPath(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, planLastReadingFile), nil
}

// savePlanLastReading keeps pu as provider pu.Provider's carry-forward
// value. The body is dropped: it belongs to one request, and the raw
// store (🎯T84) is where responses live.
func savePlanLastReading(dir string, pu PlanUsage) {
	if dir == "" || pu.Provider == "" {
		return
	}
	path, err := planLastReadingPath(dir)
	if err != nil {
		return
	}
	pu.RawBody = ""
	planLastReadingMu.Lock()
	defer planLastReadingMu.Unlock()
	doc := readPlanLastReadings(path)
	doc.Providers[string(pu.Provider)] = pu
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// loadPlanLastReading returns the carried reading for p, if any.
func loadPlanLastReading(dir string, p Provider) (PlanUsage, bool) {
	if dir == "" {
		return PlanUsage{}, false
	}
	pu, ok := readPlanLastReadings(filepath.Join(dir, planLastReadingFile)).Providers[string(p)]
	return pu, ok
}

func readPlanLastReadings(path string) planLastReadingDoc {
	doc := planLastReadingDoc{Providers: map[string]PlanUsage{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Providers == nil {
		return planLastReadingDoc{Providers: map[string]PlanUsage{}}
	}
	return doc
}
