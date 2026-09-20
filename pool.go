// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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

// Acquire returns an idle agent from the warm pool that matches the
// given Config, or creates a new one if none is available.
//
// Pool key: hash of (workdir, model, disallowTools). Window names
// follow the pattern claudia-pool-<hash12>.
//
// Disposition at release time (via [*Agent.Release]):
//   - "return": clear the held marker, leave the window running.
//   - "drop": kill the window.
//   - "keep_alive_for": clear held, set a deadline for eviction.
//
// Config.PoolPolicy controls behaviour when all matching windows are
// held:
//   - "spawn" (default): create a new window.
//   - "wait": block until a window is released (not yet implemented;
//     treated as "spawn" in this release).
//   - "error": return an error.
//
// Config.PoolCap (0 = unlimited) caps the total number of idle pool
// windows for this key. When exceeded on Acquire the oldest idle
// window is evicted before a new one is created.
//
// When a claudia daemon is listening, the daemon runs the pool and the
// returned Agent is a handle onto the warm seat it granted (🎯T64): every
// consumer on the host draws from, and returns to, the same pool, and a
// consumer that goes away returns its seats. [AcquireDirect] keeps the
// pool in this process.
func Acquire(ctx context.Context, cfg Config) (*Agent, error) {
	if usingBroker() {
		a, err := acquireViaBroker(ctx, cfg)
		if err == nil || !brokerFellThrough(err) {
			return a, err
		}
	}
	return AcquireDirect(ctx, cfg)
}

// AcquireDirect is [Acquire] from the pool in this process even when a
// claudia daemon is listening.
func AcquireDirect(ctx context.Context, cfg Config) (*Agent, error) {
	if err := checkTmux(); err != nil {
		return nil, err
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "."
	}
	workDir, _ := filepath.Abs(cfg.WorkDir)
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}

	disallowed := "Agent,SendMessage,EnterWorktree"
	if len(cfg.DisallowTools) > 0 {
		disallowed += "," + strings.Join(cfg.DisallowTools, ",")
	}

	key := poolKeyFor(workDir, cfg.Model, disallowed)
	windowName := poolWindowPrefix + key

	policy := cfg.PoolPolicy
	if policy == "" {
		policy = "spawn"
	}

	poolMu.Lock()
	agent, err := acquireLocked(ctx, cfg, workDir, disallowed, windowName, policy)
	poolMu.Unlock()
	return agent, err
}

// acquireLocked runs with poolMu held. It lists windows, finds an
// idle match, and either adopts it or spawns a fresh one.
func acquireLocked(ctx context.Context, cfg Config, workDir, disallowed, windowName, policy string) (*Agent, error) {
	windows, err := tmuxagent.ListWindows()
	if err != nil {
		return nil, fmt.Errorf("pool list windows: %w", err)
	}

	now := poolClock.Now()

	// Collect all windows matching our pool key, categorised by
	// classifyPoolWindow: idle ones are adopted, held ones are left to
	// their holders, and expired and blind ones are swept.
	type candidate struct {
		windowID  string
		sessionID string
	}
	var idle, held, expired, blind []candidate

	for _, w := range windows {
		if w.Name != windowName {
			continue
		}
		heldVal, _ := tmuxagent.GetWindowOption(w.ID, "claudia-held")
		deadlineVal, hasDeadline := tmuxagent.GetWindowOption(w.ID, "claudia-deadline")
		sidVal, _ := tmuxagent.GetWindowOption(w.ID, poolSessionOption)

		sessionID := strings.TrimSpace(sidVal)
		c := candidate{windowID: w.ID, sessionID: sessionID}
		switch classifyPoolWindow(now, poolWindowState{
			held:        strings.TrimSpace(heldVal) == "1",
			deadline:    deadlineVal,
			hasDeadline: hasDeadline,
			sessionID:   sessionID,
		}) {
		case poolExpired:
			expired = append(expired, c)
		case poolHeld:
			held = append(held, c)
		case poolBlind:
			// Spawned before 🎯T78, so nothing on the host knows which
			// transcript it writes. The only window this sweep can race is
			// a sibling process between new-window and its first
			// SetWindowOption; that process marks held before the session
			// id, so the gap is a single tmux round-trip.
			blind = append(blind, c)
		default:
			idle = append(idle, c)
		}
	}

	// Sweep expired windows.
	for _, c := range expired {
		slog.Debug("pool: evicting expired window", "window", c.windowID)
		if killErr := tmuxagent.KillWindow(c.windowID); killErr != nil {
			slog.Warn("pool: kill expired window", "window", c.windowID, "err", killErr)
		}
	}

	// Sweep windows with no recorded session: unobservable, never adopted.
	for _, c := range blind {
		slog.Info("pool: evicting window with no recorded session id", "window", c.windowID)
		if killErr := tmuxagent.KillWindow(c.windowID); killErr != nil {
			slog.Warn("pool: kill unobservable window", "window", c.windowID, "err", killErr)
		}
	}

	// Apply pool cap: evict oldest idle windows if needed.
	if cfg.PoolCap > 0 {
		total := len(idle) + len(held)
		evictCount := total - cfg.PoolCap
		for i := 0; i < evictCount && i < len(idle); i++ {
			c := idle[i]
			slog.Debug("pool: evicting idle window (cap)", "window", c.windowID)
			if killErr := tmuxagent.KillWindow(c.windowID); killErr != nil {
				slog.Warn("pool: kill cap-evicted window", "window", c.windowID, "err", killErr)
			}
			idle = idle[1:]
		}
	}

	// Try to adopt an idle window.
	for _, c := range idle {
		// Mark as held before releasing the lock (after return).
		if err := tmuxagent.SetWindowOption(c.windowID, "claudia-held", "1"); err != nil {
			slog.Warn("pool: failed to mark window held", "window", c.windowID, "err", err)
			continue
		}

		agent, err := adoptWindow(ctx, cfg, workDir, c.windowID, c.sessionID)
		if err != nil {
			// Adoption failed — unmark and try next.
			slog.Warn("pool: adopt failed", "window", c.windowID, "err", err)
			_ = tmuxagent.SetWindowOption(c.windowID, "claudia-held", "0")
			continue
		}
		slog.Info("pool: warm acquire", "window", c.windowID)
		return agent, nil
	}

	// No idle window available — consult the policy.
	if len(idle) == 0 && len(held) > 0 {
		switch policy {
		case "error":
			return nil, fmt.Errorf("pool: all %d window(s) for this key are held and PoolPolicy=error", len(held))
		case "wait":
			// Fallthrough to spawn for now; a proper condition variable
			// would require unlocking, and the pool is process-local anyway.
			slog.Debug("pool: all windows held, spawning new (wait not fully implemented)")
		default: // "spawn"
		}
	}

	// Spawn a new pool window.
	return spawnPoolWindow(ctx, cfg, workDir, disallowed, windowName)
}

// spawnPoolWindow creates a new tmux window for the pool, waits for
// readiness, and returns a ready Agent marked as held.
func spawnPoolWindow(_ context.Context, cfg Config, workDir, disallowed, windowName string) (*Agent, error) {
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "bypassPermissions"
	}

	// A pool window is spawned with an id we choose, not one Claude mints
	// for itself, so the transcript it writes is at a path every later
	// Acquire can compute — in this process or the next one (🎯T78).
	sessionID := uuid.New().String()

	args := []string{
		"--permission-mode", cfg.PermissionMode,
		"--disallowedTools", disallowed,
		"--session-id", sessionID,
	}
	if cfg.MCPConfig != "" {
		args = append(args, "--mcp-config", cfg.MCPConfig)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, cfg.ExtraArgs...)

	claudeBin, err := resolveClaudeBin()
	if err != nil {
		return nil, err
	}
	windowID, err := tmuxagent.SpawnWindow(workDir, windowName, claudeBin, args)
	if err != nil {
		return nil, fmt.Errorf("pool spawn: %w", err)
	}

	// Mark as held immediately so concurrent Acquires don't grab it.
	if err := tmuxagent.SetWindowOption(windowID, "claudia-held", "1"); err != nil {
		_ = tmuxagent.KillWindow(windowID)
		return nil, fmt.Errorf("pool: set held on new window: %w", err)
	}
	// Record the session before the long WaitReady below: until this lands,
	// a sibling process's sweep sees a window it cannot observe.
	if err := tmuxagent.SetWindowOption(windowID, poolSessionOption, sessionID); err != nil {
		_ = tmuxagent.KillWindow(windowID)
		return nil, fmt.Errorf("pool: record session id on new window: %w", err)
	}

	// Wait for the claude TUI to reach its idle input state before
	// returning. This is the dominant latency for a cold acquire (~600–700ms)
	// and ensures that a subsequent warm re-acquire sees an already-ready
	// window and can return in <100ms.
	if _, waitErr := tmuxagent.WaitReady(windowID, readyPollInterval, readyOverallTimeout); waitErr != nil {
		_ = tmuxagent.KillWindow(windowID)
		return nil, fmt.Errorf("pool: window never became ready: %w", waitErr)
	}

	slog.Info("pool: cold spawn", "window", windowID)

	// Build and return the Agent (window is already ready).
	agent, err := buildPoolAgent(cfg, workDir, windowID, sessionID, false)
	if err != nil {
		_ = tmuxagent.KillWindow(windowID)
		return nil, err
	}
	return agent, nil
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
	if termLogPath == "-" {
		termLogPath = ""
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

	// Open terminal log if configured.
	if termLogPath != "" {
		if err := os.MkdirAll(filepath.Dir(termLogPath), 0o755); err != nil {
			slog.Warn("pool term log mkdir failed", "path", termLogPath, "err", err)
		} else if f, err := os.OpenFile(termLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			slog.Warn("pool term log open failed", "path", termLogPath, "err", err)
		} else {
			a.termLog = f
			a.termLogLive = true
		}
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
