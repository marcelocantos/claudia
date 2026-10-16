// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"strings"
	"testing"
)

func TestGenerationDiscoveryPageValidation(t *testing.T) {
	good := GenerationPage{Generations: []GenerationIdentity{{Seat: "seat", SessionID: "generation"}}, Issues: []GenerationIssue{}, Cursor: "next", More: true}
	if err := good.Validate("prior", 1); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*GenerationPage){
		func(p *GenerationPage) { p.Cursor = "prior" }, func(p *GenerationPage) { p.Complete = true }, func(p *GenerationPage) { p.Generations = nil },
		func(p *GenerationPage) { p.Cursor = strings.Repeat("x", GenerationCursorLimit+1) },
		func(p *GenerationPage) {
			p.Issues = []GenerationIssue{{Code: "bad", Message: strings.Repeat("x", GenerationIssueLimit+1)}}
		},
	} {
		p := good
		mutate(&p)
		if p.Validate("prior", 1) == nil {
			t.Fatalf("accepted malformed page %+v", p)
		}
	}
}
