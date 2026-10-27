// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"slices"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

func toolsN(n int) []hostJevonsTool {
	out := make([]hostJevonsTool, n)
	for i := range out {
		out[i] = hostJevonsTool{Name: "t"}
	}
	return out
}

// A Codex seat offered 320 host tools got only hollow incomplete responses
// (2026-10-28); 252 answered. Whole servers go, last first.
func TestCodexHostToolCapDropsWholeServersFromTheEnd(t *testing.T) {
	servers := []MCPServer{{Name: "jevonsmcp"}, {Name: "playwright"}, {Name: "sawmill"}, {Name: "blurter"}}
	lists := [][]hostJevonsTool{toolsN(65), toolsN(25), toolsN(200), toolsN(3)}
	dropped := capHostToolLists(servers, lists, hostToolCap(omp.OpenAICodex))
	if !slices.Equal(dropped, []string{"blurter", "sawmill"}) {
		t.Fatalf("dropped = %v, want blurter then sawmill", dropped)
	}
	if lists[0] == nil || lists[1] == nil || lists[2] != nil || lists[3] != nil {
		t.Fatalf("kept the wrong servers: %v %v %v %v", len(lists[0]), len(lists[1]), len(lists[2]), len(lists[3]))
	}
}

func TestOtherProvidersTakeTheWholeHostToolList(t *testing.T) {
	servers := []MCPServer{{Name: "a"}, {Name: "b"}}
	lists := [][]hostJevonsTool{toolsN(300), toolsN(100)}
	if dropped := capHostToolLists(servers, lists, hostToolCap(omp.Anthropic)); dropped != nil {
		t.Fatalf("anthropic dropped %v", dropped)
	}
	if len(lists[0]) != 300 || len(lists[1]) != 100 {
		t.Fatal("anthropic lists were changed")
	}
}

// The consumer's own server stays even when it ends the list.
func TestCodexHostToolCapKeepsConsumerServers(t *testing.T) {
	SetHostToolBudgetKeeps(func(s MCPServer) bool { return s.Name == "jevonsmcp" })
	t.Cleanup(func() { SetHostToolBudgetKeeps(nil) })
	servers := []MCPServer{{Name: "atlassian"}, {Name: "sawmill"}, {Name: "jevonsmcp"}}
	lists := [][]hostJevonsTool{toolsN(41), toolsN(200), toolsN(65)}
	dropped := capHostToolLists(servers, lists, hostToolCap(omp.OpenAICodex))
	if !slices.Equal(dropped, []string{"sawmill"}) || lists[2] == nil {
		t.Fatalf("dropped = %v (jevonsmcp kept=%v), want only sawmill", dropped, lists[2] != nil)
	}
}
