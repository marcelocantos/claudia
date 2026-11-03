// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// 🎯T248: timers, budgets and checks. Timers and budgets run on a manual clock,
// so lateness, missed repeats and deadlines are exact rather than raced.

var fleetT0 = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

func newClockedFleetRig(t *testing.T, seats map[string]*fakeFleetSeat) (*fleetRig, *ManualClock) {
	t.Helper()
	clock := NewManualClock(fleetT0)
	r := &fleetRig{t: t, dir: t.TempDir(), seats: seats, clock: clock}
	r.open()
	if err := r.k.Open("f", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.k.Close() })
	return r, clock
}

// advanceUntil moves the clock in steps until cond holds, letting timer
// goroutines run between steps.
func advanceUntil(t *testing.T, clock *ManualClock, step time.Duration, what string, cond func() bool) {
	t.Helper()
	ctx := wallclockguard.UntilTestTimeout(t)
	for !cond() {
		if ctx.Err() != nil {
			t.Fatal("test timed out waiting for " + what)
		}
		clock.Advance(step)
		time.Sleep(time.Millisecond)
	}
}

func TestFleetTimerFiresAtItsTimeStampedWithTheTime(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r, clock := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	id, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "wake up", At: fleetT0.Add(10 * time.Minute)})
	if err != nil || !strings.HasPrefix(id, "t") {
		t.Fatalf("schedule: %q %v", id, err)
	}
	clock.Advance(9 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if len(seat.sends()) != 0 {
		t.Fatal("fired early")
	}
	advanceUntil(t, clock, time.Minute, "the timer", func() bool { return len(seat.sends()) == 1 })
	got := seat.sends()[0]
	for _, want := range []string{"[now ", "scheduled " + id, "wake up"} {
		if !strings.Contains(got, want) {
			t.Fatalf("delivery lacks %q: %q", want, got)
		}
	}
}

func TestFleetRepeatRunsUntilCancelledAndOnlyItsOwnerMayCancel(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetAnswer(FleetNoReply))}
	r, clock := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "tick", Every: 30 * time.Second}); err == nil {
		t.Fatal("a repeat under a minute was accepted")
	}
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "b", Text: "nag", Every: time.Minute}); err == nil {
		t.Fatal("an agent repeated a message to another agent")
	}
	id, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "tick", Every: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	advanceUntil(t, clock, time.Minute, "three ticks", func() bool { return len(seats["a"].sends()) == 3 })
	st, err := r.k.Status("f")
	if err != nil || len(st.Timers) != 1 || st.Timers[0].ID != id || st.Timers[0].EverySeconds != 60 {
		t.Fatalf("status timers %+v %v", st.Timers, err)
	}
	if err := r.k.Cancel("f", "b", id); err == nil {
		t.Fatal("another agent cancelled it")
	}
	if err := r.k.Cancel("f", "a", id); err != nil {
		t.Fatal(err)
	}
	// A firing already delivered stays delivered; nothing fires after this.
	waitFleet(t, "the mailbox to drain", func() bool {
		st, _ := r.k.Status("f")
		for _, a := range st.Agents {
			if a.Name == "a" {
				return a.Queued == 0 && a.State == FleetStateIdle
			}
		}
		return false
	})
	before := len(seats["a"].sends())
	clock.Advance(5 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n := len(seats["a"].sends()); n != before {
		t.Fatalf("fired %d times after cancel", n-before)
	}
	if st, _ := r.k.Status("f"); len(st.Timers) != 0 {
		t.Fatalf("cancelled timer still listed: %+v", st.Timers)
	}
}

func TestFleetRepeatAfterDowntimeFiresOnceAndSaysWhatItMissed(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r, clock := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "brief")
	waitFleet(t, "first turn", func() bool { return len(seat.sends()) == 1 })
	if _, err := r.k.Send("f", &FleetSendArgs{From: FleetOwner, To: "a", Kind: FleetNotice, Text: "tick", Every: time.Hour}); err != nil {
		t.Fatal(err)
	}
	r.k.Close()
	clock.Advance(4*time.Hour + 30*time.Minute)
	r.open()
	waitFleet(t, "the catch-up firing", func() bool { return len(seat.sends()) == 2 })
	got := seat.sends()[1]
	if !strings.Contains(got, "3 earlier firings missed") || !strings.Contains(got, "late") {
		t.Fatalf("catch-up: %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(seat.sends()); n != 2 {
		t.Fatalf("a burst of %d deliveries after downtime", n)
	}
	st, _ := r.k.Status("f")
	if len(st.Timers) != 1 || !st.Timers[0].Due.Equal(fleetT0.Add(5*time.Hour)) {
		t.Fatalf("repeat lost its schedule: %+v", st.Timers)
	}
}

func TestFleetStampSaysHowLongAMessageWaited(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r, clock := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "first")
	waitFleet(t, "turn", func() bool { return len(seat.sends()) == 1 })
	r.send(FleetOwner, "a", "second")
	clock.Advance(42 * time.Minute)
	st, _ := r.k.Status("f")
	if st.Agents[0].WorkingSeconds != 42*60 {
		t.Fatalf("working seconds %d", st.Agents[0].WorkingSeconds)
	}
	seat.replies <- "done"
	waitFleet(t, "second turn", func() bool { return len(seat.sends()) == 2 })
	if got := seat.sends()[1]; !strings.Contains(got, "sent 42m ago") || !strings.Contains(got, "your last turn ended 0s ago") {
		t.Fatalf("stamp: %q", got)
	}
}

func TestFleetTimersGoWithTheirAgent(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold), "b": newFakeFleetSeat(fleetHold)}
	r, _ := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	for _, args := range []*FleetSendArgs{
		{From: "a", To: "a", Text: "self", After: time.Hour},
		{From: FleetOwner, To: "a", Kind: FleetNotice, Text: "tick", Every: time.Hour},
		{From: "b", To: "a", Text: "later", After: time.Hour},
	} {
		if _, err := r.k.Send("f", args); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "a"); err != nil {
		t.Fatal(err)
	}
	// Everything to or from it goes; b's scheduled request is answered now, so a
	// later agent named a cannot inherit it.
	st, _ := r.k.Status("f")
	if len(st.Timers) != 0 {
		t.Fatalf("timers left: %+v", st.Timers)
	}
}

func TestFleetScheduleRefusals(t *testing.T) {
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	r.start("a", FleetOwner)
	for name, args := range map[string]*FleetSendArgs{
		"both delay and time": {From: "a", To: "a", Text: "x", After: time.Minute, At: fleetT0.Add(time.Hour)},
		"negative delay":      {From: "a", To: "a", Text: "x", After: -time.Minute},
		"undelayed to self":   {From: "a", To: "a", Text: "x"},
	} {
		if _, err := r.k.Send("f", args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := r.k.Cancel("f", FleetOwner, "t999"); err == nil {
		t.Error("cancelled a timer that does not exist")
	}
}

func TestFleetTurnBudgetHoldsWorkAndTellsTheOwnerOnce(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("ok"))
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 2}}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three", "four"} {
		r.send(FleetOwner, "a", text)
	}
	var seq uint64
	notices := 0
	for answers := 0; answers < 2 || notices == 0; {
		m := r.ownerMessage(&seq)
		if m.From == FleetBudgetSender {
			notices++
			if !strings.Contains(m.Text, "turns 2 of 2") {
				t.Fatalf("notice: %q", m.Text)
			}
		} else {
			answers++
		}
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(seat.sends()); n != 2 {
		t.Fatalf("%d turns ran under a budget of 2", n)
	}
	st, _ := r.k.Status("f")
	if st.Agents[0].Waiting == "" || st.Agents[0].Queued != 2 || st.Budgets[0].Spent == "" {
		t.Fatalf("status %+v %+v", st.Agents, st.Budgets)
	}
	if err := r.k.SetBudget("f", "a", FleetBudget{Turns: 10}); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "held work to run", func() bool { return len(seat.sends()) == 4 })
	if notices != 1 {
		t.Fatalf("owner told %d times", notices)
	}
}

func TestFleetTokenBudgetCountsWhatTheSeatReports(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("ok"))
	seat.turnTokens = 600
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Tokens: 1000}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.send(FleetOwner, "a", "work")
	}
	var seq uint64
	for {
		if m := r.ownerMessage(&seq); m.From == FleetBudgetSender {
			break
		}
	}
	if n := len(seat.sends()); n != 2 {
		t.Fatalf("%d turns ran; the second crossed the limit, so the third must wait", n)
	}
	st, _ := r.k.Status("f")
	if st.Budgets[0].SpentTokens != 1200 {
		t.Fatalf("spent %d tokens", st.Budgets[0].SpentTokens)
	}
}

func TestFleetBudgetCannotBeEscaped(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"p": newFakeFleetSeat(fleetHold), "c": newFakeFleetSeat(fleetAnswer("ok"))}
	r, _ := newClockedFleetRig(t, seats)
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "p", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 1}}); err != nil {
		t.Fatal(err)
	}
	// A child's bigger budget does not raise the parent's.
	if err := r.k.Start(context.Background(), "f", "p", &FleetStartArgs{Name: "c", Instructions: "x", Budget: &FleetBudget{Turns: 100}}); err != nil {
		t.Fatal(err)
	}
	// Retiring the holder does not free its descendants.
	if err := r.k.Stop("f", FleetOwner, "p"); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "p to go", func() bool { st, _ := r.k.Status("f"); return len(st.Agents) == 1 })
	r.send(FleetOwner, "c", "one")
	r.send(FleetOwner, "c", "two")
	var seq uint64
	for {
		if m := r.ownerMessage(&seq); m.From == FleetBudgetSender {
			break
		}
	}
	if n := len(seats["c"].sends()); n != 1 {
		t.Fatalf("the orphan ran %d turns under its old parent's budget of 1", n)
	}
}

func TestFleetTimeBudgetEndsOnTheClock(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("ok"))
	r, clock := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Seconds: 3600}}); err != nil {
		t.Fatal(err)
	}
	r.send(FleetOwner, "a", "early")
	waitFleet(t, "early turn", func() bool { return len(seat.sends()) == 1 })
	clock.Advance(time.Hour)
	r.send(FleetOwner, "a", "late")
	var seq uint64
	for {
		if m := r.ownerMessage(&seq); m.From == FleetBudgetSender {
			if !strings.Contains(m.Text, "time 1h00m") {
				t.Fatalf("notice: %q", m.Text)
			}
			break
		}
	}
	if n := len(seat.sends()); n != 1 {
		t.Fatalf("ran %d turns after the deadline", n-1)
	}
}

// fleetCheckRepo makes a git repository with one commit.
func fleetCheckRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "master"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "count.txt"), []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "start"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

// checkResult waits for the next check result in the log.
func checkResult(t *testing.T, r *fleetRig, seq *uint64) *FleetCheckResult {
	t.Helper()
	ctx := wallclockguard.UntilTestTimeout(t)
	for {
		events, err := r.k.Read(ctx, "f", *seq)
		if err != nil || events == nil {
			t.Fatalf("read: %v", err)
		}
		for i := range events {
			*seq = events[i].Seq
			if events[i].Kind == FleetEventCheckResult {
				return events[i].Check
			}
		}
	}
}

func TestFleetCheckRunsThePinnedScriptOnACleanCheckout(t *testing.T) {
	repo := fleetCheckRepo(t)
	seat := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	// The script reads a file in the checkout, and ignores the agent's dirty tree.
	if err := os.WriteFile(filepath.Join(repo, "count.txt"), []byte("999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "count", Repo: repo, Script: "echo working >&2\ncat count.txt\n", AgentsMayRun: true}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	id, err := r.k.RunCheck("f", FleetOwner, "count", "master")
	if err != nil {
		t.Fatal(err)
	}
	res := checkResult(t, r, &seq)
	if res.RunID != id || res.Score == nil || *res.Score != 7 || res.Exit != 0 || len(res.Commit) != 40 || res.SHA256 == "" || !strings.Contains(res.Output, "working") {
		t.Fatalf("result %+v", res)
	}
	if len(seat.sends()) != 0 {
		t.Fatal("an agent heard about the owner's run")
	}

	// An agent runs it through its tool and hears the result as a message.
	srv := httptest.NewServer(r.k)
	defer srv.Close()
	r.mu.Lock()
	token := r.specs[0].Token
	r.mu.Unlock()
	if text, isErr := fleetToolText(t, fleetRPC(t, srv, token, "tools/call", map[string]any{"name": "check", "arguments": map[string]any{"name": "count"}})); isErr {
		t.Fatalf("check tool: %s", text)
	}
	waitFleet(t, "the result message", func() bool { return fleetSeatHeard(seat, "Check count on") })
	if !fleetSeatHeard(seat, "score 7") {
		t.Fatalf("agent heard %q", seat.sends())
	}
}

func TestFleetHiddenCheckIsTheOwnersAlone(t *testing.T) {
	repo := fleetCheckRepo(t)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	r.start("a", FleetOwner)
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "heldout", Repo: repo, Script: "echo 1\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.k.RunCheck("f", "a", "heldout", ""); err == nil {
		t.Fatal("an agent ran a hidden check")
	}
	srv := httptest.NewServer(r.k)
	defer srv.Close()
	r.mu.Lock()
	token := r.specs[0].Token
	r.mu.Unlock()
	text, _ := fleetToolText(t, fleetRPC(t, srv, token, "tools/call", map[string]any{"name": "list"}))
	var st FleetStatus
	if err := json.Unmarshal([]byte(text), &st); err != nil || len(st.Checks) != 0 {
		t.Fatalf("an agent's list shows hidden checks: %q", text)
	}
	if owner, _ := r.k.Status("f"); len(owner.Checks) != 1 {
		t.Fatal("the owner cannot see the hidden check")
	}
}

func TestFleetCheckRefusesATamperedScriptAndBadRevisions(t *testing.T) {
	repo := fleetCheckRepo(t)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "c", Repo: repo, Script: "echo 1\n"}); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []string{"-x", "a b", "no-such-branch"} {
		if _, err := r.k.RunCheck("f", FleetOwner, "c", rev); err == nil {
			t.Errorf("revision %q was accepted", rev)
		}
	}
	pinned := filepath.Join(r.dir, "checks", "f", "c.sh")
	if err := os.Chmod(pinned, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinned, []byte("echo 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "c", ""); err != nil {
		t.Fatal(err)
	}
	if res := checkResult(t, r, &seq); res.Score != nil || !strings.Contains(res.Error, "modified") {
		t.Fatalf("tampered script: %+v", res)
	}
}

func TestFleetCheckTimesOutAndReportsIt(t *testing.T) {
	repo := fleetCheckRepo(t)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "slow", Repo: repo, Script: "sleep 60\n", Timeout: 200 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "slow", ""); err != nil {
		t.Fatal(err)
	}
	if res := checkResult(t, r, &seq); !strings.Contains(res.Error, "timed out") {
		t.Fatalf("result %+v", res)
	}
	if out, _ := exec.Command("git", "-C", repo, "worktree", "list").Output(); strings.Count(string(out), "\n") != 1 {
		t.Fatalf("checkout left behind: %s", out)
	}
}

func TestFleetCheckInterruptedByARestartIsAnswered(t *testing.T) {
	repo := fleetCheckRepo(t)
	seat := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "slow", Repo: repo, Script: "sleep 60\n", AgentsMayRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.k.RunCheck("f", "a", "slow", ""); err != nil {
		t.Fatal(err)
	}
	r.k.Close()
	r.open()
	waitFleet(t, "the interrupted result", func() bool { return fleetSeatHeard(seat, "broker restarted during this run") })
}

// Review finding 1: a repeat starting long ago must not spin.
func TestFleetRepeatFromThePastDoesNotFlood(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "busy")
	waitFleet(t, "turn", func() bool { return len(seat.sends()) == 1 })
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "tick", At: time.Date(1700, 1, 1, 0, 0, 0, 0, time.UTC), Every: time.Minute}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	st, _ := r.k.Status("f")
	if st.Agents[0].Queued > 1 || len(st.Timers) != 1 || st.Timers[0].Due.Before(fleetT0) {
		t.Fatalf("queued %d, timers %+v", st.Agents[0].Queued, st.Timers)
	}
}

// Review finding 3: a repeat keeps one firing waiting, however long its target
// cannot take it.
func TestFleetRepeatKeepsOneFiringWaiting(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r, clock := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "long job")
	waitFleet(t, "turn", func() bool { return len(seat.sends()) == 1 })
	if _, err := r.k.Send("f", &FleetSendArgs{From: FleetOwner, To: "a", Kind: FleetNotice, Text: "tick", Every: time.Minute}); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		clock.Advance(time.Minute)
		time.Sleep(time.Millisecond)
	}
	waitFleet(t, "firings to settle", func() bool { st, _ := r.k.Status("f"); return st.Agents[0].Queued == 1 })
	seat.replies <- FleetNoReply
	waitFleet(t, "the waiting firing", func() bool { return len(seat.sends()) == 2 })
	if got := seat.sends()[1]; !strings.Contains(got, "earlier firings missed") {
		t.Fatalf("coalesced firing: %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	if st, _ := r.k.Status("f"); st.Agents[0].Queued != 0 {
		t.Fatalf("%d firings still waiting", st.Agents[0].Queued)
	}
}

// Review finding 2: a spent budget can be raised after its holder is gone.
func TestFleetBudgetCanBeRaisedAfterItsHolderRetires(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"p": newFakeFleetSeat(fleetHold), "c": newFakeFleetSeat(fleetAnswer("ok"))}
	r, _ := newClockedFleetRig(t, seats)
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "p", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Start(context.Background(), "f", "p", &FleetStartArgs{Name: "c", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "p"); err == nil {
		// StopTree takes c too; restart c alone under p's budget instead.
		t.Log("stopped the whole tree")
	}
	// Rebuild: p with a budget, c under it, then only p retires.
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "p", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Start(context.Background(), "f", "p", &FleetStartArgs{Name: "c", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	st, _ := r.k.Status("f")
	var id string
	for _, b := range st.Budgets {
		if b.Holder == "p" {
			id = b.ID
		}
	}
	if err := r.k.Stop("f", FleetOwner, "p"); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "p to go", func() bool { st, _ := r.k.Status("f"); return len(st.Agents) == 1 })
	r.send(FleetOwner, "c", "one")
	r.send(FleetOwner, "c", "two")
	var seq uint64
	for {
		if m := r.ownerMessage(&seq); m.From == FleetBudgetSender {
			if !strings.Contains(m.Text, "raises budget "+id) {
				t.Fatalf("notice does not say how: %q", m.Text)
			}
			break
		}
	}
	if err := r.k.SetBudget("f", id, FleetBudget{Turns: 10}); err != nil {
		t.Fatalf("raise by ID: %v", err)
	}
	waitFleet(t, "held work to run", func() bool { return len(seats["c"].sends()) == 2 })
}

// Review finding 7: agents starting turns at once cannot all slip under a limit.
func TestFleetTurnBudgetHoldsUnderFanOut(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"p": newFakeFleetSeat(fleetHold)}
	for _, name := range []string{"c1", "c2", "c3", "c4", "c5"} {
		seats[name] = newFakeFleetSeat(fleetHold)
	}
	r, _ := newClockedFleetRig(t, seats)
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "p", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 1}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c1", "c2", "c3", "c4", "c5"} {
		if err := r.k.Start(context.Background(), "f", "p", &FleetStartArgs{Name: name, Instructions: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"c1", "c2", "c3", "c4", "c5"} {
		r.send(FleetOwner, name, "go")
	}
	time.Sleep(50 * time.Millisecond)
	started := 0
	for _, name := range []string{"c1", "c2", "c3", "c4", "c5"} {
		started += len(seats[name].sends())
	}
	if started != 1 {
		t.Fatalf("%d turns started under a budget of 1", started)
	}
}

// Review finding 8: a successor of the same name inherits nothing scheduled.
func TestFleetSuccessorInheritsNoScheduledMessages(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold), "b": newFakeFleetSeat(fleetAnswer(FleetNoReply))}
	r, _ := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	id, err := r.k.Send("f", &FleetSendArgs{From: "b", To: "a", Text: "later", After: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "a"); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "b to hear now", func() bool { return fleetSeatHeard(seats["b"], "agent a is gone") })
	if st, _ := r.k.Status("f"); len(st.Timers) != 0 {
		t.Fatalf("scheduled message %s outlived its target: %+v", id, st.Timers)
	}
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: FleetBudgetSender, Instructions: "x", WorkDir: "/tmp"}); err == nil {
		t.Fatal("an agent took the name the kernel's budget notices use")
	}
}

// Review finding 4: the repository's hooks cannot touch a check's export.
func TestFleetCheckIgnoresRepositoryHooks(t *testing.T) {
	repo := fleetCheckRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 100 > count.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "count", Repo: repo, Script: "cat count.txt\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "count", ""); err != nil {
		t.Fatal(err)
	}
	if res := checkResult(t, r, &seq); res.Score == nil || *res.Score != 7 {
		t.Fatalf("a hook changed the result: %+v", res)
	}
}

// Review finding 5: a check's leftovers die with it, and do not spoil its result.
func TestFleetCheckLeavesNothingRunning(t *testing.T) {
	repo := fleetCheckRepo(t)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "bg", Repo: repo, Script: "sleep 37 &\necho $! > " + pidFile + "\necho 5\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "bg", ""); err != nil {
		t.Fatal(err)
	}
	res := checkResult(t, r, &seq)
	if res.Exit != 0 || res.Score == nil || *res.Score != 5 || res.Error != "" {
		t.Fatalf("result %+v", res)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// A killed process can linger briefly until it is reaped; one left running
	// would stay for its full 37 seconds.
	waitFleet(t, "the check's background process to end", func() bool {
		return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() != nil
	})
}

func TestFleetCheckQueueIsBounded(t *testing.T) {
	repo := fleetCheckRepo(t)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	r.start("a", FleetOwner)
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "slow", Repo: repo, Script: "sleep 60\n", AgentsMayRun: true}); err != nil {
		t.Fatal(err)
	}
	for range fleetCheckQueue {
		if _, err := r.k.RunCheck("f", "a", "slow", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.k.RunCheck("f", "a", "slow", ""); err == nil {
		t.Fatal("agents' queue grew past its bound")
	}
	// Agents filling their queue cannot lock the owner out.
	if _, err := r.k.RunCheck("f", FleetOwner, "slow", ""); err != nil {
		t.Fatalf("owner locked out: %v", err)
	}
	if err := r.k.Shutdown(wallclockguard.UntilTestTimeout(t), "f"); err != nil {
		t.Fatal(err)
	}
}

// gitIn runs git in repo and returns its trimmed output, failing the test on error.
func gitIn(t *testing.T, repo, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Second review, finding 1: a commit cannot make the export write outside it.
func TestFleetCheckExportCannotEscape(t *testing.T) {
	repo := fleetCheckRepo(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := gitIn(t, repo, outside, "hash-object", "-w", "--stdin")
	file := gitIn(t, repo, "pwned\n", "hash-object", "-w", "--stdin")
	// A symlink "a" to outside, and a directory "a" holding "victim": git
	// archive emitted both, and the second write followed the first.
	inner := gitIn(t, repo, "100644 blob "+file+"\tvictim\n", "mktree")
	tree := gitIn(t, repo, "120000 blob "+link+"\ta\n040000 tree "+inner+"\ta\n100644 blob "+file+"\tcount.txt\n", "mktree", "--missing")
	commit := gitIn(t, repo, "", "commit-tree", tree, "-m", "hostile")
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "c", Repo: repo, Script: "echo 1\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "c", commit); err != nil {
		t.Fatal(err)
	}
	checkResult(t, r, &seq)
	if raw, _ := os.ReadFile(victim); string(raw) != "intact" {
		t.Fatalf("the export wrote outside its directory: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(outside, "victim")); err != nil {
		t.Fatal(err)
	}
}

// Second review, finding 2: a replace ref cannot substitute another tree.
func TestFleetCheckIgnoresReplaceRefs(t *testing.T) {
	repo := fleetCheckRepo(t)
	owner := gitIn(t, repo, "", "rev-parse", "HEAD")
	forged := gitIn(t, repo, "", "commit-tree", gitIn(t, repo, "100644 blob "+gitIn(t, repo, "100\n", "hash-object", "-w", "--stdin")+"\tcount.txt\n", "mktree"), "-m", "forged")
	gitIn(t, repo, "", "replace", owner, forged)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "count", Repo: repo, Script: "cat count.txt\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "count", owner); err != nil {
		t.Fatal(err)
	}
	if res := checkResult(t, r, &seq); res.Score == nil || *res.Score != 7 {
		t.Fatalf("a replace ref changed the result: %+v", res)
	}
}

// Second review, finding 3: the shared repository's attributes cannot hide files.
func TestFleetCheckIgnoresRepositoryAttributes(t *testing.T) {
	repo := fleetCheckRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"), []byte("count.txt export-ignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "count", Repo: repo, Script: "cat count.txt\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "count", ""); err != nil {
		t.Fatal(err)
	}
	if res := checkResult(t, r, &seq); res.Score == nil || *res.Score != 7 {
		t.Fatalf("attributes changed the export: %+v", res)
	}
}

// Second review, finding 4: a turn interrupted by a restart is finished, not
// held by the charge it already paid.
func TestFleetRedeliveredTurnIsNotHeldOrChargedAgain(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 1}}); err != nil {
		t.Fatal(err)
	}
	id := r.send(FleetOwner, "a", "the one job")
	waitFleet(t, "turn", func() bool { return len(seat.sends()) == 1 })
	r.k.Close()
	next := newFakeFleetSeat(fleetAnswer("finished"))
	r.setSeat("a", next)
	r.open()
	var seq uint64
	var m *FleetMessage
	for m == nil || m.InReplyTo != id {
		m = r.ownerMessage(&seq)
	}
	if st, _ := r.k.Status("f"); st.Budgets[0].SpentTurns != 1 {
		t.Fatalf("redelivery charged again: %+v", st.Budgets)
	}
}

// Second review, findings 5 and 6.
func TestFleetBudgetIDsAndFailedStartsLeaveNoAmbiguity(t *testing.T) {
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"b": newFakeFleetSeat(fleetAnswer(FleetNoReply))})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "b1", Instructions: "x", WorkDir: "/tmp"}); err == nil {
		t.Fatal("an agent took a name shaped like a budget ID")
	}
	r.start("b", FleetOwner)
	entered, release := make(chan struct{}), make(chan struct{})
	r.setStartFn(func(_ context.Context, spec *FleetSeatSpec) (FleetSeat, error) {
		if spec.Name == "b" {
			return r.seats["b"], nil
		}
		close(entered)
		<-release
		return nil, errors.New("no model")
	})
	failed := make(chan error, 1)
	go func() {
		failed <- r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "late", Instructions: "x", WorkDir: "/tmp"})
	}()
	<-entered
	if _, err := r.k.Send("f", &FleetSendArgs{From: "b", To: "late", Text: "later", After: time.Hour}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-failed; err == nil {
		t.Fatal("start succeeded")
	}
	if st, _ := r.k.Status("f"); len(st.Timers) != 0 {
		t.Fatalf("a failed start left %+v for a successor", st.Timers)
	}
}

// commitTree makes a commit of the given ls-tree lines in repo and returns it.
func commitTree(t *testing.T, repo, entries string) string {
	t.Helper()
	tree := gitIn(t, repo, entries, "mktree", "--missing")
	return gitIn(t, repo, "", "commit-tree", tree, "-m", "test")
}

// Third review, finding 2: a malformed object fails fast, not at the timeout.
func TestFleetCheckMalformedObjectFailsFast(t *testing.T) {
	repo := fleetCheckRepo(t)
	// A tree object, listed as a file, larger than a pipe's buffer.
	var lines strings.Builder
	blob := gitIn(t, repo, "x\n", "hash-object", "-w", "--stdin")
	for i := range 3000 {
		fmt.Fprintf(&lines, "100644 blob %s\tfile%04d\n", blob, i)
	}
	innerTree := gitIn(t, repo, lines.String(), "mktree")
	// mktree refuses the lie, so write the outer tree object directly.
	raw, err := hex.DecodeString(innerTree)
	if err != nil {
		t.Fatal(err)
	}
	outer := gitIn(t, repo, "100644 looks-like-a-file\x00"+string(raw), "hash-object", "-t", "tree", "--literally", "-w", "--stdin")
	commit := gitIn(t, repo, "", "commit-tree", outer, "-m", "malformed")
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "c", Repo: repo, Script: "echo 1\n", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "c", commit); err != nil {
		t.Fatal(err)
	}
	res := checkResult(t, r, &seq)
	if !strings.Contains(res.Error, "export") || res.Seconds > 20 {
		t.Fatalf("result %+v", res)
	}
}

// Third review, finding 3: a link may not lead outside the export.
func TestFleetCheckRefusesLinksOutOfTheExport(t *testing.T) {
	repo := fleetCheckRepo(t)
	for _, target := range []string{"/etc/hosts", "../../outside", "sub/../../x"} {
		link := gitIn(t, repo, target, "hash-object", "-w", "--stdin")
		commit := commitTree(t, repo, "120000 blob "+link+"\tcount.txt\n")
		r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
		if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "c", Repo: repo, Script: "cat count.txt\n"}); err != nil {
			t.Fatal(err)
		}
		var seq uint64
		if _, err := r.k.RunCheck("f", FleetOwner, "c", commit); err != nil {
			t.Fatal(err)
		}
		if res := checkResult(t, r, &seq); !strings.Contains(res.Error, "leaves the export") {
			t.Fatalf("link to %s: %+v", target, res)
		}
	}
	// A link within the export is fine.
	file := gitIn(t, repo, "9\n", "hash-object", "-w", "--stdin")
	link := gitIn(t, repo, "real.txt", "hash-object", "-w", "--stdin")
	commit := commitTree(t, repo, "100644 blob "+file+"\treal.txt\n120000 blob "+link+"\tcount.txt\n")
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "c", Repo: repo, Script: "cat count.txt\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "c", commit); err != nil {
		t.Fatal(err)
	}
	if res := checkResult(t, r, &seq); res.Score == nil || *res.Score != 9 {
		t.Fatalf("inner link: %+v", res)
	}
}

// Third review, finding 1: a large file is streamed to disk.
func TestFleetCheckExportsALargeFile(t *testing.T) {
	repo := fleetCheckRepo(t)
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, make([]byte, 64<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := gitIn(t, repo, "", "hash-object", "-w", big)
	count := gitIn(t, repo, "7\n", "hash-object", "-w", "--stdin")
	commit := commitTree(t, repo, "100644 blob "+blob+"\tzeros\n100644 blob "+count+"\tcount.txt\n")
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{})
	if err := r.k.RegisterCheck("f", &FleetCheckArgs{Name: "c", Repo: repo, Script: "test $(wc -c < zeros) -eq 67108864 && cat count.txt\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "c", commit); err != nil {
		t.Fatal(err)
	}
	if res := checkResult(t, r, &seq); res.Score == nil || *res.Score != 7 {
		t.Fatalf("result %+v", res)
	}
}

// Third review, finding 4: restarts during one turn charge it once.
func TestFleetTurnInterruptedByRestartsIsChargedOnce(t *testing.T) {
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 5}}); err != nil {
		t.Fatal(err)
	}
	r.send(FleetOwner, "a", "long job")
	for range 3 {
		waitFleet(t, "a turn", func() bool {
			st, _ := r.k.Status("f")
			return len(st.Agents) == 1 && st.Agents[0].State == FleetStateWorking
		})
		r.k.Close()
		r.setSeat("a", newFakeFleetSeat(fleetHold))
		r.open()
	}
	waitFleet(t, "the last turn", func() bool {
		st, _ := r.k.Status("f")
		return len(st.Agents) == 1 && st.Agents[0].State == FleetStateWorking
	})
	if st, _ := r.k.Status("f"); st.Budgets[0].SpentTurns != 1 {
		t.Fatalf("one message charged %d turns", st.Budgets[0].SpentTurns)
	}
}
