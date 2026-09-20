// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command usage prints subscription plan headroom — percent remaining and next
// rollover — for every backend claudia supports, so token-plan spend is visible
// at a glance instead of flying blind. It is a thin CLI over
// claudia.QueryAllPlanUsage (🎯T18); backends that publish no remaining/rollover
// are shown as "unavailable" with the reason, never invented numbers.
//
// Run: go run ./cmd/usage
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	claudia "github.com/marcelocantos/claudia"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 🎯T85: a person running this in a loop while debugging is one of the
	// ways the vendor sees a burst, so the CLI is paced by the same
	// host-shared floor as the daemon. Inside the floor it reprints the
	// last reading with the reason appended, rather than asking again.
	all, err := claudia.QueryAllPlanUsage(ctx, claudia.DefaultPlanFetchArgs())
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage:", err)
		os.Exit(1)
	}

	now := time.Now()
	for _, pu := range all {
		if pu.Status != claudia.PlanUsageAvailable {
			fmt.Printf("%-9s unavailable — %s\n", pu.Provider, pu.Reason)
			continue
		}
		label := string(pu.Provider)
		if pu.PlanType != "" {
			label = fmt.Sprintf("%s (%s)", pu.Provider, pu.PlanType)
		}
		fmt.Println(label)
		// An available reading that still carries a reason was withheld by
		// the request floor and carried forward (🎯T85). Its age is real,
		// so say so — a number that silently stops moving is worse than a
		// number labelled stale.
		if pu.Reason != "" {
			fmt.Printf("  (as of %s ago) %s\n",
				now.Sub(pu.FetchedAt).Round(time.Second), pu.Reason)
		}
		for _, w := range pu.Windows {
			rem := "?"
			switch {
			case w.RemainingPercent != nil:
				rem = fmt.Sprintf("%.0f%%", *w.RemainingPercent)
			case w.UsedPercent != nil:
				rem = fmt.Sprintf("%.0f%%", 100-*w.UsedPercent)
			}
			reset := ""
			if w.ResetsAt != nil {
				reset = fmt.Sprintf("  resets in %s (%s)",
					w.ResetsAt.Sub(now).Round(time.Minute), w.ResetsAt.Local().Format("Mon 15:04"))
			}
			fmt.Printf("  %-8s %s remaining%s\n", w.Name, rem, reset)
		}
	}
}
