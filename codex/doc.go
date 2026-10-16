// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package codex parses historical Codex CLI events and retains compatibility
// types for callers of the removed Task CLI transport. NewTask and NewCodexTask
// construct inert handles; Task.Run returns ErrTaskCLIRemoved without resolving
// credentials, finding a binary, or starting a process. No OMP Task replacement
// is provided (T192; T190). Interactive seats use claudia's provider API transport.
package codex
