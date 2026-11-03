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
		}),
	},
	{
		Name:        "send",
		Description: "Send a message to another agent or to \"owner\", and return immediately. Your turn ends when you stop calling tools; the recipient's reply arrives as your next message. A message to yourself with after_seconds is a timer.",
		InputSchema: fleetSchema([]string{"to", "text"}, map[string]any{
			"to":            fleetProperty("string", "An agent's name, or \"owner\"."),
			"text":          fleetProperty("string", "The message."),
			"after_seconds": fleetProperty("number", "Deliver after this delay. Works even if you go idle."),
		}),
	},
	{
		Name:        "stop",
		Description: "Stop yourself or one of your descendants for good. Whoever it owed a reply hears that it has gone.",
		InputSchema: fleetSchema([]string{"name"}, map[string]any{
			"name": fleetProperty("string", "The agent to stop."),
		}),
	},
	{
		Name:        "list",
		Description: "List every agent in this fleet as it is right now: state, parent, queue length and seconds since it last did anything.",
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
		writeFleetJSON(w, fleetRPCResult(req.ID, map[string]any{"tools": fleetTools}))
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
			Name             string  `json:"name"`
			Instructions     string  `json:"instructions"`
			InstructionsFile string  `json:"instructions_file"`
			WorkDir          string  `json:"workdir"`
			Quality          string  `json:"quality"`
			Purpose          string  `json:"purpose"`
			Model            string  `json:"model"`
			To               string  `json:"to"`
			Text             string  `json:"text"`
			AfterSeconds     float64 `json:"after_seconds"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return "", fmt.Errorf("bad tool call: %w", err)
	}
	args := call.Arguments
	switch call.Name {
	case "start":
		err := k.startForAgent(fleet, caller, &FleetStartArgs{
			Name: args.Name, Instructions: args.Instructions, InstructionsFile: args.InstructionsFile,
			WorkDir: args.WorkDir, Quality: args.Quality, Purpose: args.Purpose, Model: args.Model,
		})
		if err != nil {
			return "", err
		}
		return "starting " + args.Name, nil
	case "send":
		after := time.Duration(args.AfterSeconds * float64(time.Second))
		id, err := k.Send(fleet, caller, args.To, FleetRequest, args.Text, after)
		if err != nil {
			return "", err
		}
		return "queued as " + id, nil
	case "stop":
		if err := k.Stop(fleet, caller, args.Name); err != nil {
			return "", err
		}
		return "stopping " + args.Name, nil
	case "list":
		st, err := k.Status(fleet)
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
