// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// This file is broker policy: it decides when a warm pool window has expired
// and how long a released window stays alive. Those are exactly the timing
// decisions 🎯T2.5 (idle reaping) and 🎯T2.3 (pool drain) must be able to verify
// deterministically, so the file reads time through the injected Clock seam
// rather than the wall clock. The marker below enrols it in the policy guard
// (internal/broker/policy_guard_test.go), which fails the build if any
// time.Now/After/Sleep call reappears here.
//
//claudia:policy
var poolClock broker.Clock = broker.SystemClock{}

// poolWindowExpired reports whether a window's @claudia-deadline option, as
// stored by a keep_alive_for release, has passed at now. A missing, malformed,
// or non-positive deadline means "no deadline", never "expired" — a parse slip
// must not become a licence to kill a live window.
func poolWindowExpired(now time.Time, deadlineVal string, hasDeadline bool) bool {
	if !hasDeadline {
		return false
	}
	dl, err := strconv.ParseInt(strings.TrimSpace(deadlineVal), 10, 64)
	if err != nil || dl <= 0 {
		return false
	}
	return now.Unix() >= dl
}

// poolKeepAliveDeadline is the Unix deadline a keep_alive_for:<secs> release
// stamps on a window: the eviction sweep of the first Acquire at or after this
// instant kills it.
func poolKeepAliveDeadline(now time.Time, secs int64) int64 {
	return now.Add(time.Duration(secs) * time.Second).Unix()
}

// poolDisposition is what an Acquire sweep does with one window of its
// pool key. The decision is pure so the rules can be read, and tested,
// without a tmux server (compare poolWindowExpired above).
type poolDisposition int

const (
	// poolIdle: warm, unheld, observable — the window Acquire wants.
	poolIdle poolDisposition = iota
	// poolHeld: another consumer has it.
	poolHeld
	// poolExpired: a keep_alive_for deadline has passed; kill it.
	poolExpired
	// poolBlind: no recorded session id, so nothing can discover the
	// transcript it writes. Kill it rather than hand a consumer a seat
	// whose WaitForResponse can never return (🎯T78).
	poolBlind
)

// poolWindowState is what a sweep reads off one tmux window.
type poolWindowState struct {
	held        bool
	deadline    string
	hasDeadline bool
	sessionID   string
}

// classifyPoolWindow decides one window's fate. Order matters: an expired
// window is killed whether or not it is held, because its holder asked for
// exactly that; a held window is otherwise left alone, blind or not,
// because its holder is mid-turn and killing it would take the turn with
// it.
func classifyPoolWindow(now time.Time, st poolWindowState) poolDisposition {
	switch {
	case poolWindowExpired(now, st.deadline, st.hasDeadline):
		return poolExpired
	case st.held:
		return poolHeld
	case strings.TrimSpace(st.sessionID) == "":
		return poolBlind
	default:
		return poolIdle
	}
}

// poolMu serialises pool operations within this process. tmux itself
// serialises operations server-side, but we need the check-then-set
// on @claudia-held to be atomic from our perspective: we mark a window
// held before another goroutine in the same process can snatch it.
var poolMu sync.Mutex

// poolKeyFor computes the pool key for a given config. The key is the
// first 12 hex characters of SHA-256(workdir + model + disallowTools).
func poolKeyFor(workDir, model, disallowTools string) string {
	h := sha256.Sum256([]byte(workDir + "\x00" + model + "\x00" + disallowTools))
	return fmt.Sprintf("%x", h[:6]) // 6 bytes = 12 hex chars
}

// poolWindowPrefix is the name prefix for pool-managed windows.
const poolWindowPrefix = "claudia-pool-"

// poolSessionOption is the tmux window option that records the Claude
// session id a pool window was spawned with. It is the same option Start
// stamps on a session window, and it is what lets a later Acquire — in
// this process or another one — find the transcript Claude is writing and
// tail it (🎯T78). A pool window without it cannot be observed at all.
const poolSessionOption = "claudia-session-id"

// Acquire refuses to check out a CLI pool seat. The warm pool minted a
// tmux window; that transport is gone. The error is
// [ErrLegacyCLISeatTransport]. [AcquireDirect] is the same refusal in
// this process.
func Acquire(ctx context.Context, cfg Config) (*Agent, error) {
	return nil, fmt.Errorf("%w: pool", ErrLegacyCLISeatTransport)
}

// AcquireDirect is [Acquire] from the pool in this process even when a
// claudia daemon is listening. The pool no longer mints a CLI window.
func AcquireDirect(ctx context.Context, cfg Config) (*Agent, error) {
	return nil, fmt.Errorf("%w: pool", ErrLegacyCLISeatTransport)
}

// acquireLocked used to run with poolMu held: list windows, adopt an
// idle match, or spawn. It refuses before any of that. Nothing in this
// package calls it; the body stays so a future caller cannot exec.
func acquireLocked(ctx context.Context, cfg Config, workDir, disallowed, windowName, policy string) (*Agent, error) {
	return nil, fmt.Errorf("%w: pool", ErrLegacyCLISeatTransport)
}

// spawnPoolWindow refuses. It used to create a tmux window, wait for
// readiness, and return a held Agent.
func spawnPoolWindow(_ context.Context, cfg Config, workDir, disallowed, windowName string) (*Agent, error) {
	return nil, fmt.Errorf("%w: pool", ErrLegacyCLISeatTransport)
}

// adoptWindow dials control mode and verifies readiness for an
// existing pool window, then wraps it in an Agent.
func adoptWindow(ctx context.Context, cfg Config, workDir, windowID, sessionID string) (*Agent, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("window %s has no recorded @%s", windowID, poolSessionOption)
	}
	// Verify the window is still alive.
	if !tmuxagent.IsWindowAlive(windowID) {
		return nil, fmt.Errorf("window %s is no longer alive", windowID)
	}

	// Check readiness via capture-pane (should be near-instant for a
	// warm window that's already at the idle input box).
	frame, err := tmuxagent.CapturePane(windowID)
	if err != nil {
		return nil, fmt.Errorf("capture-pane: %w", err)
	}
	if !tmuxagent.MatchReady(frame) {
		// The window may be mid-response from a prior session. Try a
		// brief wait (up to ~2s) before giving up.
		_, waitErr := tmuxagent.WaitReady(windowID, 50*time.Millisecond, 2*time.Second)
		if waitErr != nil {
			return nil, fmt.Errorf("warm window not ready: %w", waitErr)
		}
	}
	_ = ctx // reserved for future cancellation

	return buildPoolAgent(cfg, workDir, windowID, sessionID, false)
}

// poolTailOffset is where this holder's event stream begins: the end of
// the pool window's transcript at the moment it was acquired. Everything
// before it belongs to a previous holder of the same window, and a new
// holder must not be handed that conversation as if it were its own
// (🎯T78). A transcript that does not exist yet has nothing to skip.
//
// An unreadable transcript is 0, not an error: a pool window whose events
// start one turn too early is a bug worth reporting, but a pool window
// that refuses to be acquired because of a stat is worse.
func poolTailOffset(jsonlPath string) int64 {
	fi, err := os.Stat(jsonlPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("pool: cannot measure transcript, tailing from the start",
				"path", jsonlPath, "err", err)
		}
		return 0
	}
	return fi.Size()
}

// buildPoolAgent constructs an Agent wrapping an existing (or newly
// spawned) tmux window running the Claude session sessionID. If
// waitForReady is true it waits for the TUI ready pattern; if false the
// window is assumed already ready.
//
// The agent tails that session's transcript and publishes Events exactly
// as a Start-ed agent does, so WaitForResponse and subscribers work on a
// pooled seat (🎯T78). It tails from the transcript's current end, which
// is what keeps a re-acquired window's event stream this holder's own.
func buildPoolAgent(cfg Config, workDir, windowID, sessionID string, waitForReady bool) (*Agent, error) {
	termLogPath := cfg.TermLogPath
	switch termLogPath {
	case "", "-":
		termLogPath = ""
	default:
		return nil, fmt.Errorf("%w: term log", ErrLegacyCLISeatTransport)
	}
	jsonlPath := SessionJSONLPath(sessionID, workDir)
	tailFrom := poolTailOffset(jsonlPath)

	a := &Agent{
		provider:     ProviderClaude,
		sessionID:    sessionID,
		jsonlPath:    jsonlPath,
		termLogPath:  termLogPath,
		tmuxWindowID: windowID,
		ops:          claudeAgentOps(),
		alive:        true,
		ready:        make(chan struct{}),
		poolWindow:   true,
		poolWorkDir:  workDir,
		model:        cfg.Model,
		eventSubs:    make(map[int64]EventFunc),
	}

	// Dial control mode for terminal byte stream.
	ctrl, err := tmuxagent.DialControl(windowID)
	if err != nil {
		return nil, fmt.Errorf("pool control-mode: %w", err)
	}
	a.tmuxCtrl = ctrl

	go func() {
		for data := range ctrl.Bytes() {
			a.pushTermOutput(data)
		}
		a.markDead()
	}()

	// Observe the seat the way Start observes one: the transcript is the
	// event stream WaitForResponse reads, and the pane poll supplies the
	// provisional ⏺ preview (🎯T51).
	go a.tailJSONLFrom(tailFrom)

	preview := &tuiPreviewTracker{}
	// A Start-ed window's pane is empty; a re-acquired pool window's pane
	// still shows the previous holder's ⏺ blocks. resetTurn arms the
	// tracker to take its baseline from the first frame it sees, so those
	// blocks are the floor rather than a preview published to this holder.
	preview.resetTurn("")
	a.tuiPreview = preview
	a.capturePane = func() (string, error) {
		b, err := tmuxagent.CapturePane(a.tmuxWindowID)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	go a.pollTUIPreview()

	if waitForReady {
		go a.detectReady()
	} else {
		// Window is already ready — close the channel immediately.
		close(a.ready)
	}

	return a, nil
}

// Release returns or disposes of the agent according to disposition:
//   - "return": clear the held marker and leave the window warm for
//     the next Acquire.
//   - "drop": kill the window.
//   - "keep_alive_for:<seconds>": clear held, set @claudia-deadline so
//     the window is evicted by the next Acquire sweep after the TTL.
//
// Release closes the control-mode connection in all cases. For "drop"
// it also closes any open term log.
func (a *Agent) Release(disposition string) error {
	if a.ops.release != nil {
		// A daemon-held pooled seat: the daemon runs the pool.
		return a.ops.release(a, disposition)
	}
	windowID := a.tmuxWindowID

	switch {
	case disposition == "return":
		// Stop observing before the window is offered to anyone else: the
		// tailer and the pane poll outlive the control client, and a
		// returned handle that keeps publishing is the next holder's turn
		// arriving on the previous holder's subscribers (🎯T78).
		a.stopPoolObservers()
		// Close the control client but leave the window alive.
		if a.tmuxCtrl != nil {
			a.tmuxCtrl.Close()
		}
		if err := tmuxagent.SetWindowOption(windowID, "claudia-held", "0"); err != nil {
			return fmt.Errorf("pool: clear held: %w", err)
		}
		slog.Info("pool: window returned", "window", windowID)
		return nil

	case disposition == "drop":
		// Full teardown.
		a.Stop()
		slog.Info("pool: window dropped", "window", windowID)
		return nil

	case strings.HasPrefix(disposition, "keep_alive_for:"):
		secsStr := strings.TrimPrefix(disposition, "keep_alive_for:")
		secs, err := strconv.ParseInt(secsStr, 10, 64)
		if err != nil || secs <= 0 {
			return fmt.Errorf("pool: invalid keep_alive_for seconds %q", secsStr)
		}
		deadline := poolKeepAliveDeadline(poolClock.Now(), secs)

		a.stopPoolObservers()
		if a.tmuxCtrl != nil {
			a.tmuxCtrl.Close()
		}
		if err := tmuxagent.SetWindowOption(windowID, "claudia-held", "0"); err != nil {
			return fmt.Errorf("pool: clear held for keep_alive_for: %w", err)
		}
		if err := tmuxagent.SetWindowOption(windowID, "claudia-deadline", strconv.FormatInt(deadline, 10)); err != nil {
			return fmt.Errorf("pool: set deadline: %w", err)
		}
		slog.Info("pool: window kept alive with deadline",
			"window", windowID, "deadline", time.Unix(deadline, 0))
		return nil

	default:
		return fmt.Errorf("pool: unknown disposition %q (want: return, drop, keep_alive_for:<secs>)", disposition)
	}
}

// stopPoolObservers retires this handle's transcript tailer and pane
// poll. Both loops compare the generation they started under against the
// agent's current one on every pass, so bumping it is how a goroutine
// that is mid-sleep learns it is no longer this window's observer.
func (a *Agent) stopPoolObservers() {
	a.backendGen.Add(1)
}
