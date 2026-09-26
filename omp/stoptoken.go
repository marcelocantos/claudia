// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import "strings"

// StopTokens are model stop markers. A turn records the token as a
// field. It is not owner-visible text (🎯T870).
var StopTokens = []string{"<|endoftext|>", "<|im_end|>", "<|eot_id|>", "<|eos|>", "<|eot|>"}

// StripStopToken removes stop tokens from assistant text. token is the
// first one found, or empty.
func StripStopToken(s string) (visible, token string) {
	visible = s
	for _, tok := range StopTokens {
		if strings.Contains(visible, tok) {
			if token == "" {
				token = tok
			}
			visible = strings.ReplaceAll(visible, tok, "")
		}
	}
	return visible, token
}
