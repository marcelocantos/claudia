// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"
)

// FleetMCPServerName names the seat-facing MCP server. Every seat uses the same
// name with its own URL, whose final path segment is its token.
const FleetMCPServerName = "fleet"

const (
	fleetMCPProtocol   = "2025-03-26"
	fleetMCPMaxBody    = 8 << 20
	jsonRPCMethodError = -32601
	jsonRPCParseError  = -32700
)

type fleetTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func fleetSchema(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func fleetProperty(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

// fleetTools is the whole surface: lifecycle and communication, nothing else.
var fleetTools = []fleetTool{
	{
		Name:        "start",
		Description: "Start a child agent and return at once. You can send it messages straight away, and they wait until it is up. If it cannot start, a message tells you. It receives its instructions with its first message.",
		InputSchema: fleetSchema([]string{"name"}, map[string]any{
			"name":              fleetProperty("string", "Lowercase letters, digits and single hyphens. Unique in this fleet."),
			"instructions":      fleetProperty("string", "What this agent is and how it behaves. Set this or instructions_file."),
			"instructions_file": fleetProperty("string", "Absolute path of a file holding the instructions."),
			"workdir":           fleetProperty("string", "Absolute directory it works in. Default: yours."),
			"quality":           fleetProperty("string", "economy, standard or frontier. Default: standard."),
			"purpose":           fleetProperty("string", "What the model should be good at, if it matters."),
			"model":             fleetProperty("string", "A specific model, only if you need that one."),
			"tools":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The tools it may call, chosen from your own. Default: all of yours."},
			"budget": map[string]any{"type": "object", "description": "Optional limit on what it and its descendants spend. Every budget over you still applies, so this can only narrow.",
				"properties": map[string]any{
					"tokens":  fleetProperty("integer", "Tokens, where the provider reports them."),
					"turns":   fleetProperty("integer", "Turns, across its whole subtree."),
					"seconds": fleetProperty("number", "Wall-clock seconds from now."),
				}},
		}),
	},
	{
		Name:        "send",
		Description: "Send a message to another agent or to \"owner\", now or later, and return immediately. Your turn ends when you stop calling tools; the recipient's reply arrives as your next message. A scheduled message to yourself is a timer, and it arrives even if you go idle. Scheduled messages return an ID that cancel takes. Every message you receive starts with the time now and how long it took to reach you.",
		InputSchema: fleetSchema([]string{"to", "text"}, map[string]any{
			"to":                    fleetProperty("string", "An agent's name, or \"owner\"."),
			"text":                  fleetProperty("string", "The message."),
			"after_seconds":         fleetProperty("number", "Deliver after this delay."),
			"at":                    fleetProperty("string", "Deliver at this time, in RFC 3339 form (2026-11-03T15:00:00+11:00)."),
			"every_seconds":         fleetProperty("number", "Repeat this often, to yourself only, at least 60 seconds apart, until cancelled."),
			"accept_within_seconds": fleetProperty("number", "How soon you expect it to be taken up. If it is not, you are woken once with a notice from clock saying where it stands."),
			"answer_within_seconds": fleetProperty("number", "How soon you expect an answer. If none comes, you are woken once with a notice from clock saying where it stands."),
			"mode":                  fleetProperty("string", "steer or interrupt, to expedite it as it is sent, as the expedite tool does."),
		}),
	},
	{
		Name:        "expedite",
		Description: "Move a message waiting in an agent's queue ahead of the rest. steer folds it into the agent's running turn, whose reply then answers it too; not every agent's provider can steer. interrupt ends the running turn, whose requester hears it was interrupted, so the message goes next. Either way it goes first in the queue if that cannot be done. Takes authority over the agent.",
		InputSchema: fleetSchema([]string{"id", "mode"}, map[string]any{
			"id":   fleetProperty("string", "The message's ID, as send returned it."),
			"mode": fleetProperty("string", "steer or interrupt."),
		}),
	},
	{
		Name:        "stop",
		Description: "Stop an agent for good: yourself, or one you have authority over. You have authority over agents you started and everything they started, and over everything if the owner gave you that. Whoever it owed a reply hears that it has gone.",
		InputSchema: fleetSchema([]string{"name"}, map[string]any{
			"name": fleetProperty("string", "The agent to stop."),
		}),
	},
	{
		Name:        "cancel",
		Description: "Cancel a message you scheduled, by the ID send returned.",
		InputSchema: fleetSchema([]string{"id"}, map[string]any{
			"id": fleetProperty("string", "The scheduled message's ID."),
		}),
	},
	{
		Name:        "check",
		Description: "Run one of the owner's checks against a commit, and return at once. The result arrives later as a message from check:<name>, and is recorded where no agent can change it. list shows the checks you may run.",
		InputSchema: fleetSchema([]string{"name"}, map[string]any{
			"name":   fleetProperty("string", "The check's name."),
			"commit": fleetProperty("string", "The commit or branch to check out. Default: HEAD of the check's repository."),
		}),
	},
	{
		Name:        "budget",
		Description: "Set the limits on a budget over an agent you have authority over, named by its ID or by the agent holding it, creating one if it holds none. You need authority over whoever set it before, too. Every budget above an agent still applies. Once spent, no new turn starts under it until it is raised. Zero in a dimension means unlimited.",
		InputSchema: fleetSchema([]string{"target"}, map[string]any{
			"target":  fleetProperty("string", "An agent's name, or a budget ID such as b3."),
			"tokens":  fleetProperty("integer", "Tokens, where the provider reports them."),
			"turns":   fleetProperty("integer", "Turns, across the whole subtree."),
			"seconds": fleetProperty("number", "Wall-clock seconds from now."),
		}),
	},
	{
		Name:        "register_check",
		Description: "Register a check, or replace one of the same name if you have authority over whoever registered it: a script Claudia pins by hash and runs against a fresh export of a commit, recording the result where no agent can change it. The last line of its output, if a number, is the score. A check other agents may not run is held out: only you and those with authority over you can run it or see it.",
		InputSchema: fleetSchema([]string{"name", "repo"}, map[string]any{
			"name":            fleetProperty("string", "Lowercase letters, digits and single hyphens."),
			"repo":            fleetProperty("string", "Absolute path of the git repository."),
			"script":          fleetProperty("string", "The shell script. Set this or script_file."),
			"script_file":     fleetProperty("string", "Absolute path of a file holding the script; its contents are copied now."),
			"agents_may_run":  fleetProperty("boolean", "Let any agent run it with the check tool. Default: false, held out."),
			"timeout_seconds": fleetProperty("number", "Ends a run that takes longer. Default: 1800."),
		}),
	},
	{
		Name:        "list",
		Description: "List this fleet as it is right now: every agent with its state, parent, queue length, seconds since it last did anything and how long its current turn has run; every scheduled message; every budget and what is spent; and the checks you may run.",
		InputSchema: fleetSchema(nil, map[string]any{}),
	},
}

type jsonRPCRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func fleetRPCResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func fleetRPCError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

// ServeHTTP is the stateless MCP endpoint each seat calls. It keeps no sessions,
// so a seat's client carries on unchanged across a broker restart. The URL's
// final segment says which agent is calling.
func (k *FleetKernel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this server is stateless and offers no stream", http.StatusMethodNotAllowed)
		return
	}
	fleet, caller, ok := k.AgentByToken(path.Base(r.URL.Path))
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, fleetMCPMaxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeFleetJSON(w, fleetRPCError(nil, jsonRPCParseError, err.Error()))
		return
	}
	// A notification has no ID and gets no response.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		writeFleetJSON(w, fleetRPCResult(req.ID, map[string]any{
			"protocolVersion": fleetMCPProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": FleetMCPServerName, "version": Version},
		}))
	case "ping":
		writeFleetJSON(w, fleetRPCResult(req.ID, map[string]any{}))
	case "tools/list":
		writeFleetJSON(w, fleetRPCResult(req.ID, map[string]any{"tools": k.toolsOf(fleet, caller)}))
	case "tools/call":
		text, err := k.callFleetTool(r, fleet, caller, req.Params)
		result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		if err != nil {
			result["content"] = []map[string]any{{"type": "text", "text": err.Error()}}
			result["isError"] = true
		}
		writeFleetJSON(w, fleetRPCResult(req.ID, result))
	default:
		writeFleetJSON(w, fleetRPCError(req.ID, jsonRPCMethodError, "unknown method "+req.Method))
	}
}

func writeFleetJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (k *FleetKernel) callFleetTool(r *http.Request, fleet, caller string, params json.RawMessage) (string, error) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Name             string       `json:"name"`
			Instructions     string       `json:"instructions"`
			InstructionsFile string       `json:"instructions_file"`
			WorkDir          string       `json:"workdir"`
			Quality          string       `json:"quality"`
			Purpose          string       `json:"purpose"`
			Model            string       `json:"model"`
			To               string       `json:"to"`
			Text             string       `json:"text"`
			AfterSeconds     float64      `json:"after_seconds"`
			At               string       `json:"at"`
			EverySeconds     float64      `json:"every_seconds"`
			ID               string       `json:"id"`
			Commit           string       `json:"commit"`
			Budget           *FleetBudget `json:"budget"`
			Tools            []string     `json:"tools"`
			Repo             string       `json:"repo"`
			Script           string       `json:"script"`
			ScriptFile       string       `json:"script_file"`
			AgentsMayRun     bool         `json:"agents_may_run"`
			TimeoutSeconds   float64      `json:"timeout_seconds"`
			Target           string       `json:"target"`
			Tokens           int64        `json:"tokens"`
			Turns            int          `json:"turns"`
			Seconds          float64      `json:"seconds"`
			AcceptWithin     float64      `json:"accept_within_seconds"`
			AnswerWithin     float64      `json:"answer_within_seconds"`
			Mode             string       `json:"mode"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return "", fmt.Errorf("bad tool call: %w", err)
	}
	args := call.Arguments
	if !k.holdsTool(fleet, caller, call.Name) {
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
	switch call.Name {
	case "start":
		err := k.startForAgent(fleet, caller, &FleetStartArgs{
			Name: args.Name, Instructions: args.Instructions, InstructionsFile: args.InstructionsFile,
			WorkDir: args.WorkDir, Quality: args.Quality, Purpose: args.Purpose, Model: args.Model, Budget: args.Budget,
			Tools: args.Tools,
		})
		if err != nil {
			return "", err
		}
		return "starting " + args.Name, nil
	case "send":
		send := &FleetSendArgs{From: caller, To: args.To, Kind: FleetRequest, Text: args.Text,
			After: fleetSeconds(args.AfterSeconds), Every: fleetSeconds(args.EverySeconds),
			AcceptWithin: fleetSeconds(args.AcceptWithin), AnswerWithin: fleetSeconds(args.AnswerWithin), Mode: args.Mode}
		if args.At != "" {
			at, err := time.Parse(time.RFC3339, args.At)
			if err != nil {
				return "", fmt.Errorf("at %q is not an RFC 3339 time: %w", args.At, err)
			}
			send.At = at
		}
		id, note, err := k.Deliver(fleet, send)
		if err != nil {
			return "", err
		}
		if note != "" {
			return "queued as " + id + "; " + note, nil
		}
		return "queued as " + id, nil
	case "expedite":
		return k.Expedite(fleet, caller, args.ID, args.Mode)
	case "cancel":
		if err := k.Cancel(fleet, caller, args.ID); err != nil {
			return "", err
		}
		return "cancelled " + args.ID, nil
	case "budget":
		if err := k.SetBudget(fleet, caller, args.Target, FleetBudget{Tokens: args.Tokens, Turns: args.Turns, Seconds: args.Seconds}); err != nil {
			return "", err
		}
		return "budget set on " + args.Target, nil
	case "register_check":
		if err := k.RegisterCheck(fleet, caller, &FleetCheckArgs{Name: args.Name, Repo: args.Repo, Script: args.Script, ScriptFile: args.ScriptFile,
			AgentsMayRun: args.AgentsMayRun, Timeout: fleetSeconds(args.TimeoutSeconds)}); err != nil {
			return "", err
		}
		return "registered check " + args.Name, nil
	case "check":
		id, err := k.RunCheck(fleet, caller, args.Name, args.Commit)
		if err != nil {
			return "", err
		}
		return "running as " + id + "; the result will arrive as a message", nil
	case "stop":
		if err := k.Stop(fleet, caller, args.Name); err != nil {
			return "", err
		}
		return "stopping " + args.Name, nil
	case "list":
		st, err := k.StatusFor(fleet, caller)
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(st)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	default:
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
}

// fleetSeconds turns a tool's seconds into a duration.
func fleetSeconds(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}
