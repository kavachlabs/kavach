package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
)

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
	info := init["serverInfo"].(map[string]any)
	// The server's version is the CLI's, which a release sets, not the protocol's.
	if init["protocolVersion"] != "2025-03-26" || info["name"] != "kavach" || info["version"] != version {
		t.Fatalf("initialize: %v", init)
	}
	if resps[1]["id"] != "two" || resps[1]["result"] == nil {
		t.Fatalf("ping: %v", resps[1])
	}
	var names []string
	var mapTool map[string]any
	for _, tool := range resps[2]["result"].(map[string]any)["tools"].([]any) {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["inputSchema"].(map[string]any)["type"] != "object" || tm["description"] == "" {
			t.Fatalf("tool %v", tm)
		}
		if tm["name"] == "kavach_map" {
			mapTool = tm
		}
	}
	if strings.Join(names, ",") != "kavach_list_incidents,kavach_replay,kavach_time_travel,kavach_diff,kavach_contract_show,kavach_contract_check,kavach_map" {
		t.Fatalf("tools: %v", names)
	}
	props := mapTool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, wantProp := range []string{"spec", "fixtures", "bin", "new", "diffs", "overlays", "out", "max_data", "title", "include_model"} {
		if _, ok := props[wantProp]; !ok {
			t.Fatalf("kavach_map inputSchema missing property %q: %v", wantProp, props)
		}
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
	host, work := storeContract(t)
	traffic := work + "/traffic.kavach"
	resps := mcpSession(t,
		call(1, "kavach_list_incidents", `{"dir":"../../examples"}`),
		call(2, "kavach_replay", `{"fixture":"`+fixture+`"}`),
		call(3, "kavach_replay", `{"fixture":"`+fixture+`","colour":true}`),
		call(4, "kavach_diff", `{"fixture":"`+fixture+`","old":"x"}`),
		call(5, "kavach_diff", `{"fixture":"`+fixture+`","old":"x","new":"y","variants":-1}`),
		call(6, "kavach_list_incidents", `{"dir":"/does/not/exist"}`),
		call(7, "kavach_diff", `{"fixture":"`+fixture+`","old":"x","new":"y","regressions":-1}`),
		call(8, "kavach_time_travel", `{"fixture":"`+traffic+`","bin":"`+host+` base","until":3,"probes":["{\"argv\":[\"GET\",\"greeting\"]}"],"include_steps":true}`),
	)
	sc, text, isErr := toolOutput(t, resps[0])
	incs := sc["incidents"].([]any)
	if isErr || len(incs) != 1 || !strings.Contains(text, `"seq": 32`) {
		t.Fatalf("list: %s", text)
	}
	if f := incs[0].(map[string]any)["failure"].(map[string]any); f["kind"] != "panic" || f["seq"] != 32.0 {
		t.Fatalf("failure: %v", f)
	}
	for i, want := range map[int]string{
		1: "no host command", 2: `unknown field "colour"`, 3: "fixture, old and new are required",
		4: "variants must not be negative", 5: "no such file", 6: "regressions must not be negative",
	} {
		_, text, isErr := toolOutput(t, resps[i])
		if !isErr || !strings.Contains(text, want) {
			t.Fatalf("call %d: isError=%v %q, want %q", i+1, isErr, text, want)
		}
	}
	ttSC, ttText, ttErr := toolOutput(t, resps[7])
	if ttErr || ttSC["verdict"] != "ok" || ttSC["steps_replayed"] != 2.0 || ttSC["held_at_seq"] != 3.0 {
		t.Fatalf("time_travel: err=%v %s", ttErr, ttText)
	}
	probes := ttSC["probes"].([]any)
	if len(probes) != 1 {
		t.Fatalf("probes: %v", probes)
	}
	p0 := probes[0].(map[string]any)
	outs := p0["outputs"].([]any)
	if p0["outcome"] != "ok" || len(outs) != 1 || outs[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("probe 0: %v", p0)
	}
}

func TestListCommand(t *testing.T) {
	code, out, _ := runCLI(t, "list", "../../examples")
	if code != exitPass || !strings.Contains(out, "null-amount.kavach") || !strings.Contains(out, "panic at seq 32") {
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

func TestMCPMap(t *testing.T) {
	dir := t.TempDir()

	fxPath := filepath.Join(dir, "incident.kavach")
	jf, err := journal.OpenFile(fxPath, journal.Meta{Service: "billing"}, journal.FileOptions{})
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	records := []journal.Record{
		{Type: journal.TypeEnvironment, Flags: journal.FlagCritical},
		{Type: journal.TypeInput, Source: "queue", Position: "1", Data: []byte(`{"op":"charge","amount":100}`)},
		{Type: journal.TypeOutput, Sink: "ledger", Data: []byte(`{"ok":true}`)},
		{Type: journal.TypeInput, Source: "queue", Position: "2", Data: []byte(`{"op":"charge","amount":null}`)},
		{Type: journal.TypeMarker, Kind: journal.MarkerPanic, Message: "nil amount"},
	}
	for _, r := range records {
		if _, err := jf.Append(r); err != nil {
			t.Fatalf("append record: %v", err)
		}
	}
	if err := jf.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	specPath := filepath.Join(dir, "map.json")
	specJSON := `{
		"kavach_map": 1,
		"title": "Billing Map",
		"groups": [
			{"id": "in", "title": "Inputs"},
			{"id": "out", "title": "Outputs"}
		],
		"nodes": [
			{"id": "charge", "group": "in", "kind": "entry", "title": "Charge", "match": [{"on": "input", "source": "queue"}]},
			{"id": "ledger", "group": "out", "kind": "sink", "title": "Ledger", "match": [{"on": "output", "sink": "ledger"}]}
		],
		"edges": [
			{"from": "charge", "to": "ledger", "kind": "emits"}
		],
		"flows": [
			{"id": "happy", "title": "Charge Flow", "steps": [{"node": "charge", "text": "Receive charge"}, {"node": "ledger", "text": "Write ledger"}]}
		]
	}`
	if err := os.WriteFile(specPath, []byte(specJSON), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	outHTML := filepath.Join(dir, "out", "billing-map.html")

	mustJSON := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	resps := mcpSession(t,
		call(1, "kavach_map", mustJSON(map[string]any{
			"spec":     specPath,
			"fixtures": []string{fxPath},
			"out":      outHTML,
		})),
		call(2, "kavach_map", mustJSON(map[string]any{
			"spec":          specPath,
			"fixtures":      []string{fxPath},
			"include_model": true,
		})),
		call(3, "kavach_map", `{}`),
		call(4, "kavach_map", mustJSON(map[string]any{
			"fixtures": []string{fxPath},
			"new":      "./fixed-host",
		})),
		call(5, "kavach_map", mustJSON(map[string]any{
			"fixtures": []string{fxPath},
			"max_data": -1,
		})),
	)

	sc, text, isErr := toolOutput(t, resps[0])
	if isErr {
		t.Fatalf("kavach_map summary error: %s", text)
	}
	if _, hasModel := sc["model"]; hasModel {
		t.Fatalf("summary mode should not include model: %v", sc)
	}
	if sc["title"] != "Billing Map" || sc["spec_path"] != specPath || sc["out_path"] != outHTML {
		t.Fatalf("summary header mismatch: %v", sc)
	}
	htmlBytes, _ := sc["html_bytes"].(float64)
	if htmlBytes <= 0 {
		t.Fatalf("expected html_bytes > 0, got %v", sc["html_bytes"])
	}
	if sc["nodes"].(float64) < 2 || sc["edges"].(float64) < 1 || sc["flows"].(float64) != 1 {
		t.Fatalf("unexpected graph counts: nodes=%v edges=%v flows=%v", sc["nodes"], sc["edges"], sc["flows"])
	}
	fxs := sc["fixtures"].([]any)
	if len(fxs) != 1 {
		t.Fatalf("expected 1 fixture, got %v", fxs)
	}
	runs := fxs[0].(map[string]any)["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %v", runs)
	}
	r0 := runs[0].(map[string]any)
	if r0["id"] != "recorded" || r0["steps"].(float64) != 2 || !strings.HasPrefix(r0["verdict"].(string), "panic@") {
		t.Fatalf("unexpected run summary: %v", r0)
	}
	ff, ok := r0["first_failure"].(map[string]any)
	if !ok || ff["outcome"] != "panic" || ff["message"] != "nil amount" {
		t.Fatalf("unexpected first_failure: %v", r0["first_failure"])
	}
	if _, ok := sc["overlays"].([]any); !ok {
		t.Fatalf("missing overlays slice: %v", sc)
	}
	if _, ok := sc["warnings"].([]any); !ok {
		t.Fatalf("missing warnings slice: %v", sc)
	}
	if stats, ok := sc["stats"].(map[string]any); !ok || len(stats) < 2 {
		t.Fatalf("unexpected stats: %v", sc["stats"])
	}

	htmlData, err := os.ReadFile(outHTML)
	if err != nil {
		t.Fatalf("read written HTML: %v", err)
	}
	if len(htmlData) != int(htmlBytes) || !strings.Contains(string(htmlData), `id="kavach-map-data"`) {
		t.Fatalf("HTML output invalid: len=%d html_bytes=%v", len(htmlData), htmlBytes)
	}

	scFull, textFull, isErrFull := toolOutput(t, resps[1])
	if isErrFull {
		t.Fatalf("kavach_map include_model error: %s", textFull)
	}
	model, ok := scFull["model"].(map[string]any)
	if !ok {
		t.Fatalf("expected model in include_model=true response: %v", scFull)
	}
	if model["kavach_map_model"] != 1.0 || model["title"] != "Billing Map" {
		t.Fatalf("unexpected model: %v", model)
	}

	for i, want := range map[int]string{
		2: "at least one of spec, fixtures, or overlays is required",
		3: "new requires bin",
		4: "max_data must not be negative",
	} {
		_, errText, errFlag := toolOutput(t, resps[i])
		if !errFlag || !strings.Contains(errText, want) {
			t.Fatalf("call %d: isError=%v %q, want %q", i+1, errFlag, errText, want)
		}
	}
}
