// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// Env vars for ProviderBedrock (no secrets; credential material stays in the
// AWS SDK default chain / shared config — never commit keys).
const (
	bedrockRegionEnv = "CLAUDIA_BEDROCK_REGION"
	bedrockModelEnv  = "CLAUDIA_BEDROCK_MODEL_ID"
	bedrockLiveEnv   = "CLAUDIA_BEDROCK_LIVE"
	awsRegionEnv     = "AWS_REGION"
	awsDefaultRegion = "AWS_DEFAULT_REGION"
	awsProfileEnv    = "AWS_PROFILE"
)

// bedrockSettings is the resolved Bedrock Task configuration (env + TaskConfig).
type bedrockSettings struct {
	Region  string
	ModelID string
	Profile string
}

// bedrockStreamArgs is the per-Run ConverseStream request.
type bedrockStreamArgs struct {
	Region  string
	ModelID string
	Profile string
	Prompt  string
}

// bedrockStreamer issues one ConverseStream (or fake) and yields TaskEvents.
// Production uses awsBedrockStreamer; hermetic tests inject fakes.
type bedrockStreamer interface {
	Stream(ctx context.Context, args bedrockStreamArgs) (<-chan TaskEvent, error)
}

// bedrockTaskBackend implements taskBackend for ProviderBedrock.
// When streamer is nil, RunTask builds the AWS ConverseStream client.
type bedrockTaskBackend struct {
	streamer bedrockStreamer
}

func bedrockProviderCapabilities() providerCapabilities {
	return providerCapabilities{
		Task: true,
		// Session, Resume, Rewind, Cost, Permissions, TmuxAttach,
		// TerminalBytes deliberately false for v1 — see docs/bedrock-provider.md.
	}
}

func (bedrockTaskBackend) Capabilities() providerCapabilities {
	return bedrockProviderCapabilities()
}

// bedrockTaskPrecheck refuses a request carrying a field ConverseStream
// cannot honour.
//
// 🎯T24: every one of these used to be dropped on the floor. DisallowTools
// is the worst of them — the published matrix says Bedrock cannot restrict
// tools, and the path ran anyway, so a caller who stripped Bash from a
// summariser got the same fail-open 🎯T4.6 closed for Codex and 🎯T23 for
// Grok, one provider further along.
func bedrockTaskPrecheck(req taskRunRequest) error {
	if len(req.DisallowTools) > 0 {
		return capabilityRefusal(ProviderBedrock, CapabilityToolRestrictions,
			"the Bedrock tool_restrictions claim was flipped to supported, but buildBedrockConverseInput still sends no toolConfig")
	}
	if req.SandboxMode != "" || req.ApprovalPolicy != "" {
		return capabilityRefusal(ProviderBedrock, CapabilitySandboxPolicy,
			"the Bedrock sandbox_policy claim was flipped to supported, but ConverseStream is still called with no sandbox or approval setting")
	}
	if req.SessionID != "" {
		// ConverseStream is stateless: the id would be dropped and the
		// caller would believe a conversation was being continued.
		return capabilityRefusal(ProviderBedrock, CapabilityResume,
			"the Bedrock resume claim was flipped to supported, but ConverseStream is still sent a single user message with no prior turns")
	}
	return nil
}

func (b bedrockTaskBackend) RunTask(ctx context.Context, req taskRunRequest) (*taskRun, error) {
	if err := bedrockTaskPrecheck(req); err != nil {
		return nil, err
	}
	settings, err := resolveBedrockSettings(os.Getenv, req.Model)
	if err != nil {
		return nil, err
	}
	streamer := b.streamer
	if streamer == nil {
		streamer, err = newAWSBedrockStreamer(ctx, settings)
		if err != nil {
			return nil, err
		}
	}

	streamCtx, cancel := context.WithCancel(ctx)
	events, err := streamer.Stream(streamCtx, bedrockStreamArgs{
		Region:  settings.Region,
		ModelID: settings.ModelID,
		Profile: settings.Profile,
		Prompt:  req.Prompt,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	// Bridge: honour cancel by closing consumption; streamer should observe ctx.
	// Emit TaskEventInit with the resolved model id first so Task.Model() and
	// consumers can assert requested-vs-resolved (Bedrock has no alias layer —
	// the ModelID passed to ConverseStream is the resolved id).
	out := make(chan TaskEvent, 16)
	go func() {
		defer close(out)
		defer cancel()
		initEv := TaskEvent{Type: TaskEventInit, Model: settings.ModelID}
		if req.RawLog != nil {
			req.RawLog([]byte(fmt.Sprintf(`{"provider":"bedrock","type":%q,"model":%q}`, initEv.Type, initEv.Model)))
		}
		select {
		case out <- initEv:
		case <-streamCtx.Done():
			return
		}
		for {
			select {
			case <-streamCtx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if req.RawLog != nil {
					// API path has no NDJSON wire; log a compact diagnostic line.
					req.RawLog([]byte(fmt.Sprintf(`{"provider":"bedrock","type":%q}`, ev.Type)))
				}
				select {
				case out <- ev:
				case <-streamCtx.Done():
					return
				}
			}
		}
	}()

	return &taskRun{
		events: out,
		interrupt: func() error {
			cancel()
			return nil
		},
	}, nil
}

// resolveBedrockSettings maps env + TaskConfig.Model to Bedrock settings.
// getenv is injectable for hermetic tests.
func resolveBedrockSettings(getenv func(string) string, modelFromConfig string) (bedrockSettings, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	model := strings.TrimSpace(modelFromConfig)
	if model == "" {
		model = strings.TrimSpace(getenv(bedrockModelEnv))
	}
	if model == "" {
		return bedrockSettings{}, fmt.Errorf("bedrock: model id required (TaskConfig.Model or %s)", bedrockModelEnv)
	}

	region := strings.TrimSpace(getenv(bedrockRegionEnv))
	if region == "" {
		region = strings.TrimSpace(getenv(awsRegionEnv))
	}
	if region == "" {
		region = strings.TrimSpace(getenv(awsDefaultRegion))
	}
	if region == "" {
		return bedrockSettings{}, fmt.Errorf("bedrock: region required (%s, %s, or %s)", bedrockRegionEnv, awsRegionEnv, awsDefaultRegion)
	}

	return bedrockSettings{
		Region:  region,
		ModelID: model,
		Profile: strings.TrimSpace(getenv(awsProfileEnv)),
	}, nil
}

// buildBedrockConverseInput shapes the ConverseStream request (hermetic-testable).
func buildBedrockConverseInput(modelID, prompt string) *bedrockruntime.ConverseStreamInput {
	return &bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(modelID),
		Messages: []types.Message{
			{
				Role: types.ConversationRoleUser,
				Content: []types.ContentBlock{
					&types.ContentBlockMemberText{Value: prompt},
				},
			},
		},
	}
}

// mapBedrockTextDelta maps a text delta string to TaskEventText, or nil if empty.
func mapBedrockTextDelta(delta string) *TaskEvent {
	if delta == "" {
		return nil
	}
	return &TaskEvent{Type: TaskEventText, Content: delta}
}

// mapBedrockUsage converts Bedrock TokenUsage into claudia Usage.
func mapBedrockUsage(u *types.TokenUsage) Usage {
	if u == nil {
		return Usage{}
	}
	var out Usage
	if u.InputTokens != nil {
		out.InputTokens = int(*u.InputTokens)
	}
	if u.OutputTokens != nil {
		out.OutputTokens = int(*u.OutputTokens)
	}
	if u.CacheReadInputTokens != nil {
		out.CacheReadInputTokens = int(*u.CacheReadInputTokens)
	}
	if u.CacheWriteInputTokens != nil {
		out.CacheCreationInputTokens = int(*u.CacheWriteInputTokens)
	}
	return out
}

// mapBedrockAPIError turns a pre-stream or mid-stream error into a user-facing message.
func mapBedrockAPIError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Keep mapping stable for tests without importing smithy API error types tightly.
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "no credential") ||
		strings.Contains(lower, "credentials") && strings.Contains(lower, "not found") ||
		strings.Contains(lower, "failed to refresh cached credentials") ||
		strings.Contains(lower, "ec2role") ||
		strings.Contains(lower, "sso"):
		return "bedrock: credentials unavailable via AWS default chain: " + msg
	case strings.Contains(lower, "accessdenied") || strings.Contains(lower, "access denied") ||
		strings.Contains(lower, "not authorized") || strings.Contains(lower, "unauthorized"):
		return "bedrock: access denied (check IAM and model access): " + msg
	case strings.Contains(lower, "validation"):
		return "bedrock: validation error: " + msg
	default:
		return "bedrock: " + msg
	}
}

// --- AWS production streamer ---

type awsBedrockStreamer struct {
	client *bedrockruntime.Client
}

func newAWSBedrockStreamer(ctx context.Context, settings bedrockSettings) (bedrockStreamer, error) {
	var opts []func(*config.LoadOptions) error
	opts = append(opts, config.WithRegion(settings.Region))
	if settings.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(settings.Profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("%s", mapBedrockAPIError(err))
	}
	return &awsBedrockStreamer{client: bedrockruntime.NewFromConfig(cfg)}, nil
}

func (s *awsBedrockStreamer) Stream(ctx context.Context, args bedrockStreamArgs) (<-chan TaskEvent, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("bedrock: nil client")
	}
	input := buildBedrockConverseInput(args.ModelID, args.Prompt)
	start := time.Now()
	out, err := s.client.ConverseStream(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("%s", mapBedrockAPIError(err))
	}
	stream := out.GetStream()
	ch := make(chan TaskEvent, 16)
	go func() {
		defer close(ch)
		defer func() {
			if err := stream.Close(); err != nil {
				slog.Debug("bedrock stream close", "error", err)
			}
		}()

		var text strings.Builder
		var usage Usage

		for event := range stream.Events() {
			if ctx.Err() != nil {
				return
			}
			switch v := event.(type) {
			case *types.ConverseStreamOutputMemberContentBlockDelta:
				delta := extractTextDelta(v.Value.Delta)
				if ev := mapBedrockTextDelta(delta); ev != nil {
					text.WriteString(delta)
					select {
					case ch <- *ev:
					case <-ctx.Done():
						return
					}
				}
			case *types.ConverseStreamOutputMemberMetadata:
				usage = mapBedrockUsage(v.Value.Usage)
			case *types.ConverseStreamOutputMemberMessageStop,
				*types.ConverseStreamOutputMemberMessageStart,
				*types.ConverseStreamOutputMemberContentBlockStart,
				*types.ConverseStreamOutputMemberContentBlockStop:
				// Lifecycle markers — result emitted after stream ends so
				// late metadata can still fill Usage.
			default:
				// Unknown / exception members surface via stream.Err().
			}
		}
		if err := stream.Err(); err != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case ch <- TaskEvent{
				Type:     TaskEventError,
				IsError:  true,
				ErrorMsg: mapBedrockAPIError(err),
			}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case ch <- TaskEvent{
			Type:       TaskEventResult,
			Content:    text.String(),
			Usage:      usage,
			DurationMs: float64(time.Since(start).Milliseconds()),
		}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func extractTextDelta(delta types.ContentBlockDelta) string {
	if delta == nil {
		return ""
	}
	if t, ok := delta.(*types.ContentBlockDeltaMemberText); ok {
		return t.Value
	}
	return ""
}
