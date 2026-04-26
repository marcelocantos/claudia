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

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

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
func Acquire(ctx context.Context, cfg Config) (*Agent, error) {
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

	disallowed := "Agent,TeamCreate,TeamDelete,SendMessage,EnterWorktree"
	if cfg.DisallowTools != "" {
		disallowed += "," + cfg.DisallowTools
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

	now := time.Now().Unix()

	// Collect all windows matching our pool key, categorised:
	//   - idle: not held and not expired
	//   - held: currently in use
	//   - expired: deadline set and past
	type candidate struct {
		windowID string
	}
	var idle, held, expired []candidate

	for _, w := range windows {
		if w.Name != windowName {
			continue
		}
		heldVal, _ := tmuxagent.GetWindowOption(w.ID, "claudia-held")
		isHeld := strings.TrimSpace(heldVal) == "1"

		deadlineVal, hasDeadline := tmuxagent.GetWindowOption(w.ID, "claudia-deadline")
		isExpired := false
		if hasDeadline {
			dl, dlErr := strconv.ParseInt(strings.TrimSpace(deadlineVal), 10, 64)
			if dlErr == nil && dl > 0 && now >= dl {
				isExpired = true
			}
		}

		switch {
		case isExpired:
			expired = append(expired, candidate{w.ID})
		case isHeld:
			held = append(held, candidate{w.ID})
		default:
			idle = append(idle, candidate{w.ID})
		}
	}

	// Sweep expired windows.
	for _, c := range expired {
		slog.Debug("pool: evicting expired window", "window", c.windowID)
		if killErr := tmuxagent.KillWindow(c.windowID); killErr != nil {
			slog.Warn("pool: kill expired window", "window", c.windowID, "err", killErr)
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

		agent, err := adoptWindow(ctx, cfg, workDir, c.windowID)
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

	args := []string{
		"--permission-mode", cfg.PermissionMode,
		"--disallowedTools", disallowed,
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
	agent, err := buildPoolAgent(cfg, workDir, windowID, false)
	if err != nil {
		_ = tmuxagent.KillWindow(windowID)
		return nil, err
	}
	return agent, nil
}

// adoptWindow dials control mode and verifies readiness for an
// existing pool window, then wraps it in an Agent.
func adoptWindow(ctx context.Context, cfg Config, workDir, windowID string) (*Agent, error) {
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

	return buildPoolAgent(cfg, workDir, windowID, false)
}

// buildPoolAgent constructs an Agent wrapping an existing (or newly
// spawned) tmux window. If waitForReady is true it waits for the TUI
// ready pattern; if false the window is assumed already ready.
func buildPoolAgent(cfg Config, workDir, windowID string, waitForReady bool) (*Agent, error) {
	termLogPath := cfg.TermLogPath
	if termLogPath == "-" {
		termLogPath = ""
	}
	// Pool agents don't have a session ID yet at adoption time; a
	// session ID will be set when the consumer actually sends a prompt
	// and claude creates its JSONL. We use the window ID as a
	// placeholder for logs.
	placeholderID := "pool-" + windowID

	a := &Agent{
		sessionID:    placeholderID,
		jsonlPath:    "", // populated on first send when Claude writes it
		termLogPath:  termLogPath,
		tmuxWindowID: windowID,
		alive:        true,
		ready:        make(chan struct{}),
		poolWindow:   true,
		poolWorkDir:  workDir,
	}

	// Open terminal log if configured.
	if termLogPath != "" {
		if err := os.MkdirAll(filepath.Dir(termLogPath), 0o755); err != nil {
			slog.Warn("pool term log mkdir failed", "path", termLogPath, "err", err)
		} else if f, err := os.OpenFile(termLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			slog.Warn("pool term log open failed", "path", termLogPath, "err", err)
		} else {
			a.termLog = f
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
		a.mu.Lock()
		a.alive = false
		a.mu.Unlock()
	}()

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
	windowID := a.tmuxWindowID

	switch {
	case disposition == "return":
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
		deadline := time.Now().Add(time.Duration(secs) * time.Second).Unix()

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
