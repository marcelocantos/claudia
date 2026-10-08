// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package codex implements a Task-style Codex agent that drives the headless
// `codex exec --json` CLI on ChatGPT-subscription auth.
//
// The public surface mirrors claudia Task mode: [NewTask] / [NewCodexTask]
// create a task, [Task.Run] spawns one `codex exec` process per prompt,
// streams structured [Event] values (init / text / tool_use / result / error),
// and captures the Codex thread id as [Task.SessionID].
//
// Failure modes are typed:
//
//   - [*AuthError] — subscription preflight failed, or JSONL/auth messaging
//   - [*RateLimitError] — throttle / 429 / usage-cap messaging
//   - [*ExitError] — non-zero process exit without a prior structured error
//
// Binary resolution: CODEX_BIN, then known install locations (user-local
// and Homebrew bin dirs, and the CLI bundled in ChatGPT.app at
// Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex), then PATH.
//
// Live residual: set CLAUDIA_CODEX_LIVE=1 (or CLAUDIA_LIVE=1) to run the
// real-subscription smoke in task_live_test.go.
package codex
