// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command t28size answers the load-bearing 🎯T28 questions empirically:
// at what payload size does Claude Code collapse a bracketed paste into
// an unexpanded "[Pasted text #N +X lines]" chip, does Enter on such a
// chip submit the turn, and what does the pane look like AFTERWARDS.
//
// The default sweep isolates chip FORMATION from submission. Each case
// is pasted into an IDLE composer with load-buffer + paste-buffer -p -r
// — exactly the bytes tmuxagent.pasteViaBuffer sends — and captured
// WITHOUT pressing Enter. The composer is cleared before the next case.
// Nothing is submitted, so a run costs one session start and no turns.
//
// With -submit, each case is followed by Enter presses (bounded, as the
// send path bounds them) and the run reports how many it took for the
// payload to leave the composer, or that it never did. That costs one
// real turn per case, so keep the case list short.
//
// With -midturn, a long-running turn is started first, so cases are
// pasted into a BUSY composer — the daily path for fleet nudges, event
// pushes and spawn briefs, and the population the 🎯T28 daily-path
// failures come from. Post-submit it reports the two chip verdicts
// separately: framechip (what MatchUnsubmittedPaste answers today, over
// the whole frame) and bodychip (the same question asked of the
// composer box alone). They disagree exactly when the chip glyph is
// transcript echo of a payload that DID submit rather than payload
// still held back — the 🎯T28 over-broadness.
//
// The verdict rests on pane frames only: the transcript is not a
// delivery oracle (jevons 🎯T417).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

func main() {
	workdir := flag.String("workdir", ".", "workdir for the probe session")
	dump := flag.String("dump", "", "directory to write captured frames into")
	settle := flag.Duration("settle", 1500*time.Millisecond, "how long to let the paste render before capture")
	byteSizes := flag.String("bytes", "100,200,300,350,380,400,500,800,1600,3200,6400", "single-line payload sizes to sweep")
	lineCounts := flag.String("lines", "1,2,3,4,5,6,8,12,20,40", "line counts to sweep, each line ~40 chars")
	submit := flag.Bool("submit", false, "press Enter after each paste and report how many presses submitted it (costs one real turn per case)")
	midturn := flag.Bool("midturn", false, "start a long-running turn first so cases are pasted into a BUSY composer (implies -submit)")
	brief := flag.Bool("brief", false, "add one case shaped like a real standing brief: markdown, a table, multi-KB")
	flag.Parse()
	if *midturn {
		*submit = true
	}

	if *dump != "" {
		if err := os.MkdirAll(*dump, 0o755); err != nil {
			die("mkdir dump: %v", err)
		}
	}

	a, err := claudia.Start(claudia.Config{Provider: claudia.ProviderClaude, WorkDir: *workdir})
	if err != nil {
		die("start: %v", err)
	}
	defer a.Stop()

	windowID := windowIDFrom(a.AttachCommand())
	fmt.Printf("session=%s window=%s\n\n", a.SessionID(), windowID)

	cases := buildCases(*byteSizes, *lineCounts, *brief)

	fmt.Printf("%-16s %8s %6s %6s  %-5s %-5s %-8s %-9s %-8s %-6s %s\n",
		"case", "bytes", "lines", "body", "chip", "held", "submit",
		"framechip", "bodychip", "queued", "note")
	for _, c := range cases {
		if *midturn {
			if err := ensureBusyTurn(windowID, *settle); err != nil {
				die("busy turn: %v", err)
			}
		}
		r := probe(windowID, c, *settle, *dump, *submit, *midturn)
		fmt.Printf("%-16s %8d %6d %6d  %-5v %-5v %-8s %-9v %-8v %-6v %s\n",
			c.name, len(c.payload), strings.Count(c.payload, "\n")+1, r.bodyLines,
			r.chip, r.held, r.submit, r.frameChip, r.bodyChip, r.queued, r.note)
	}
}

// busyPrompt starts a turn long enough to still be running while the
// next case is pasted and submitted. It asks for output, not thought,
// so the turn is reliably slow without depending on model latency.
const busyPrompt = "Count from 1 to 400, printing one number per line and nothing else."

// ensureBusyTurn guarantees a turn is running before the next case is
// pasted. The running state is read from the pane, never assumed:
// pasting into a composer that has gone idle again would measure the
// idle path while claiming to measure the busy one.
func ensureBusyTurn(windowID string, settle time.Duration) error {
	frame, err := tmuxagent.CapturePane(windowID)
	if err == nil && tmuxagent.MatchTurnInProgress(frame) {
		return nil
	}
	if err := tmuxagent.SendKeys(windowID, busyPrompt); err != nil {
		return fmt.Errorf("send busy prompt: %w", err)
	}
	for range 20 {
		frame, err := tmuxagent.CapturePane(windowID)
		if err == nil && tmuxagent.MatchTurnInProgress(frame) {
			return nil
		}
		time.Sleep(settle)
	}
	return fmt.Errorf("no turn chrome after busy prompt")
}

type probeCase struct {
	name    string
	payload string
}

type probeResult struct {
	chip      bool   // composer body holds a collapsed "[Pasted text #N" chip
	held      bool   // composer body holds the case's literal marker text
	submit    string // -submit only: presses it took to leave the composer, or "stuck"
	bodyLines int
	// The two chip verdicts on the FINAL post-submit frame, which is the
	// frame the send path classifies. frameChip is what
	// MatchUnsubmittedPaste answers today (whole frame); bodyChip asks the
	// same question of the composer box alone. They disagree exactly when
	// the chip glyph is transcript echo rather than held-back payload —
	// the 🎯T28 over-broadness.
	frameChip bool
	bodyChip  bool
	queued    bool // composer shows Claude Code's queued-messages hint
	note      string
}

// buildCases builds two families so bytes and lines can be told apart:
// "bytes/N" is a single line of N bytes, "lines/M" is M short lines.
// With brief set, one more case is shaped like a real standing brief —
// the population that actually fails on the daily path.
func buildCases(byteSizes, lineCounts string, brief bool) []probeCase {
	var cases []probeCase
	for i, f := range strings.Split(byteSizes, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(f, "%d", &n); err != nil {
			die("bad -bytes entry %q: %v", f, err)
		}
		marker := fmt.Sprintf("T28SIZEB%02d", i)
		body := marker + " " + strings.Repeat("x", max(0, n-len(marker)-1))
		cases = append(cases, probeCase{name: fmt.Sprintf("bytes/%d", n), payload: body})
	}
	for i, f := range strings.Split(lineCounts, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		var m int
		if _, err := fmt.Sscanf(f, "%d", &m); err != nil {
			die("bad -lines entry %q: %v", f, err)
		}
		marker := fmt.Sprintf("T28SIZEL%02d", i)
		var b strings.Builder
		b.WriteString(marker)
		for j := 1; j < m; j++ {
			fmt.Fprintf(&b, "\npadding line %02d of the %s case", j, marker)
		}
		cases = append(cases, probeCase{name: fmt.Sprintf("lines/%d", m), payload: b.String()})
	}
	if brief {
		cases = append(cases, probeCase{name: "brief", payload: briefPayload()})
	}
	return cases
}

// briefPayload reproduces the shape of a fleet standing brief: markdown
// headings, a table, and several KB — the exact population the 🎯T28
// daily-path failures come from.
func briefPayload() string {
	var b strings.Builder
	b.WriteString("T28SIZEBRIEF — do not act on this, it is a delivery probe.\n\n")
	b.WriteString("# Standing brief\n\n| mode | error string | what it means |\n|---|---|---|\n")
	for i := range 6 {
		fmt.Fprintf(&b, "| mode-%d | turn not submitted: composer state=paste_chip | the payload reached the pane and was collapsed |\n", i)
	}
	b.WriteString("\n## Evidence\n\n")
	for i := range 24 {
		fmt.Fprintf(&b, "- item %02d: a paragraph of the kind a real brief carries, long enough to matter for size.\n", i)
	}
	return b.String()
}

// probe pastes one case into the composer, captures the frame without
// submitting, and — with submit set — presses Enter until the payload
// leaves the box, which is the question the chip hypothesis actually
// turns on. The post-submit frame is captured and classified both ways.
func probe(windowID string, c probeCase, settle time.Duration, dump string, submit, busy bool) probeResult {
	if err := clearComposer(windowID, busy); err != nil {
		return probeResult{note: "pre-clear: " + err.Error()}
	}
	if err := pasteBuffer(windowID, c.payload); err != nil {
		return probeResult{note: "paste: " + err.Error()}
	}
	time.Sleep(settle)
	frame, err := tmuxagent.CapturePane(windowID)
	if err != nil {
		return probeResult{note: "capture: " + err.Error()}
	}
	dumpFrame(dump, c.name, "", frame)
	body := composerBody(frame)
	r := probeResult{
		chip:      strings.Contains(string(body), "[Pasted text #"),
		held:      strings.Contains(string(body), markerOf(c)),
		bodyLines: strings.Count(string(body), "\n") + 1,
	}
	if body == nil {
		r.bodyLines = 0
		r.note = "no composer box at frame tail"
	}
	if submit {
		verdict, final := submitPresses(windowID, c, settle, dump)
		r.submit = verdict
		finalBody := composerBody(final)
		r.frameChip = tmuxagent.MatchUnsubmittedPaste(final)
		r.bodyChip = strings.Contains(string(finalBody), "[Pasted text #")
		r.queued = queuedHintRe.Match(finalBody)
		dumpFrame(dump, c.name, "_final", final)
	}
	if err := clearComposer(windowID, busy); err != nil {
		r.note = strings.TrimSpace(r.note + " post-clear: " + err.Error())
	}
	return r
}

// queuedHintRe is Claude Code's ghost hint for a message accepted behind
// a running turn — the CLI's own statement that the payload left the
// composer (tmuxagent.queuedMessagesHint).
var queuedHintRe = regexp.MustCompile(`(?i)press up to edit queued message`)

// markerOf is the case's leading token, which the payload carries so a
// frame can be searched for this case specifically.
func markerOf(c probeCase) string {
	marker := strings.SplitN(c.payload, " ", 2)[0]
	return strings.SplitN(marker, "\n", 2)[0]
}

func dumpFrame(dump, caseName, suffix string, frame []byte) {
	if dump == "" || frame == nil {
		return
	}
	name := strings.ReplaceAll(caseName, "/", "_") + suffix + ".txt"
	_ = os.WriteFile(filepath.Join(dump, name), frame, 0o644)
}

// maxProbePresses bounds the Enter presses the same way the send path
// bounds them (tmuxagent.maxSubmitPresses), so a "stuck" verdict here
// means stuck for the send path too.
const maxProbePresses = 8

// submitPresses presses Enter until the pasted payload is gone from the
// composer, and reports how many presses that took plus the frame it
// finished on. The payload is gone when neither the chip nor its marker
// is in the box any more — read from the pane, never assumed.
func submitPresses(windowID string, c probeCase, settle time.Duration, dump string) (string, []byte) {
	marker := markerOf(c)
	var last []byte
	for press := 1; press <= maxProbePresses; press++ {
		if err := sendKey(windowID, "Enter"); err != nil {
			return "err:" + err.Error(), last
		}
		time.Sleep(settle)
		frame, err := tmuxagent.CapturePane(windowID)
		if err != nil {
			return "err:" + err.Error(), last
		}
		last = frame
		body := string(composerBody(frame))
		if !strings.Contains(body, "[Pasted text #") && !strings.Contains(body, marker) {
			return fmt.Sprintf("press%d", press), frame
		}
	}
	dumpFrame(dump, c.name, "_stuck", last)
	return "stuck", last
}

// composerRe extracts the live composer at the tail of the frame — the
// text between the last two horizontal rules. Anything above is
// transcript, where a pasted payload legitimately appears once
// submitted; matching the whole frame would call the transcript's echo
// a chip in the box (the 🎯T28 over-broadness).
var composerRe = regexp.MustCompile(`─{10,}\n❯([^\n]*(?:\n(?:[ \t\x{00A0}][^\n]*)?)*)\n─{10,}`)

func composerBody(frame []byte) []byte {
	m := composerRe.FindAllSubmatch(frame, -1)
	if len(m) == 0 {
		return nil
	}
	return m[len(m)-1][1]
}

// clearKeys is one clearing cycle: jump to the end and kill backwards
// for wrapped text, then Escape to drop a collapsed chip. Ctrl-C is
// deliberately absent — a second Ctrl-C exits Claude Code, which would
// end the probe session mid-sweep. Escape is dropped while a turn is
// running, because there it cancels the turn the busy path depends on.
var clearKeys = []string{"C-e", "C-u", "C-u", "C-u", "C-u", "C-u", "C-u", "Escape"}

// clearComposer empties the input box and verifies it from the pane,
// never by assumption. A case that starts on a dirty composer is
// reported, not silently measured.
func clearComposer(windowID string, busy bool) error {
	keys := clearKeys
	if busy {
		keys = clearKeys[:len(clearKeys)-1]
	}
	for range 8 {
		frame, err := tmuxagent.CapturePane(windowID)
		if err != nil {
			return err
		}
		if body := composerBody(frame); body != nil && composerIsEmpty(body) {
			return nil
		}
		for _, key := range keys {
			if err := sendKey(windowID, key); err != nil {
				return err
			}
			time.Sleep(60 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("composer still holds text after 8 clear cycles")
}

// composerIsEmpty treats Claude Code's queued-messages ghost hint as an
// empty box: it is chrome the CLI draws in a vacated composer while a
// message waits behind the running turn, and no amount of C-u removes it.
func composerIsEmpty(body []byte) bool {
	if queuedHintRe.Match(body) {
		return true
	}
	return strings.TrimSpace(string(body)) == ""
}

func sendKey(windowID, key string) error {
	out, err := exec.Command(
		"tmux", "-S", tmuxagent.SocketPath(),
		"send-keys", "-t", windowID, key,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux send-keys %s: %w: %s", key, err, out)
	}
	return nil
}

// pasteBuffer mirrors tmuxagent.pasteViaBuffer exactly — load-buffer
// from a temp file, then paste-buffer -p -r — so the probe exercises
// the same bytes the send path delivers.
func pasteBuffer(windowID, msg string) error {
	f, err := os.CreateTemp("", "t28size-*.txt")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(msg); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	sock := tmuxagent.SocketPath()
	const buf = "t28size"
	if out, err := exec.Command("tmux", "-S", sock, "load-buffer", "-b", buf, path).CombinedOutput(); err != nil {
		return fmt.Errorf("load-buffer: %w: %s", err, out)
	}
	if out, err := exec.Command("tmux", "-S", sock,
		"paste-buffer", "-p", "-r", "-d", "-b", buf, "-t", windowID).CombinedOutput(); err != nil {
		return fmt.Errorf("paste-buffer: %w: %s", err, out)
	}
	return nil
}

// windowIDFrom pulls the tmux target out of Agent.AttachCommand, which
// renders as `tmux -S <sock> attach -t <windowID>`.
func windowIDFrom(attach string) string {
	fields := strings.Fields(attach)
	for i, f := range fields {
		if f == "-t" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	die("cannot parse window id from %q", attach)
	return ""
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(3)
}
