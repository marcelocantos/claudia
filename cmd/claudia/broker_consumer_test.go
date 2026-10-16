// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/daemon"
)

func TestBrokerConsumerFlags(t *testing.T) {
	var opts daemon.Options
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	bindMCPConsumerFlags(fs, &opts)
	if err := fs.Parse(nil); err != nil || opts.MCPSeedStateDirs != nil || opts.MCPConsumerOwnedPrefixes != nil {
		t.Fatalf("defaults must be neutral: %+v, %v", opts, err)
	}
	if err := fs.Parse([]string{"-mcp-seed-dir", " /one ", "-mcp-seed-dir", "/two", "-mcp-consumer-prefix", " host- ", "-mcp-consumer-prefix", "other-"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opts.MCPSeedStateDirs, []string{"/one", "/two"}) ||
		!reflect.DeepEqual(opts.MCPConsumerOwnedPrefixes, []string{"host-", "other-"}) {
		t.Fatalf("explicit settings lost: %+v", opts)
	}
	for _, name := range []string{"-mcp-seed-dir", "-mcp-consumer-prefix"} {
		if err := fs.Parse([]string{name, " \t "}); err == nil {
			t.Fatalf("%s accepted an empty value", name)
		}
	}
}

func TestBrokerServeAdvertisesConsumerFlagsWithoutStarting(t *testing.T) {
	out := captureStderr(t, func() error {
		if err := serve([]string{"-help"}); err != flag.ErrHelp {
			t.Fatalf("serve help: %v", err)
		}
		return nil
	})
	for _, flag := range []string{"-mcp-seed-dir", "-mcp-consumer-prefix"} {
		if !strings.Contains(out, flag) {
			t.Fatalf("serve does not bind %s: %s", flag, out)
		}
	}
}
