// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Claude Code (seen on v2.1.288 through v2.1.294) lists "❯ No, exit"
// FIRST in the workspace-trust dialog and focuses it, so the bare Enter
// 🎯T87 sent to "confirm the default" answered No and Claude exited
// (pimp-den-harness, 2026-10-08 21:21 AEDT: "startup menu ... still
// present after 3 auto-confirmations", claude rc=1). The fixtures are
// verbatim 80x24 `tmux capture-pane -p` frames from claude 2.1.294 in a
// fresh, untrusted git repo: before and after one Down keypress.

func TestTrustFolderStepCancelFirstFixtures(t *testing.T) {
	cancel := loadFrame(t, "frame_trust_folder_cancel_focused.txt")
	accept := loadFrame(t, "frame_trust_folder_accept_focused.txt")
	legacy := loadFrame(t, "frame_trust_folder.txt")

	for name, f := range map[string][]byte{"cancel": cancel, "accept": accept} {
		if !MatchTrustFolder(f) {
			t.Fatalf("%s: MatchTrustFolder = false on claude 2.1.294 trust chrome", name)
		}
		if MatchReady(f) {
			t.Fatalf("%s: trust dialog is not a live composer", name)
		}
	}
	cases := []struct {
		name  string
		frame []byte
		want  trustFolderMove
	}{
		{"2.1.294 cursor on No, exit", cancel, trustDown},
		{"2.1.294 cursor on Yes after Down", accept, trustConfirm},
		{"2.1.226 numbered, Yes first, no cursor drawn", legacy, trustConfirm},
		{"unnumbered cursor on Yes", []byte(trustFolderCursorUnnumbered), trustConfirm},
		{"Yes listed above a focused No", []byte(" Quick safety check: Is this a project you created or one you trust?\n\n   Yes, I trust this folder\n ❯ No, exit\n\n Enter to confirm · Esc to cancel\n"), trustUp},
		{"settings variant, focused decline", []byte(" Quick safety check: Is this a project you created or one you trust?\n\n ❯ No, continue without these permissions\n   Yes, I trust this folder\n"), trustDown},
		{"no cursor drawn, No listed first", []byte(" Quick safety check: Is this a project you created or one you trust?\n\n   No, exit\n   Yes, I trust this folder\n"), trustUnknown},
	}
	for _, c := range cases {
		if got := trustFolderStep(c.frame); got != c.want {
			t.Errorf("%s: trustFolderStep = %d, want %d", c.name, got, c.want)
		}
	}
}

// fakeTrustDialog models the 2.1.294 dialog: cursor starts on "No, exit";
// Down/Up move it; Enter on No exits Claude (the window goes away);
// Enter on Yes reaches the composer. dropKeys swallows that many arrow
// presses first, like a dialog still refusing input just after it opens.
type fakeTrustDialog struct {
	t          *testing.T
	cancel     []byte
	accept     []byte
	onYes      bool
	exited     bool
	ready      bool
	dropKeys   int
	enters     int
	keys       []string
	declineHit int
}

func (f *fakeTrustDialog) driver() readyDriver {
	return readyDriver{
		capture: func() ([]byte, error) {
			switch {
			case f.exited:
				return nil, errors.New("tmux capture-pane @9: can't find window: @9")
			case f.ready:
				return []byte(liveComposerFrame), nil
			case f.onYes:
				return f.accept, nil
			default:
				return f.cancel, nil
			}
		},
		sendEnter: func() error {
			f.enters++
			if f.ready || f.exited {
				return nil
			}
			if f.onYes {
				f.ready = true
			} else {
				f.declineHit++
				f.exited = true
			}
			return nil
		},
		sendKey: func(key string) error {
			f.keys = append(f.keys, key)
			if f.dropKeys > 0 {
				f.dropKeys--
				return nil
			}
			switch key {
			case "Down":
				f.onYes = true
			case "Up":
				f.onYes = false
			}
			return nil
		},
	}
}

func TestWaitReadyCancelFirstTrustWalksToYes(t *testing.T) {
	f := &fakeTrustDialog{
		t:      t,
		cancel: loadFrame(t, "frame_trust_folder_cancel_focused.txt"),
		accept: loadFrame(t, "frame_trust_folder_accept_focused.txt"),
	}
	if _, err := waitReadyLoop(f.driver(), time.Millisecond, 2*time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitReadyLoop: %v (keys %v, enters %d)", err, f.keys, f.enters)
	}
	if f.declineHit != 0 {
		t.Fatalf("pressed Enter on \"No, exit\" %d time(s): that exits Claude", f.declineHit)
	}
	if strings.Join(f.keys, ",") != "Down" || f.enters != 1 {
		t.Fatalf("keys = %v, enters = %d; want exactly Down then one Enter", f.keys, f.enters)
	}
}

func TestWaitReadyCancelFirstTrustRetriesDroppedKey(t *testing.T) {
	f := &fakeTrustDialog{
		t:        t,
		cancel:   loadFrame(t, "frame_trust_folder_cancel_focused.txt"),
		accept:   loadFrame(t, "frame_trust_folder_accept_focused.txt"),
		dropKeys: 2,
	}
	if _, err := waitReadyLoop(f.driver(), time.Millisecond, 2*time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitReadyLoop: %v (keys %v)", err, f.keys)
	}
	if f.declineHit != 0 {
		t.Fatalf("pressed Enter on \"No, exit\" %d time(s)", f.declineHit)
	}
	if len(f.keys) != 3 || f.enters != 1 {
		t.Fatalf("keys = %v, enters = %d; want three Downs (two dropped) then one Enter", f.keys, f.enters)
	}
}

// A cursor that never reaches the accept option must stay bounded and
// must still never be answered with Enter.
func TestWaitReadyCancelFirstTrustStuckIsBoundedAndNeverDeclines(t *testing.T) {
	f := &fakeTrustDialog{
		t:        t,
		cancel:   loadFrame(t, "frame_trust_folder_cancel_focused.txt"),
		accept:   loadFrame(t, "frame_trust_folder_accept_focused.txt"),
		dropKeys: 1 << 30,
	}
	_, err := waitReadyLoop(f.driver(), time.Millisecond, 100*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout when the trust cursor never moves")
	}
	if !strings.Contains(err.Error(), "trust-folder") {
		t.Fatalf("error should name the trust-folder prompt, got: %v", err)
	}
	if f.enters != 0 {
		t.Fatalf("pressed Enter %d time(s) with the cursor on \"No, exit\"", f.enters)
	}
	if len(f.keys) != maxTrustMoves {
		t.Fatalf("arrow presses = %d, want maxTrustMoves=%d", len(f.keys), maxTrustMoves)
	}
}
