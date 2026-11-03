// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package claudia

import "os/exec"

// setCheckProcessGroup kills only the script on Windows, which has no process
// groups of this kind.
func setCheckProcessGroup(cmd *exec.Cmd) {}
