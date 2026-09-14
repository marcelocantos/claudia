// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/marcelocantos/claudia"
)

func modelsCmd(args []string) error {
	if len(args) == 0 || args[0] != "intel" {
		usage()
		return errors.New("models: expected intel")
	}
	args = args[1:]
	if len(args) == 0 {
		usage()
		return errors.New("models intel: subcommand required")
	}
	switch args[0] {
	case "refresh":
		return modelsIntelRefresh(args[1:])
	case "latest":
		return modelsIntelLatest(args[1:])
	case "history":
		return modelsIntelHistory(args[1:])
	case "drift":
		return modelsIntelDrift(args[1:])
	default:
		usage()
		return fmt.Errorf("models intel: unknown subcommand %q", args[0])
	}
}

func intelArgsFromFlags(dir string) *claudia.ModelIntelArgs {
	return &claudia.ModelIntelArgs{Dir: dir}
}

func modelsIntelRefresh(args []string) error {
	fs := flag.NewFlagSet("models intel refresh", flag.ContinueOnError)
	dir := fs.String("dir", "", "intel store (default: CLAUDIA_MODEL_INTEL or state dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	run, err := claudia.RefreshModelIntel(context.Background(), intelArgsFromFlags(*dir))
	if run.Count > 0 {
		fmt.Printf("aa: wrote %d observations\n", run.Count)
	}
	return err
}

func modelsIntelLatest(args []string) error {
	fs := flag.NewFlagSet("models intel latest", flag.ContinueOnError)
	dir := fs.String("dir", "", "intel store")
	purpose := fs.String("purpose", "", "filter purpose")
	if err := fs.Parse(args); err != nil {
		return err
	}
	obs, err := claudia.LatestModelIntel(intelArgsFromFlags(*dir))
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PURPOSE\tGENERATION\tEFFORT\tVALUE\tCOST\tREV\tAT")
	for _, o := range obs {
		if *purpose != "" && string(o.Purpose) != *purpose {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%.4f\t%s\t%s\n",
			o.Purpose, o.Generation, effortOrDash(o.Effort), o.Value, o.CostUSD, o.SourceRev,
			o.ObservedAt.UTC().Format(time.RFC3339))
	}
	return w.Flush()
}

func modelsIntelHistory(args []string) error {
	fs := flag.NewFlagSet("models intel history", flag.ContinueOnError)
	dir := fs.String("dir", "", "intel store")
	gen := fs.String("generation", "", "catalog generation (required)")
	effort := fs.String("effort", "", "effort (empty = unspecified)")
	purpose := fs.String("purpose", "", "purpose filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *gen == "" {
		return errors.New("models intel history: --generation is required")
	}
	obs, err := claudia.ModelIntelHistory(intelArgsFromFlags(*dir), *gen, claudia.ModelEffort(*effort), claudia.ModelPurpose(*purpose))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(obs)
}

func modelsIntelDrift(args []string) error {
	fs := flag.NewFlagSet("models intel drift", flag.ContinueOnError)
	dir := fs.String("dir", "", "intel store")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, err := claudia.DriftModelIntel(intelArgsFromFlags(*dir))
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "GENERATION\tEFFORT\tPURPOSE\tFROM\tTO\tDELTA\tMOVE")
	for _, d := range rows {
		move := ""
		if d.BoardEvent {
			move = "board"
		} else if d.Significant {
			move = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%.2f\t%+.2f\t%s\n",
			d.Generation, effortOrDash(d.Effort), d.Purpose, d.From, d.To, d.Delta, move)
	}
	return w.Flush()
}

func effortOrDash(e claudia.ModelEffort) string {
	if e == "" {
		return "-"
	}
	return string(e)
}
