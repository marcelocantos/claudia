// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia_test

import (
	"regexp"
	"testing"

	"github.com/marcelocantos/claudia"
)

func TestVersion(t *testing.T) {
	if claudia.Version == "" {
		t.Fatal("Version is empty")
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(claudia.Version) {
		t.Fatalf("Version %q does not match semver pattern", claudia.Version)
	}
}
