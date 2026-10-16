// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Receipts and accounting project non-context facts from the session owner's
// journal. They do not own a transcript, a writer or a second recovery queue.
export interface FactJournal {
  readonly store?: { readonly seat: string; readonly session: string };
  readonly entries: readonly { type: string; customType?: string; data?: unknown }[];
  appendFact(customType: string, data: unknown): void;
}
