// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMapGrokOnDemandSpendT967_2(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

	t.Run("onDemandCap 0 maps no spend", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "grok", "billing_rollover.json"))
		if err != nil {
			t.Fatal(err)
		}
		pu := parseGrokBilling(raw, now)
		if pu.Spend != nil {
			t.Fatalf("expected nil Spend, got %+v", pu.Spend)
		}
	})

	t.Run("cap enabled but used zero stays silent", func(t *testing.T) {
		sp := mapGrokOnDemandSpend(&grokMoneyVal{Val: 10000}, &grokMoneyVal{Val: 0})
		if sp != nil {
			t.Fatalf("expected nil until onDemandUsed is non-zero, got %+v", sp)
		}
	})

	t.Run("spending maps AUD money", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "grok", "billing_ondemand_spending.json"))
		if err != nil {
			t.Fatal(err)
		}
		pu := parseGrokBilling(raw, now)
		if pu.Spend == nil || !pu.Spend.Enabled {
			t.Fatalf("expected enabled Spend, got %+v", pu.Spend)
		}
		if pu.Spend.Used == nil || pu.Spend.Used.AmountMinor != 3250 || pu.Spend.Used.Currency != "AUD" {
			t.Errorf("Used=%+v", pu.Spend.Used)
		}
		if pu.Spend.Limit == nil || pu.Spend.Limit.AmountMinor != 10000 {
			t.Errorf("Limit=%+v", pu.Spend.Limit)
		}
	})
}
