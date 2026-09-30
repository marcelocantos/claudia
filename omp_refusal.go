// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marcelocantos/claudia/omp"
)

// explainOMPSidecarRefusal is origin PR #59's handshake explanation for the
// path this line starts Grok and Cursor on (🎯T163). #59 diagnosed an omp
// helper that the vendor CLI ran as a child; here the seat is loaded into
// the Oh My Pi sidecar over its socket, so a refusal arrives as the
// sidecar's error event and never passes through [explainGrokSidecarHandshake].
//
// err already carries the sidecar's own words. The explanation adds what
// the operator can do about them: which process refused, where its log and
// pid are, the PATH a supervised broker must hand it, and that restarting
// the broker service does not by itself restart the sidecar.
func explainOMPSidecarRefusal(err error, provider Provider, socket string) error {
	if err == nil {
		return nil
	}
	dir := filepath.Dir(socket)
	logPath := filepath.Join(dir, "omp-sidecar.log")
	var b strings.Builder
	fmt.Fprintf(&b, "The Oh My Pi sidecar refused to load this %s seat; its words are above. "+
		"Grok, Cursor and other subscription-plan seats run inside that sidecar, a Bun process (%s) the broker starts on first use, listening on %s and logging to %s; tmux Claude and Codex CLI seats do not use it. ",
		provider, omp.ServerScript(), socket, logPath)
	b.WriteString("The sidecar, and every command its seats run, inherits the broker's environment, so a refusal that names a missing command or file is usually the broker's PATH. " +
		"A supervised broker needs Homebrew (/opt/homebrew/bin), bun (~/.bun/bin) and the user tool directories (~/.grok/bin, ~/.local/bin, ~/go/bin, ~/.cargo/bin, ~/.py/bin) on its PATH. " +
		"supervisor/claudia.ini and supervisor/run-claudia.sh set that PATH (`make supervisor-install` renders them); a launchd or `brew services` broker runs on /usr/bin:/bin:/usr/sbin:/sbin unless its plist says otherwise. ")
	if bun, err := exec.LookPath("bun"); err == nil {
		fmt.Fprintf(&b, "On this broker's PATH, bun is %s.", bun)
	} else {
		b.WriteString("This broker's PATH has no bun.")
	}
	if missing := missingUserToolDirs(); len(missing) > 0 {
		fmt.Fprintf(&b, " Present on this machine but not on its PATH: %s.", strings.Join(missing, ", "))
	}
	fmt.Fprintf(&b, " Restart the broker service so the next grant is served with that PATH: `supervisorctl restart claudia` under supervisord, or `brew services restart claudia` for a Homebrew service. "+
		"The sidecar outlives a broker restart and keeps the environment it was started with, so after fixing the PATH also stop it (`kill $(cat %s.pid)`); the next sidecar grant starts a fresh one from the broker. "+
		"Every seat on the old sidecar loses its connection when it stops.", socket)
	if excerpt := ompSidecarLogExcerptAt(logPath); excerpt != "" {
		b.WriteString("\n\n" + excerpt)
	}
	return fmt.Errorf("%w\n\n%s", err, b.String())
}

// missingUserToolDirs are the user tool directories that exist on this
// machine but are not on this process's PATH — the ones a service PATH
// most often leaves out.
func missingUserToolDirs() []string {
	home, _ := os.UserHomeDir()
	onPath := filepath.SplitList(os.Getenv("PATH"))
	var out []string
	for _, dir := range grokUserToolDirs(home) {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		if !slices.Contains(onPath, dir) {
			out = append(out, dir)
		}
	}
	return out
}
