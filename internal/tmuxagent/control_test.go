// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"bytes"
	"testing"
)

func TestDecodeOutputEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []byte
	}{
		{"empty", "", []byte{}},
		{"plain ascii", "hello world", []byte("hello world")},
		{"cr as octal", `\015`, []byte{0x0d}},
		{"lf as octal", `\012`, []byte{0x0a}},
		{"esc as octal", `\033`, []byte{0x1b}},
		{"backslash double", `\\`, []byte{'\\'}},
		{"backslash octal", `\134`, []byte{'\\'}},
		{
			"mixed text and escapes",
			`hello\040world\015\012`,
			[]byte("hello world\r\n"),
		},
		{
			"high byte (utf-8 continuation)",
			`\302\256`,
			[]byte{0xc2, 0xae}, // ® encoded as UTF-8
		},
		{
			"box drawing char (U+2500 ─ in utf-8)",
			`\342\224\200`,
			[]byte{0xe2, 0x94, 0x80},
		},
		{
			"malformed trailing backslash",
			`abc\`,
			[]byte(`abc\`),
		},
		{
			"malformed non-octal after backslash",
			`a\x1b`,
			[]byte(`a\x1b`),
		},
		{
			"ansi escape sequence (CSI 1A)",
			`\033[1A`,
			[]byte{0x1b, '[', '1', 'A'},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeOutputEscape(tc.in)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("decodeOutputEscape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHandleLinePaneFilter verifies that handleLine only delivers
// %output for the pane the Control instance is filtering on.
func TestHandleLinePaneFilter(t *testing.T) {
	c := &Control{
		paneID: "%3",
		bytes:  make(chan []byte, 4),
	}

	// Matching pane: delivered.
	c.handleLine(`%output %3 hello\012`)
	// Non-matching pane: dropped.
	c.handleLine(`%output %7 ignored\012`)
	// Other notification: dropped.
	c.handleLine(`%sessions-changed`)
	// Matching pane, second message.
	c.handleLine(`%output %3 world`)

	close(c.bytes)

	var got [][]byte
	for b := range c.bytes {
		got = append(got, b)
	}

	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %q", len(got), got)
	}
	if !bytes.Equal(got[0], []byte("hello\n")) {
		t.Errorf("msg[0] = %q, want %q", got[0], "hello\n")
	}
	if !bytes.Equal(got[1], []byte("world")) {
		t.Errorf("msg[1] = %q, want %q", got[1], "world")
	}
}
