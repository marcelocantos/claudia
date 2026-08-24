// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"io"
	"log/slog"
)

// ACP JSON-RPC is one JSON object per newline. Cursor session stores can
// replay tool results of several megabytes on session/load (jevons 🎯T545:
// overseer store.db has ~3.9 MiB blobs). A 1 MiB Scan cap treats that as
// EOF and surfaces as "connection closed waiting for session/load".
const acpMaxJSONLine = 16 << 20

func newACPLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), acpMaxJSONLine)
	return sc
}

func logACPScanErr(kind string, err error) {
	if err == nil {
		return
	}
	slog.Error("acp stdout scan stopped", "transport", kind, "err", err)
}
