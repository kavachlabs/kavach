package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kavachlabs/kavach"
)

// The MCP server speaks JSON-RPC 2.0 over stdio, one message per line
// (https://modelcontextprotocol.io/specification). It implements only what
// tools need: initialize, ping, tools/list and tools/call.

const mcpLatestVersion = "2025-06-18"

var mcpVersions = []string{mcpLatestVersion, "2025-03-26", "2024-11-05"}

const mcpInstructions = `Kavach replays production crashes of journal-driven Go services from fixture files (*.kavach) and verifies fixes.
Workflow: kavach_list_incidents to find fixtures; kavach_replay with the current build to reproduce (expect still_failing@N) before changing code; fix the handler, never the fixture; build old and new binaries; kavach_diff to verify. A fix counts only when kavach_diff's verdict is "fixed". Report verdicts exactly as returned.`

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

func cmdMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("mcp", stderr)
	if _, err := parse(fs, args); err != nil {
		return exitUsage
	}
	if err := serveMCP(stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "kavach mcp: %v\n", err)
		return exitError
	}
	return exitPass
}

// serveMCP answers requests from r on w until r ends.
func serveMCP(r io.Reader, w io.Writer) error {
	in := bufio.NewReader(r)
	enc := json.NewEncoder(w)
	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := handleMCP(line); resp != nil {
				if werr := enc.Encode(resp); werr != nil {
					return werr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// handleMCP answers one message; it returns nil for notifications.
func handleMCP(line []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{rpcParseError, "parse error: " + err.Error()}}
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // a notification, e.g. notifications/initialized
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if req.JSONRPC != "2.0" || req.Method == "" {
		resp.Error = &rpcError{rpcInvalidRequest, "not a JSON-RPC 2.0 request"}
		return resp
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := mcpLatestVersion
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kavach", "version": kavach.Version},
			"instructions":    mcpInstructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{rpcInvalidParams, err.Error()}
			return resp
		}
		tool, ok := mcpHandlers[p.Name]
		if !ok {
			resp.Error = &rpcError{rpcInvalidParams, "unknown tool " + p.Name}
			return resp
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		out, err := tool(p.Arguments)
		resp.Result = toolResult(out, err)
	default:
		resp.Error = &rpcError{rpcMethodNotFound, "method not found: " + req.Method}
	}
	return resp
}

// toolResult wraps a tool's output. A verdict that is not a pass is a normal
// result; isError is reserved for tools that could not run.
func toolResult(out map[string]any, err error) map[string]any {
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(b)}},
		"structuredContent": out,
	}
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var mcpTools = []map[string]any{
	{
		"name":        "kavach_list_incidents",
		"title":       "List incidents",
		"description": "List Kavach fixtures (*.kavach files) under a directory, recursively, with the failure each recorded: kind (panic, error, invariant), the seq of the failing input and the message. Saved variants of an incident are listed with their mutation.",
		"inputSchema": obj(map[string]any{
			"dir": str("directory to search; defaults to the server's working directory"),
		}),
		"annotations": map[string]any{"readOnlyHint": true},
	},
	{
		"name":  "kavach_replay",
		"title": "Replay an incident",
		"description": "Replay a fixture through a replay binary (a Go binary whose main calls kavach.MaybeReplay) and return the verdict. " +
			"Use it to reproduce an incident with the current build before changing code: expect still_failing@N, where N is the seq of the failing input. " +
			"Verdicts: still_failing@N, invariant_violated(X)@N, diverged@N, nondeterministic@N, fixed, ok.",
		"inputSchema": obj(map[string]any{
			"fixture":       str("path to the .kavach fixture"),
			"bin":           str("replay binary; defaults to $KAVACH_BIN"),
			"include_steps": map[string]any{"type": "boolean", "description": "also return every step's outputs"},
		}, "fixture"),
		"annotations": map[string]any{"readOnlyHint": true},
	},
	{
		"name":  "kavach_diff",
		"title": "Verify a fix",
		"description": "Verify a candidate fix: replay the fixture with the old (failing) and new binaries, then check the new binary against variants of the incident on which the old one fails the same way. " +
			"The fix counts only when verdict is \"fixed\". variant_failed(K)@N: the fix is too narrow; the failing variant is saved (variants.checks[].file) and can be replayed with kavach_replay. " +
			"unverified: too few variants reproduce the incident to check the fix. Any other verdict is the new binary's replay of the fixture itself.",
		"inputSchema": obj(map[string]any{
			"fixture": str("path to the .kavach fixture"),
			"old":     str("replay binary built from the code that failed"),
			"new":     str("replay binary built from the candidate fix"),
			"variants": map[string]any{"type": "integer", "minimum": 0,
				"description": fmt.Sprintf("variants that must reproduce the incident and pass; default %d, 0 checks only the fixture", kavach.DefaultMinVariants)},
			"keep": str("directory to save failing variants in; defaults to a new temporary directory"),
		}, "fixture", "old", "new"),
	},
}

var mcpHandlers = map[string]func(json.RawMessage) (map[string]any, error){
	"kavach_list_incidents": mcpListIncidents,
	"kavach_replay":         mcpReplay,
	"kavach_diff":           mcpDiff,
}

func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func mcpListIncidents(raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Dir string `json:"dir"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Dir == "" {
		a.Dir = "."
	}
	list, truncated, err := listIncidents(a.Dir)
	if err != nil {
		return nil, err
	}
	return map[string]any{"root": a.Dir, "incidents": orEmpty(list), "truncated": truncated}, nil
}

func mcpReplay(raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Fixture      string `json:"fixture"`
		Bin          string `json:"bin"`
		IncludeSteps bool   `json:"include_steps"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Fixture == "" {
		return nil, errors.New("fixture is required")
	}
	if a.Bin == "" {
		a.Bin = os.Getenv("KAVACH_BIN")
	}
	res, wall, err := replayWith(a.Bin, a.Fixture)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"fixture": a.Fixture, "verdict": res.String(), "passed": res.Passed(), "status": res.Status,
		"detail": res.Detail, "service": res.Service, "start": res.Start, "records": res.Records,
		"steps_replayed": len(res.Steps), "synthesized_reads": synthesized(res), "wall_ms": ms(wall),
	}
	if res.Seq != nil {
		out["seq"] = *res.Seq
	}
	if res.Recorded != nil {
		out["recorded_failure"] = res.Recorded
	}
	if res.Variant != "" {
		out["variant"] = res.Variant
	}
	if a.IncludeSteps {
		out["steps"] = res.Steps
	}
	return out, nil
}

func mcpDiff(raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Fixture  string `json:"fixture"`
		Old      string `json:"old"`
		New      string `json:"new"`
		Variants *int   `json:"variants"`
		Keep     string `json:"keep"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Fixture == "" || a.Old == "" || a.New == "" {
		return nil, errors.New("fixture, old and new are required")
	}
	min := kavach.DefaultMinVariants
	if a.Variants != nil {
		if *a.Variants < 0 {
			return nil, errors.New("variants must not be negative")
		}
		min = *a.Variants
	}
	o, err := verifyFix(a.Fixture, a.Old, a.New, min, a.Keep)
	if err != nil {
		return nil, err
	}
	out := o.report()
	out["passed"] = o.v.Passed()
	return out, nil
}
