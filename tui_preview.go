// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProgressTUIPreview is published for provisional Claude Session text under
// an open ⏺ block that does not yet have a matching assistant-text JSONL
// record (🎯T51 / 🎯T53). Text is generated Markdown from a full scrape
// parse — not raw pane glyphs. PreviewUpdate is append (suffix delta) or
// rewrite (full MD). Consumers replace the provisional buffer wholesale when
// the sealed Type=assistant Event arrives. WaitForResponse ignores these.
const ProgressTUIPreview = "tui_preview"

// ProgressTUIPreviewFault is published when a checked TUI-scrape invariant
// fails (🎯T51.4). Text names the invariant id, explains observed vs
// expected, and includes a bounded pane excerpt. WaitForResponse ignores
// these Events — JSONL remains the seal/completion path. Faults usually
// mean Claude Code TUI chrome changed under Claudia's feet.
const ProgressTUIPreviewFault = "tui_preview_fault"

// PreviewUpdate values on provisional Events (🎯T53). Clients use these
// without branching on provider: append extends the open buffer; rewrite
// replaces it. Streaming backends tag chunk deltas as append; Claude TUI
// derives the kind by prefix-comparing generated Markdown snapshots.
const (
	PreviewUpdateAppend  = "append"
	PreviewUpdateRewrite = "rewrite"
)

// Stable invariant ids used in ProgressTUIPreviewFault reports (🎯T51.4).
const (
	// TUIInvariantOpenBlockVanished: an open unsealed preview block must
	// remain findable in later frames until sealed (not silently dropped).
	TUIInvariantOpenBlockVanished = "open_block_vanished"
	// TUIInvariantRanShellChromeForm: tool chrome that looks like
	// "Ran N … command(s)" must match the expected "Ran N shell command(s)"
	// form; near-misses mean chrome drift.
	TUIInvariantRanShellChromeForm = "ran_shell_chrome_form"
)

// TUIInvariantOpenBlockPrefixStable is retained as a name for older fault
// reports / docs. Scrape-level prefix stability is no longer faulted (🎯T53):
// generated-Markdown rewrite tags cover reflow.
const TUIInvariantOpenBlockPrefixStable = "open_block_prefix_stable"

const tuiPreviewFaultExcerptMax = 1500

var (
	tuiPreviewBullet   = regexp.MustCompile(`^⏺\s*(.*)$`)
	tuiPreviewRanShell = regexp.MustCompile(`(?i)^\s*Ran\s+\d+\s+shell\s+commands?\s*$`)
	// Near-miss: "Ran N <something> command(s)" that is not the shell form.
	tuiPreviewRanNear  = regexp.MustCompile(`(?i)^\s*Ran\s+\d+\s+\S.*\bcommands?\s*$`)
	tuiPreviewToolCall = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*\(`)
)

// extractAssistantPreviewBlocks returns ⏺ assistant scrape blocks from a
// capture-pane frame, excluding tool-shaped bullets (e.g. ⏺ Bash(...)) and
// stopping a block before "Ran N shell command(s)" / composer chrome.
func extractAssistantPreviewBlocks(frame string) []string {
	blocks, _ := analysePreviewFrame(frame)
	return blocks
}

// analysePreviewFrame extracts scrape blocks and chrome-form faults.
func analysePreviewFrame(frame string) (blocks []string, faults []tuiPreviewFault) {
	lines := strings.Split(strings.ReplaceAll(frame, "\r\n", "\n"), "\n")
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		for len(cur) > 0 && strings.TrimSpace(cur[len(cur)-1]) == "" {
			cur = cur[:len(cur)-1]
		}
		if len(cur) == 0 {
			return
		}
		first := strings.TrimSpace(cur[0])
		if tuiPreviewToolCall.MatchString(first) {
			cur = nil
			return
		}
		text := strings.Join(cur, "\n")
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, text)
		}
		cur = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if tuiPreviewRanNear.MatchString(trimmed) && !tuiPreviewRanShell.MatchString(trimmed) {
			faults = append(faults, tuiPreviewFault{
				Invariant: TUIInvariantRanShellChromeForm,
				Detail: fmt.Sprintf(
					"pane line %q looks like Ran-N command chrome but does not match expected form %q",
					trimmed, `^\s*Ran\s+\d+\s+shell\s+commands?\s*$`,
				),
			})
		}
		if m := tuiPreviewBullet.FindStringSubmatch(line); m != nil {
			flush()
			cur = []string{m[1]}
			continue
		}
		if len(cur) == 0 {
			continue
		}
		if tuiPreviewRanShell.MatchString(line) {
			flush()
			continue
		}
		// Full-line composer rule only — table borders also use ─.
		if isComposerRuleLine(line) || strings.HasPrefix(trimmed, "❯") {
			flush()
			continue
		}
		if isStatusSpinnerLine(trimmed) {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return blocks, faults
}

func isComposerRuleLine(line string) bool {
	t := strings.TrimSpace(line)
	if utf8.RuneCountInString(t) < 20 {
		return false
	}
	for _, r := range t {
		if r != '─' && r != '━' && r != '-' && r != '=' {
			return false
		}
	}
	return true
}

func isStatusSpinnerLine(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(trimmed)
	switch r {
	case '✻', '✢', '✶':
		return true
	}
	return false
}

type tuiPreviewFault struct {
	Invariant string
	Detail    string
}

// tuiPreviewTracker maps pane frames to provisional preview Events and
// seals them in order when assistant-text JSONL records arrive (🎯T51).
// Each open block is fully parsed to Markdown; PreviewUpdate is derived by
// prefix-comparing generated MD (🎯T53).
type tuiPreviewTracker struct {
	turnID       string
	turnBaseline int // ⏺ count at turn open; -1 = unset until next observe
	sealed       int // how many post-baseline blocks have been sealed
	lastEmitted  []string // generated Markdown per open slot
	// reportedFaults dedupes identical invariant reports within a turn.
	reportedFaults map[string]struct{}
}

func (t *tuiPreviewTracker) resetTurn(turnID string) {
	t.turnID = turnID
	t.turnBaseline = -1
	t.sealed = 0
	t.lastEmitted = nil
	t.reportedFaults = nil
}

func (t *tuiPreviewTracker) observeFrame(frame string) []Event {
	blocks, chromeFaults := analysePreviewFrame(frame)
	var out []Event
	for _, f := range chromeFaults {
		if ev, ok := t.faultEvent(f, frame); ok {
			out = append(out, ev)
		}
	}

	if t.turnBaseline < 0 {
		t.turnBaseline = len(blocks)
		return out
	}

	start := t.turnBaseline + t.sealed
	var openBlocks []string
	if start < len(blocks) {
		openBlocks = blocks[start:]
	}

	// Viewport scroll drops older ⏺ slots from capture-pane. That is not
	// chrome drift — trim emitted state to match visible open slots.
	if len(t.lastEmitted) > len(openBlocks) {
		t.lastEmitted = t.lastEmitted[:len(openBlocks)]
	}

	for i := 0; i < len(openBlocks); i++ {
		md := scrapeBlockToMarkdown(openBlocks[i])
		if strings.TrimSpace(md) == "" {
			continue
		}
		prev := ""
		if i < len(t.lastEmitted) {
			prev = t.lastEmitted[i]
		}
		if md == prev {
			continue
		}
		kind := PreviewUpdateRewrite
		text := md
		if prev != "" && strings.HasPrefix(md, prev) {
			kind = PreviewUpdateAppend
			text = md[len(prev):]
		}
		for len(t.lastEmitted) <= i {
			t.lastEmitted = append(t.lastEmitted, "")
		}
		t.lastEmitted[i] = md
		out = append(out, Event{
			Type:          "progress",
			ProgressType:  ProgressTUIPreview,
			PreviewUpdate: kind,
			TurnID:        t.turnID,
			MessageID:     fmt.Sprintf("tui-preview-%d", start+i),
			Text:          text,
		})
	}
	return out
}

// sealAssistantText marks the next open preview block as sealed by an
// assistant-text JSONL record. Order-based within the turn (🎯T51.3).
func (t *tuiPreviewTracker) sealAssistantText() {
	t.sealed++
	if len(t.lastEmitted) > 0 {
		t.lastEmitted = t.lastEmitted[1:]
	}
}

func (t *tuiPreviewTracker) faultEvent(f tuiPreviewFault, frame string) (Event, bool) {
	key := f.Invariant + "\x00" + f.Detail
	if t.reportedFaults == nil {
		t.reportedFaults = make(map[string]struct{})
	}
	if _, seen := t.reportedFaults[key]; seen {
		return Event{}, false
	}
	t.reportedFaults[key] = struct{}{}
	return Event{
		Type:         "progress",
		ProgressType: ProgressTUIPreviewFault,
		TurnID:       t.turnID,
		MessageID:    "tui-preview-fault:" + f.Invariant,
		Text:         formatTUIPreviewFault(f, frame),
	}, true
}

func formatTUIPreviewFault(f tuiPreviewFault, frame string) string {
	var b strings.Builder
	b.WriteString("tui_preview_fault: invariant=")
	b.WriteString(f.Invariant)
	b.WriteByte('\n')
	b.WriteString("detail: ")
	b.WriteString(f.Detail)
	b.WriteByte('\n')
	b.WriteString("excerpt:\n---\n")
	b.WriteString(boundPaneExcerpt(frame, tuiPreviewFaultExcerptMax))
	b.WriteString("\n---\n")
	b.WriteString("hint: Claude Code TUI chrome likely changed; provisional preview is untrusted until scrape invariants are updated")
	return b.String()
}

func boundPaneExcerpt(frame string, maxBytes int) string {
	frame = strings.ReplaceAll(frame, "\r\n", "\n")
	if len(frame) <= maxBytes {
		return frame
	}
	if maxBytes < 1 {
		return ""
	}
	excerpt := frame[len(frame)-maxBytes:]
	if i := strings.IndexByte(excerpt, '\n'); i >= 0 && i+1 < len(excerpt) {
		excerpt = excerpt[i+1:]
	}
	return "…\n" + excerpt
}

// scrapeBlockToMarkdown turns one open ⏺ scrape block into generated
// Markdown: blank-line paragraphs with soft-wrap join; box-drawing tables
// as pipe MD truncated at the last complete data row (🎯T53).
func scrapeBlockToMarkdown(block string) string {
	lines := strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n")
	var units []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		u := strings.Join(cur, "\n")
		cur = nil
		if looksLikeBoxTableUnit(u) {
			if md := boxTableToPipeMarkdown(u); md != "" {
				units = append(units, md)
			}
			return
		}
		if p := joinSoftWrappedParagraph(u); p != "" {
			units = append(units, p)
		}
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return strings.Join(units, "\n\n")
}

func looksLikeBoxTableUnit(u string) bool {
	return strings.ContainsAny(u, "┌┬┐├┼┤└┴┘│")
}

func joinSoftWrappedParagraph(u string) string {
	var b strings.Builder
	lines := strings.Split(u, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(t)
			continue
		}
		prev := b.String()
		// Soft wrap: continuation indent or lowercase continuation.
		if strings.HasPrefix(line, "  ") || (t != "" && unicode.IsLower([]rune(t)[0]) && !strings.HasSuffix(prev, ".") && !strings.HasSuffix(prev, ":") && !strings.HasSuffix(prev, "!") && !strings.HasSuffix(prev, "?")) {
			b.WriteByte(' ')
			b.WriteString(t)
			continue
		}
		// Hard break inside a blank-line unit — keep as same paragraph with space.
		_ = i
		b.WriteByte(' ')
		b.WriteString(t)
	}
	return b.String()
}

func boxTableToPipeMarkdown(u string) string {
	var rows [][]string
	for _, line := range strings.Split(u, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// Border / separator lines — skip; incomplete bottom border is EOF OK.
		if strings.ContainsAny(t, "┌┬┐├┼┤└┴┘") && !strings.Contains(t, "│") {
			continue
		}
		if strings.HasPrefix(t, "│") && strings.HasSuffix(t, "│") {
			inner := strings.TrimPrefix(strings.TrimSuffix(t, "│"), "│")
			parts := strings.Split(inner, "│")
			cells := make([]string, 0, len(parts))
			complete := true
			for _, p := range parts {
				cells = append(cells, strings.TrimSpace(p))
			}
			// A truncated row mid-paint often lacks the closing bar — already
			// gated by HasSuffix; still require ≥2 cells.
			if !complete || len(cells) < 2 {
				// Stop at first incomplete row — prior rows are delivered.
				break
			}
			rows = append(rows, cells)
			continue
		}
		// Non-table noise inside a table unit ends the table early.
		if len(rows) > 0 {
			break
		}
	}
	if len(rows) == 0 {
		return ""
	}
	cols := len(rows[0])
	for _, r := range rows[1:] {
		if len(r) > cols {
			cols = len(r)
		}
	}
	norm := func(r []string) []string {
		out := make([]string, cols)
		copy(out, r)
		return out
	}
	var b strings.Builder
	header := norm(rows[0])
	b.WriteString("|")
	for _, c := range header {
		b.WriteString(" ")
		b.WriteString(c)
		b.WriteString(" |")
	}
	b.WriteByte('\n')
	b.WriteString("|")
	for range header {
		b.WriteString(" --- |")
	}
	for _, r := range rows[1:] {
		b.WriteByte('\n')
		b.WriteString("|")
		for _, c := range norm(r) {
			b.WriteString(" ")
			b.WriteString(c)
			b.WriteString(" |")
		}
	}
	return b.String()
}
