// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"strings"
	"testing"
)

func TestExtractAssistantPreviewBlocksSkipsToolsAndRanChrome(t *testing.T) {
	frame := strings.Join([]string{
		"⏺ The new process (pid 5769) started after the claudia fix, so I'll monitor it",
		"  for a couple minutes to confirm the overseer reattaches.",
		"",
		"  Ran 1 shell command",
		"",
		"⏺ Bash(cd /tmp && echo hi)",
		"  ⎿  hi",
		"",
		"⏺ So far it looks clean, but 34 seconds isn't enough.",
		"",
		"  Ran 2 shell commands",
	}, "\n")

	got := extractAssistantPreviewBlocks(frame)
	if len(got) != 2 {
		t.Fatalf("blocks = %d (%q), want 2", len(got), got)
	}
	if !strings.Contains(got[0], "pid 5769") {
		t.Errorf("block0 = %q", got[0])
	}
	if strings.Contains(got[0], "Ran 1") {
		t.Errorf("block0 leaked tool chrome: %q", got[0])
	}
	if !strings.Contains(got[1], "34 seconds") {
		t.Errorf("block1 = %q", got[1])
	}
	if strings.Contains(got[1], "Bash(") {
		t.Errorf("tool bullet leaked into prose blocks: %q", got)
	}
}

func TestScrapeBlockToMarkdownParagraphsAndTable(t *testing.T) {
	block := strings.Join([]string{
		"Apples are crisp and sweet fruits that grow on trees.",
		"",
		"  Oranges are citrus fruits known for their bright color and vitamin C content.",
		"",
		"  ┌──────┬──────┬──────┐",
		"  │ ColA │ ColB │ ColC │",
		"  ├──────┼──────┼──────┤",
		"  │ a1   │ b1   │ c1   │",
		"  ├──────┼──────┼──────┤",
		"  │ a2   │ b2   │ c2   │",
		"  └──────┴──────┴──────┘",
		"",
		"  Bananas are tropical fruits with soft, creamy flesh.",
	}, "\n")

	md := scrapeBlockToMarkdown(block)
	if !strings.Contains(md, "Apples are crisp") {
		t.Fatalf("missing apples: %q", md)
	}
	if !strings.Contains(md, "Oranges are citrus") {
		t.Fatalf("missing oranges: %q", md)
	}
	if !strings.Contains(md, "Bananas are tropical") {
		t.Fatalf("missing bananas: %q", md)
	}
	if !strings.Contains(md, "| ColA | ColB | ColC |") {
		t.Fatalf("missing header: %q", md)
	}
	if !strings.Contains(md, "| a1 | b1 | c1 |") || !strings.Contains(md, "| a2 | b2 | c2 |") {
		t.Fatalf("missing cells: %q", md)
	}
	// Soft-wrap long sentence stays one paragraph.
	wrap := "The quick brown fox jumps over the lazy dog again and again while the\n  moonlight glimmers on the lake.\n\n  Short closer."
	got := scrapeBlockToMarkdown(wrap)
	parts := strings.Split(got, "\n\n")
	if len(parts) != 2 {
		t.Fatalf("soft-wrap split wrong: %#v", parts)
	}
	if strings.Contains(parts[0], "\n") {
		t.Fatalf("soft wrap not joined: %q", parts[0])
	}
}

func TestScrapeBlockToMarkdownTruncatesIncompleteTableRow(t *testing.T) {
	// EOF mid-table: header + one full row, then incomplete border / missing row.
	block := strings.Join([]string{
		"Intro sentence.",
		"",
		"  ┌──────┬──────┬──────┐",
		"  │ ColA │ ColB │ ColC │",
		"  ├──────┼──────┼──────┤",
		"  │ a1   │ b1   │ c1   │",
		"  ├──────┼──────┼──────┤",
		// row 2 not yet painted
	}, "\n")
	md := scrapeBlockToMarkdown(block)
	if !strings.Contains(md, "| a1 | b1 | c1 |") {
		t.Fatalf("want complete row kept: %q", md)
	}
	if strings.Contains(md, "a2") {
		t.Fatalf("incomplete/missing row leaked: %q", md)
	}
}

func TestTUIPreviewTrackerAppendRewriteOnGeneratedMarkdown(t *testing.T) {
	var tr tuiPreviewTracker
	tr.resetTurn("turn-1")
	_ = tr.observeFrame("") // baseline

	evs := tr.observeFrame("⏺ Hello world.\n")
	if len(evs) != 1 || evs[0].PreviewUpdate != PreviewUpdateRewrite {
		t.Fatalf("first open = %+v", evs)
	}
	if evs[0].Text != "Hello world." {
		t.Fatalf("Text = %q", evs[0].Text)
	}

	// Prefix growth on generated MD → append delta.
	evs = tr.observeFrame("⏺ Hello world. More text.\n")
	if len(evs) != 1 || evs[0].PreviewUpdate != PreviewUpdateAppend {
		t.Fatalf("growth = %+v", evs)
	}
	if evs[0].Text != " More text." {
		t.Fatalf("append delta = %q", evs[0].Text)
	}

	// Non-prefix reflow → rewrite full MD (no scrape-level fault).
	evs = tr.observeFrame("⏺ Completely different reply.\n")
	if len(evs) != 1 || evs[0].PreviewUpdate != PreviewUpdateRewrite {
		t.Fatalf("rewrite = %+v", evs)
	}
	if evs[0].Text != "Completely different reply." {
		t.Fatalf("rewrite Text = %q", evs[0].Text)
	}
	for _, ev := range evs {
		if ev.ProgressType == ProgressTUIPreviewFault {
			t.Fatalf("scrape rewrite must not fault: %+v", ev)
		}
	}
}

func TestTUIPreviewTrackerOpenPreviewThenSeal(t *testing.T) {
	var tr tuiPreviewTracker
	tr.resetTurn("turn-1")

	hist := "⏺ Older turn reply that already has JSONL.\n"
	if evs := tr.observeFrame(hist); len(evs) != 0 {
		t.Fatalf("baseline observe leaked events: %+v", evs)
	}

	growing := hist + "\n⏺ The new process (pid 5769) started\n"
	evs := tr.observeFrame(growing)
	if len(evs) != 1 || evs[0].ProgressType != ProgressTUIPreview {
		t.Fatalf("open preview = %+v", evs)
	}
	if evs[0].PreviewUpdate != PreviewUpdateRewrite {
		t.Fatalf("PreviewUpdate = %q", evs[0].PreviewUpdate)
	}
	if evs[0].MessageID != "tui-preview-1" {
		t.Fatalf("MessageID = %q", evs[0].MessageID)
	}
	if !strings.Contains(evs[0].Text, "pid 5769") {
		t.Fatalf("Text = %q", evs[0].Text)
	}

	if evs := tr.observeFrame(growing); len(evs) != 0 {
		t.Fatalf("duplicate emit: %+v", evs)
	}

	grown := hist + "\n⏺ The new process (pid 5769) started after the fix.\n"
	evs = tr.observeFrame(grown)
	if len(evs) != 1 || evs[0].PreviewUpdate != PreviewUpdateAppend {
		t.Fatalf("growth = %+v", evs)
	}
	if evs[0].Text != " after the fix." {
		t.Fatalf("delta = %q", evs[0].Text)
	}

	tr.sealAssistantText()

	afterSeal := grown + "  (stale redraw)\n"
	if evs := tr.observeFrame(afterSeal); len(evs) != 0 {
		t.Fatalf("preview after seal: %+v", evs)
	}

	second := afterSeal + "\n  Ran 1 shell command\n\n⏺ Second prose block.\n"
	evs = tr.observeFrame(second)
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "Second prose") {
		t.Fatalf("second open = %+v", evs)
	}
	if evs[0].MessageID != "tui-preview-2" {
		t.Fatalf("second MessageID = %q", evs[0].MessageID)
	}
}

func TestTUIPreviewScrollDropsOpenSlotWithoutFault(t *testing.T) {
	var tr tuiPreviewTracker
	tr.resetTurn("turn-1")
	_ = tr.observeFrame("")

	open := "⏺ Provisional streaming text still open.\n"
	if evs := tr.observeFrame(open); len(evs) != 1 {
		t.Fatalf("open = %+v", evs)
	}

	// Viewport scrolled the open ⏺ away — not a chrome-drift fault.
	evs := tr.observeFrame("❯ \n")
	for _, ev := range evs {
		if ev.ProgressType == ProgressTUIPreviewFault {
			t.Fatalf("scroll must not fault: %+v", ev)
		}
	}
}

func TestTUIPreviewInvariantRanShellChromeForm(t *testing.T) {
	var tr tuiPreviewTracker
	tr.resetTurn("turn-1")
	_ = tr.observeFrame("")

	frame := strings.Join([]string{
		"⏺ Some prose.",
		"",
		"  Ran 2 tool commands",
		"",
		"⏺ More prose.",
	}, "\n")
	evs := tr.observeFrame(frame)
	var sawFault, sawPreview bool
	for _, ev := range evs {
		switch ev.ProgressType {
		case ProgressTUIPreviewFault:
			sawFault = true
			if !strings.Contains(ev.Text, TUIInvariantRanShellChromeForm) {
				t.Fatalf("fault missing chrome invariant: %q", ev.Text)
			}
		case ProgressTUIPreview:
			sawPreview = true
		}
	}
	if !sawFault {
		t.Fatalf("expected ran_shell_chrome_form fault, events=%+v", evs)
	}
	if !sawPreview {
		t.Fatalf("expected prose preview alongside chrome fault, events=%+v", evs)
	}
}

func TestWaitForResponseIgnoresTUIPreviewAndFault(t *testing.T) {
	agent := &Agent{
		alive:     true,
		eventSubs: map[int64]EventFunc{},
		ready:     make(chan struct{}),
	}
	close(agent.ready)

	done := make(chan string, 1)
	go func() {
		text, err := agent.WaitForResponse(t.Context())
		if err != nil {
			t.Errorf("WaitForResponse: %v", err)
			done <- ""
			return
		}
		done <- text
	}()

	for agent.EventSubscriberCount() == 0 {
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		default:
		}
	}

	agent.PublishEvent(Event{
		Type:          "progress",
		ProgressType:  ProgressTUIPreview,
		PreviewUpdate: PreviewUpdateRewrite,
		Text:          "pane preview must not surface here",
	})
	agent.PublishEvent(Event{
		Type:         "progress",
		ProgressType: ProgressTUIPreviewFault,
		Text:         "tui_preview_fault: invariant=open_block_vanished\ndetail: test",
	})
	agent.PublishEvent(Event{
		Type:       "assistant",
		Text:       "sealed markdown",
		StopReason: "end_turn",
	})

	got := <-done
	if got != "sealed markdown" {
		t.Fatalf("WaitForResponse = %q, want sealed markdown", got)
	}
}

func TestTUIPreviewHermeticOpenPreviewThenJSONLSeal(t *testing.T) {
	agent := &Agent{
		alive:      true,
		sessionID:  "sess-t51",
		eventSubs:  map[int64]EventFunc{},
		ready:      make(chan struct{}),
		tuiPreview: &tuiPreviewTracker{},
	}
	close(agent.ready)

	var got []Event
	agent.SubscribeEvents(func(ev Event) {
		got = append(got, ev)
	})

	userLine := `{"type":"user","uuid":"turn-abc","sessionId":"sess-t51","message":{"role":"user","content":"hi"}}`
	agent.applyClaudeTUIPreviewTranscript(parseEvent(userLine))

	hist := "⏺ Prior sealed reply.\n"
	agent.ObservePaneFrame(hist)
	agent.ObservePaneFrame(hist + "\n⏺ Streaming provisional answer…\n")

	if len(got) != 1 || got[0].ProgressType != ProgressTUIPreview {
		t.Fatalf("preview events = %+v", got)
	}
	if got[0].PreviewUpdate != PreviewUpdateRewrite {
		t.Fatalf("PreviewUpdate = %q", got[0].PreviewUpdate)
	}
	if got[0].SessionID != "sess-t51" || got[0].TurnID != "turn-abc" {
		t.Fatalf("identity = %+v", got[0])
	}

	asstLine := `{"type":"assistant","uuid":"rec-1","sessionId":"sess-t51","message":{"id":"msg-1","content":[{"type":"text","text":"Streaming provisional answer…"}],"stop_reason":"end_turn"}}`
	ev := parseEvent(asstLine)
	ev.TurnID = "turn-abc"
	agent.applyClaudeTUIPreviewTranscript(ev)
	agent.PublishEvent(ev)

	n := len(got)
	agent.ObservePaneFrame(hist + "\n⏺ Streaming provisional answer…\n  (redraw)\n")
	for _, ev := range got[n:] {
		if ev.ProgressType == ProgressTUIPreview {
			t.Fatalf("preview after seal: %+v", ev)
		}
	}
	if got[len(got)-1].Type != "assistant" {
		t.Fatalf("last event should be sealed assistant, got %+v", got[len(got)-1])
	}
}

func TestExtractStopsAtComposerRuleNotTableBorder(t *testing.T) {
	frame := strings.Join([]string{
		"⏺ Intro",
		"",
		"  ┌──────┬──────┐",
		"  │ A    │ B    │",
		"  └──────┴──────┘",
		"",
		"────────────────────────────────────────────────────────────────────────────────",
		"❯ ",
	}, "\n")
	blocks := extractAssistantPreviewBlocks(frame)
	if len(blocks) != 1 {
		t.Fatalf("blocks=%d %q", len(blocks), blocks)
	}
	if !strings.Contains(blocks[0], "│ A") {
		t.Fatalf("table truncated by composer rule false positive: %q", blocks[0])
	}
	if strings.Contains(blocks[0], "❯") {
		t.Fatalf("composer leaked: %q", blocks[0])
	}
}
