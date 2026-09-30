// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 🎯T167: every renewal of a plan's login is attributable from claudia.log
// alone. On 2026-09-30 the login was lost three times, and seat-level
// refreshes, which rotate the token under every seat, logged nothing.
func TestT167EveryRenewalNamesItsCaller(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fresh := `{"refresh_token":"r2","access_token":"a2","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
	login := Login{Script: "auth.ts", Caller: "seat jv-x refused", Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(fresh), nil
	}}
	if _, err := login.fetch(context.Background(), Anthropic, Record{RefreshToken: "r1", AccessToken: "a1"}); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{`msg="omp plan login renewed"`, "provider=anthropic", "verb=refresh", `caller="seat jv-x refused"`, "pid=" + strconv.Itoa(os.Getpid())} {
		if !strings.Contains(line, want) {
			t.Fatalf("renewal log %q lacks %s", line, want)
		}
	}

	// A path that forgot to say who it is shows up as such, not as silence.
	buf.Reset()
	login.Caller = ""
	if _, err := login.fetch(context.Background(), Anthropic, Record{RefreshToken: "r1", AccessToken: "a1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "caller=unattributed") {
		t.Fatalf("an unnamed renewal was not marked: %q", buf.String())
	}
}

// 🎯T167: only the broker can rotate the shared refresh token. The sidecar is
// structurally incapable: nothing the broker sends it can carry one.
func TestT167SidecarNeverReceivesARefreshToken(t *testing.T) {
	typ := reflect.TypeOf(Message{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
		if strings.Contains(name, "refresh") || strings.Contains(name, "record") || strings.Contains(name, "grant") {
			t.Fatalf("omp.Message field %s could carry a refresh token to the sidecar", f.Name)
		}
		if f.Type == reflect.TypeOf(Record{}) || f.Type == reflect.TypeOf(&Record{}) {
			t.Fatalf("omp.Message field %s carries a whole plan record", f.Name)
		}
	}
}
