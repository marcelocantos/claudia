// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// claudiad is the optional backing daemon for the claudia library.
//
// Slice 1 of target 🎯T1 (see docs/targets.yaml) provides a minimal
// session-chain tracker: the claudia library opportunistically
// reports each claude session it spawns, and the daemon watches
// ~/.claude/projects/ for new JSONL files so it can attribute
// /clear rollovers to the owning process deterministically. Session
// chains are queryable via claudia.LookupChain(sid).
//
// Slices 2–4 will add a warm agent pool, a live observability
// refactor, and packaging. Slice 1 deliberately implements the
// smallest useful surface to validate the daemon plumbing.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/marcelocantos/claudia/internal/daemon"
	"github.com/marcelocantos/claudia/internal/daemonproto"
)

const helpAgent = `claudiad — optional backing daemon for the claudia library.

This is Slice 1 of the daemon work: a session-chain tracker that
watches ~/.claude/projects/ for /clear rollovers and exposes chains
via JSON-RPC 2.0 over a Unix socket at $XDG_STATE_HOME/claudia/claudiad.sock
(defaulting to ~/.local/state/claudia/claudiad.sock).

When the daemon is running, claudia.Start() opportunistically reports
each spawned claude session and claudia.LookupChain(sid) returns the
full chain of session IDs reachable via /clear rollover.

When the daemon is NOT running, claudia operates identically to prior
releases — the daemon is an enhancement, not a dependency.

Usage:
  claudiad [--socket PATH] [--projects-dir DIR]

Flags:
  --socket PATH         Override the Unix socket path.
  --projects-dir DIR    Override the Claude Code projects directory
                        (default: ~/.claude/projects).
  --version             Print the daemon version and exit.
  --help                Print this message.
  --help-agent          Print this message plus an agent-oriented guide.

Signals:
  SIGTERM, SIGINT       Graceful shutdown (socket removed on exit).

Agent notes:
  - The daemon has no on-disk state. Restarting it drops all tracked
    chains; consumers will re-register on their next claudia.Start().
  - The socket's parent directory is chmod'd 0700 and the socket file
    itself is chmod'd 0600 — access control is filesystem permissions
    only. Do not expose the socket over a network.
  - Slice 1 does not include pooling, live agent routing, or the API
    refactor. If claudia.Start or Agent methods misbehave, the daemon
    is almost certainly not the cause — all of that still happens in
    the consumer process.
`

// Version is the daemon's advertised version string. Kept in sync
// with the module release via /release.
const Version = "0.6.0-dev"

func main() {
	var (
		socketPath  = flag.String("socket", "", "Unix socket path (default: $XDG_STATE_HOME/claudia/claudiad.sock)")
		projectsDir = flag.String("projects-dir", "", "Claude Code projects dir (default: ~/.claude/projects)")
		showVersion = flag.Bool("version", false, "print version and exit")
		helpFull    = flag.Bool("help-agent", false, "print the agent-oriented help guide")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, helpAgent)
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("claudiad %s\n", Version)
		return
	}
	if *helpFull {
		fmt.Print(helpAgent)
		return
	}

	srv, err := daemon.NewServer(daemon.Config{
		SocketPath:  *socketPath,
		ProjectsDir: *projectsDir,
	})
	if err != nil {
		slog.Error("claudiad: init", "err", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		slog.Error("claudiad: start", "err", err)
		os.Exit(1)
	}

	sock := srv.SocketPath()
	if sock == "" {
		sock = daemonproto.SocketPath()
	}
	slog.Info("claudiad listening", "socket", sock, "version", Version)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	slog.Info("claudiad: shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(shutdownCtx); err != nil {
		slog.Warn("claudiad: shutdown error", "err", err)
	}
}
