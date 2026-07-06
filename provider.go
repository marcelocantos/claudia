// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

type providerCapabilities struct {
	Task          bool
	Session       bool
	Resume        bool
	Rewind        bool
	Cost          bool
	Permissions   bool
	TmuxAttach    bool
	TerminalBytes bool
}

func claudeProviderCapabilities() providerCapabilities {
	return providerCapabilities{
		Task:          true,
		Session:       true,
		Resume:        true,
		Rewind:        true,
		Cost:          true,
		Permissions:   true,
		TmuxAttach:    true,
		TerminalBytes: true,
	}
}
