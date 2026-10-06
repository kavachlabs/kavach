package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mcpSession sends each message to a server and decodes every response.
func mcpSession(t *testing.T, msgs ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(strings.Join(msgs, "\n")), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, m)
	}
	return resps
}

func call(id int, tool string, args string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestMCPProtocol(t *testing.T) {
	resps := mcpSession(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope"}}`,
	)
	if len(resps) != 7 {
		t.Fatalf("got %d responses, want 7 (the notification gets none): %v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" || init["serverInfo"].(map[string]any)["name"] != "kavach" {
		t.Fatalf("initialize: %v", init)
	}
	if resps[1]["id"] != "two" || resps[1]["result"] == nil {
		t.Fatalf("ping: %v", resps[1])
	}
	var names []string
	for _, tool := range resps[2]["result"].(map[string]any)["tools"].([]any) {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["inputSchema"].(map[string]any)["type"] != "object" || tm["description"] == "" {
			t.Fatalf("tool %v", tm)
		}
	}
	if strings.Join(names, ",") != "kavach_list_incidents,kavach_replay,kavach_diff" {
		t.Fatalf("tools: %v", names)
	}
	for i, code := range map[int]float64{3: rpcMethodNotFound, 4: rpcParseError, 6: rpcInvalidParams} {
		if e, _ := resps[i]["error"].(map[string]any); e == nil || e["code"] != code {
			t.Fatalf("response %d: %v, want error %v", i, resps[i], code)
		}
	}
	if v := resps[5]["result"].(map[string]any)["protocolVersion"]; v != mcpLatestVersion {
		t.Fatalf("unsupported version negotiated to %v", v)
	}
}

func toolOutput(t *testing.T, resp map[string]any) (map[string]any, string, bool) {
	t.Helper()
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := r["isError"].(bool)
	sc, _ := r["structuredContent"].(map[string]any)
	return sc, text, isErr
}

func TestMCPTools(t *testing.T) {
	t.Setenv("KAVACH_BIN", "")
	resps := mcpSession(t,
		call(1, "kavach_list_incidents", `{"dir":"../../examples"}`),
		call(2, "kavach_replay", `{"fixture":"`+fixture+`"}`),
		call(3, "kavach_replay", `{"fixture":"`+fixture+`","colour":true}`),
		call(4, "kavach_diff", `{"fixture":"`+fixture+`","old":"x"}`),
		call(5, "kavach_diff", `{"fixture":"`+fixture+`","old":"x","new":"y","variants":-1}`),
		call(6, "kavach_list_incidents", `{"dir":"/does/not/exist"}`),
	)
	sc, text, isErr := toolOutput(t, resps[0])
	incs := sc["incidents"].([]any)
	if isErr || len(incs) != 1 || !strings.Contains(text, `"seq": 31`) {
		t.Fatalf("list: %s", text)
	}
	if f := incs[0].(map[string]any)["failure"].(map[string]any); f["kind"] != "panic" || f["seq"] != 31.0 {
		t.Fatalf("failure: %v", f)
	}
	for i, want := range map[int]string{
		1: "no replay binary", 2: `unknown field "colour"`, 3: "fixture, old and new are required",
		4: "variants must not be negative", 5: "no such file",
	} {
		_, text, isErr := toolOutput(t, resps[i])
		if !isErr || !strings.Contains(text, want) {
			t.Fatalf("call %d: isError=%v %q, want %q", i+1, isErr, text, want)
		}
	}
}

func TestListCommand(t *testing.T) {
	code, out, _ := runCLI(t, "list", "../../examples")
	if code != exitPass || !strings.Contains(out, "null-amount.kavach") || !strings.Contains(out, "panic at seq 31") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	code, out, _ = runCLI(t, "list", "--json", t.TempDir())
	if code != exitPass || !strings.Contains(out, `"incidents": []`) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if code, _, _ := runCLI(t, "list", "a", "b"); code != exitUsage {
		t.Fatalf("exit %d for two directories", code)
	}
}
