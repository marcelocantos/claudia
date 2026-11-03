// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// 🎯T249: one rule of authority, along the line of creation plus the owner's
// grant of everything, and tools chosen by whoever starts an agent.

// authorityRig starts an overseer holding everything, a PO under it, and a
// worker under the PO, beside an unrelated root agent.
func authorityRig(t *testing.T) *fleetRig {
	t.Helper()
	seats := map[string]*fakeFleetSeat{}
	for _, name := range []string{"overseer", "po", "worker", "stranger"} {
		seats[name] = newFakeFleetSeat(fleetAnswer(FleetNoReply))
	}
	r, _ := newClockedFleetRig(t, seats)
	ctx := context.Background()
	if err := r.k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "overseer", Instructions: "x", WorkDir: "/tmp", Authority: FleetAuthorityAll}); err != nil {
		t.Fatal(err)
	}
	r.start("stranger", FleetOwner)
	if err := r.k.Start(ctx, "f", "overseer", &FleetStartArgs{Name: "po", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Start(ctx, "f", "po", &FleetStartArgs{Name: "worker", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFleetOnlyTheOwnerGivesAuthorityOverEverything(t *testing.T) {
	r := authorityRig(t)
	r.seats["deputy"] = newFakeFleetSeat(fleetHold)
	if err := r.k.Start(context.Background(), "f", "overseer", &FleetStartArgs{Name: "deputy", Instructions: "x", Authority: FleetAuthorityAll}); err == nil {
		t.Fatal("an agent handed out authority over everything")
	}
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "deputy", Instructions: "x", WorkDir: "/tmp", Authority: "some"}); err == nil {
		t.Fatal("an unknown authority was accepted")
	}
}

func TestFleetAuthorityFollowsTheLineOfCreation(t *testing.T) {
	r := authorityRig(t)
	ctx := context.Background()
	// The PO governs its worker, and nobody else.
	if _, err := r.k.Send("f", &FleetSendArgs{From: "po", To: "worker", Kind: FleetNotice, Text: "tick", Every: time.Hour}); err != nil {
		t.Fatalf("PO could not schedule for its worker: %v", err)
	}
	if err := r.k.SetBudget("f", "po", "worker", FleetBudget{Turns: 5}); err != nil {
		t.Fatalf("PO could not budget its worker: %v", err)
	}
	for name, err := range map[string]error{
		"PO stops a stranger":     r.k.Stop("f", "po", "stranger"),
		"PO stops its creator":    r.k.Stop("f", "po", "overseer"),
		"worker budgets its PO":   r.k.SetBudget("f", "worker", "po", FleetBudget{Turns: 1}),
		"PO budgets itself above": r.k.SetBudget("f", "stranger", "po", FleetBudget{Turns: 1}),
		"stranger schedules for worker": func() error {
			_, err := r.k.Send("f", &FleetSendArgs{From: "stranger", To: "worker", Text: "x", Every: time.Hour})
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	// The overseer holds everything: it can override what the PO set, and reach
	// an agent it did not create.
	if err := r.k.SetBudget("f", "overseer", "worker", FleetBudget{Turns: 50}); err != nil {
		t.Fatalf("overseer could not override the PO's budget: %v", err)
	}
	// Now the PO cannot change what the overseer set.
	if err := r.k.SetBudget("f", "po", "worker", FleetBudget{Turns: 500}); err == nil {
		t.Fatal("PO changed a budget its creator set")
	}
	st, _ := r.k.Status("f")
	if err := r.k.Cancel("f", "overseer", st.Timers[0].ID); err != nil {
		t.Fatalf("overseer could not cancel the PO's scheduled message: %v", err)
	}
	if err := r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "stranger"); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Stop("f", "overseer", "worker"); err != nil {
		t.Fatalf("overseer could not stop a worker it did not create: %v", err)
	}
	_ = ctx
}

func TestFleetChecksBelongToWhoeverRegisteredThem(t *testing.T) {
	repo := fleetCheckRepo(t)
	r := authorityRig(t)
	// The overseer sets the PO's measure, held out from everyone below it.
	if err := r.k.RegisterCheck("f", "overseer", &FleetCheckArgs{Name: "measure", Repo: repo, Script: "cat count.txt\n"}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.RegisterCheck("f", "po", &FleetCheckArgs{Name: "measure", Repo: repo, Script: "echo 0\n"}); err == nil {
		t.Fatal("the PO replaced the measure its creator set")
	}
	if _, err := r.k.RunCheck("f", "po", "measure", ""); err == nil {
		t.Fatal("the PO ran a held-out check")
	}
	if st, _ := r.k.StatusFor("f", "po"); len(st.Checks) != 0 {
		t.Fatalf("the PO can see a held-out check: %+v", st.Checks)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", "overseer", "measure", ""); err != nil {
		t.Fatal(err)
	}
	checkResult(t, r, &seq)
	st, _ := r.k.StatusFor("f", "overseer")
	if len(st.Checks) != 1 || st.Checks[0].Last == nil || *st.Checks[0].Last.Score != 7 {
		t.Fatalf("overseer's view of the latest result: %+v", st.Checks)
	}
	// Anyone may register a check of their own.
	if err := r.k.RegisterCheck("f", "worker", &FleetCheckArgs{Name: "mine", Repo: repo, Script: "echo 1\n"}); err != nil {
		t.Fatalf("a worker could not register its own check: %v", err)
	}
}

func TestFleetToolsNarrowDownTheLineOfCreation(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"po": newFakeFleetSeat(fleetHold), "worker": newFakeFleetSeat(fleetHold), "helper": newFakeFleetSeat(fleetHold)}
	r, _ := newClockedFleetRig(t, seats)
	ctx := context.Background()
	if err := r.k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "po", Instructions: "x", WorkDir: "/tmp", Tools: []string{"start", "send", "list", "check"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Start(ctx, "f", "po", &FleetStartArgs{Name: "worker", Instructions: "x", Tools: []string{"send", "stop"}}); err == nil {
		t.Fatal("a tool the starter lacks was given")
	}
	if err := r.k.Start(ctx, "f", "po", &FleetStartArgs{Name: "worker", Instructions: "x", Tools: []string{"bogus"}}); err == nil {
		t.Fatal("a tool that does not exist was given")
	}
	if err := r.k.Start(ctx, "f", "po", &FleetStartArgs{Name: "worker", Instructions: "x", Tools: []string{"send", "check", "start"}}); err != nil {
		t.Fatal(err)
	}
	// With no choice, a child gets all of its starter's tools, and no more.
	if err := r.k.Start(ctx, "f", "worker", &FleetStartArgs{Name: "helper", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.k)
	defer srv.Close()
	r.mu.Lock()
	tokens := map[string]string{}
	for _, spec := range r.specs {
		tokens[spec.Name] = spec.Token
	}
	r.mu.Unlock()
	names := func(agent string) []string {
		var out []string
		for _, tool := range fleetRPC(t, srv, tokens[agent], "tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
			out = append(out, tool.(map[string]any)["name"].(string))
		}
		return out
	}
	if got := strings.Join(names("helper"), ","); got != "start,send,check" {
		t.Fatalf("helper's tools: %s", got)
	}
	if text, isErr := fleetToolText(t, fleetRPC(t, srv, tokens["worker"], "tools/call", map[string]any{"name": "list"})); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("a tool it was not given: %q", text)
	}
}

// Review findings 2 and 3: a name keeps nothing after its agent retires.
func TestFleetAReusedNameInheritsNoAuthority(t *testing.T) {
	repo := fleetCheckRepo(t)
	seats := map[string]*fakeFleetSeat{}
	for _, name := range []string{"alice", "bob", "p", "c"} {
		seats[name] = newFakeFleetSeat(fleetHold)
	}
	r, _ := newClockedFleetRig(t, seats)
	ctx := context.Background()
	r.start("alice", FleetOwner)
	r.start("bob", FleetOwner)
	if err := r.k.RegisterCheck("f", "alice", &FleetCheckArgs{Name: "secret", Repo: repo, Script: "echo 1\n"}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "alice"); err != nil {
		t.Fatal(err)
	}
	seats["alice"] = newFakeFleetSeat(fleetHold)
	if err := r.k.Start(ctx, "f", "bob", &FleetStartArgs{Name: "alice", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.k.StatusFor("f", "bob"); len(st.Checks) != 0 {
		t.Fatalf("a newcomer's starter sees the old holder's check: %+v", st.Checks)
	}
	if _, err := r.k.RunCheck("f", "bob", "secret", ""); err == nil {
		t.Fatal("ran a check through a reused name")
	}
	if err := r.k.RegisterCheck("f", "bob", &FleetCheckArgs{Name: "secret", Repo: repo, Script: "echo 0\n"}); err == nil {
		t.Fatal("replaced a check through a reused name")
	}

	// A budget its parent set cannot be escaped by taking the parent's name.
	if err := r.k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "p", Instructions: "x", WorkDir: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Start(ctx, "f", "p", &FleetStartArgs{Name: "c", Instructions: "x", Budget: &FleetBudget{Turns: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Stop("f", FleetOwner, "p"); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "p to go", func() bool { st, _ := r.k.Status("f"); return !hasAgent(st, "p") })
	seats["p"] = newFakeFleetSeat(fleetHold)
	if err := r.k.Start(ctx, "f", "c", &FleetStartArgs{Name: "p", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.SetBudget("f", "c", "c", FleetBudget{Turns: 1000}); err == nil {
		t.Fatal("escaped a budget by taking its setter's name")
	}
}

func hasAgent(st *FleetStatus, name string) bool {
	for _, a := range st.Agents {
		if a.Name == name {
			return true
		}
	}
	return false
}

// Review finding 4: an old script's result never stands for a new one.
func TestFleetReregisteringACheckClearsItsResult(t *testing.T) {
	repo := fleetCheckRepo(t)
	r, _ := newClockedFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	r.start("a", FleetOwner)
	if err := r.k.RegisterCheck("f", FleetOwner, &FleetCheckArgs{Name: "held", Repo: repo, Script: "echo HELDOUT-SECRET\necho 7\n"}); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if _, err := r.k.RunCheck("f", FleetOwner, "held", ""); err != nil {
		t.Fatal(err)
	}
	checkResult(t, r, &seq)
	if err := r.k.RegisterCheck("f", FleetOwner, &FleetCheckArgs{Name: "held", Repo: repo, Script: "echo 1\n", AgentsMayRun: true}); err != nil {
		t.Fatal(err)
	}
	st, _ := r.k.StatusFor("f", "a")
	if len(st.Checks) != 1 || st.Checks[0].Last != nil {
		t.Fatalf("an old result survived re-registration: %+v", st.Checks)
	}
}
