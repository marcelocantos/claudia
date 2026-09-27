// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// brokerRunCmd is a machine-facing one-turn client. The daemon chooses a
// capacity-eligible model, then owns the provider process and its event stream.
// Input is one JSON object on stdin; output is a selection record followed
// by TaskEvent wire JSONL. Failures are also emitted as error records.
func brokerRunCmd(args []string, input io.Reader, output io.Writer) (runErr error) {
	var errorEventSent bool
	defer func() {
		if runErr == nil || errorEventSent {
			return
		}
		line, err := json.Marshal(struct {
			Type     string `json:"type"`
			ErrorMsg string `json:"error_msg"`
		}{Type: "error", ErrorMsg: runErr.Error()})
		if err == nil {
			_, _ = output.Write(append(line, '\n'))
		}
	}()
	if len(args) != 0 {
		return errors.New("broker run takes one JSON request on stdin and no arguments")
	}
	var request struct {
		Predicates json.RawMessage            `json:"predicates"`
		Tasks      map[string]json.RawMessage `json:"tasks"`
		Prompt     string                     `json:"prompt"`
	}
	const maxRequestBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(input, maxRequestBytes+1))
	if err != nil {
		return fmt.Errorf("broker run input: %w", err)
	}
	if len(body) > maxRequestBytes {
		return errors.New("broker run input exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("broker run input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("broker run input must contain exactly one JSON object")
	}
	if strings.TrimSpace(request.Prompt) == "" || len(request.Tasks) == 0 {
		return errors.New("broker run requires a prompt and at least one provider task")
	}
	pred, err := claudia.DecodePredicatesWire(request.Predicates)
	if err != nil {
		return err
	}
	pred.Mode = claudia.CapabilityTask
	pred.Background = true
	pred.RequireUsage = true
	pred.PreferPlan = true
	for _, row := range claudia.ModelCatalog() {
		if _, ok := request.Tasks[string(row.Provider)]; !ok {
			pred.ExcludeProviders = append(pred.ExcludeProviders, row.Provider)
		}
	}
	predRaw, err := claudia.EncodePredicatesWire(pred)
	if err != nil {
		return err
	}
	resp, err := roundTrip(&broker.Request{Type: broker.TypeResolve, Resolve: &broker.ResolveRequest{Predicates: predRaw}})
	if err != nil {
		return fmt.Errorf("broker run resolve: %w", err)
	}
	if resp.Resolved == nil {
		return fmt.Errorf("broker run resolve answered with %s", resp.Type)
	}
	pick, err := claudia.DecodePickWire(resp.Resolved.Pick)
	if err != nil {
		return err
	}
	taskRaw, ok := request.Tasks[string(pick.Provider)]
	if !ok {
		return fmt.Errorf("broker selected unconfigured provider %s", pick.Provider)
	}
	cfg, err := claudia.DecodeTaskConfigWire(taskRaw)
	if err != nil {
		return err
	}
	if cfg.Provider != "" && cfg.Provider != pick.Provider {
		return fmt.Errorf("task provider %s disagrees with broker selection %s", cfg.Provider, pick.Provider)
	}
	cfg.Provider = pick.Provider
	cfg.Model = pick.Model
	cfg.RequireBroker = true
	selection, err := json.Marshal(struct {
		Type     string           `json:"type"`
		Provider claudia.Provider `json:"provider"`
		Model    string           `json:"model"`
		Reason   string           `json:"reason"`
	}{Type: "selection", Provider: pick.Provider, Model: pick.Model, Reason: pick.Reason})
	if err != nil {
		return err
	}
	if _, err := output.Write(append(selection, '\n')); err != nil {
		return err
	}
	events, err := claudia.NewTask(cfg).Run(context.Background(), request.Prompt)
	if err != nil {
		return fmt.Errorf("broker run task: %w", err)
	}
	var result bool
	var taskErr error
	for ev := range events {
		line, err := claudia.EncodeTaskEventWire(ev)
		if err != nil {
			return err
		}
		if _, err := output.Write(append(line, '\n')); err != nil {
			return err
		}
		if ev.Type == claudia.TaskEventResult {
			result = true
		}
		if ev.Type == claudia.TaskEventError {
			taskErr = errors.New(ev.ErrorMsg)
			errorEventSent = true
		}
	}
	if taskErr != nil {
		return taskErr
	}
	if !result {
		return errors.New("broker run ended without a result")
	}
	return nil
}
