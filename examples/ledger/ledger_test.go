package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/kavachtest"
)

const fixture = "testdata/null-amount.kavach"

func newHandler() kavach.Handler { return NewLedger() }

// TestNullAmountFixture replays the captured production crash. With the planted
// bug it must still fail; built with -tags ledgerfix it must be fixed.
func TestNullAmountFixture(t *testing.T) {
	res, err := kavach.ReplayFile(fixture, newHandler)
	if err != nil {
		t.Fatal(err)
	}
	want := "still_failing@32"
	if fixNullAmount {
		want = "fixed"
	}
	if res.String() != want {
		t.Fatalf("got %s (%s), want %s", res, res.Detail, want)
	}
}

// TestRegressionFixtures is what a service's suite looks like once its bug is
// fixed: every captured incident replays as a passing Go test.
func TestRegressionFixtures(t *testing.T) {
	if !fixNullAmount {
		t.Skip("the demo ships with the bug planted; run with -tags ledgerfix")
	}
	kavachtest.Run(t, "testdata/*.kavach", newHandler)
}

func TestInvariantsHoldOnCleanInput(t *testing.T) {
	r := kavach.NewRecorder(NewLedger(), kavach.Options{Dir: t.TempDir(), RecoverPanics: true})
	for _, ev := range []string{
		`{"id":"1","type":"deposit","account":"a","amount":10}`,
		`{"id":"2","type":"transfer","account":"a","to":"b","amount":4}`,
		`{"id":"3","type":"withdraw","account":"b","amount":5}`,
		`{"id":"4","type":"refund","account":"b","amount":1}`,
		`{"id":"5","type":"deposit","account":"b","amount":-1}`,
	} {
		if err := r.Step(kavach.Input{Source: "test", Data: []byte(ev)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Step(kavach.Input{Source: "test", Data: []byte(`not json`)}); err == nil {
		t.Fatal("expected a decode error")
	}
}

// TestCLI drives the real binaries the way the README demo does.
func TestCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	dir := t.TempDir()
	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		return filepath.Join(dir, name)
	}
	build := func(out string, args ...string) {
		cmd := exec.Command("go", append([]string{"build", "-o", out}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", args, err, b)
		}
	}
	cli, oldBin, newBin, partialBin := exe("kavach"), exe("ledger-old"), exe("ledger-new"), exe("ledger-partial")
	build(cli, "github.com/kavachlabs/kavach/cmd/kavach")
	build(oldBin, ".")
	build(newBin, "-tags", "ledgerfix", ".")
	build(partialBin, "-tags", "ledgerpartialfix", ".")

	// The buggy service crashes and leaves a fixture behind.
	fx := filepath.Join(dir, "fixtures")
	if err := exec.Command(oldBin, "-in", "testdata/events.jsonl", "-fixtures", fx).Run(); err == nil {
		t.Fatal("expected the buggy ledger to crash")
	}
	paths, _ := filepath.Glob(filepath.Join(fx, "*.kavach"))
	if len(paths) != 1 {
		t.Fatalf("fixtures written: %v", paths)
	}
	j, err := journal.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if last := j.Records[len(j.Records)-1]; last.Kind != journal.MarkerPanic {
		t.Fatalf("last record = %+v", last)
	}

	run := func(wantCode int, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := exec.Command(cli, args...)
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != wantCode {
			t.Fatalf("kavach %v: exit %d, want %d\n%s", args, code, wantCode, out.String())
		}
		return out.String()
	}
	mustContain := func(out string, subs ...string) {
		t.Helper()
		for _, s := range subs {
			if !strings.Contains(out, s) {
				t.Fatalf("output lacks %q:\n%s", s, out)
			}
		}
	}

	mustContain(run(0, "inspect", paths[0]), "service ledger", "marker    panic")
	mustContain(run(0, "inspect", "--json", paths[0]), `"type": "marker"`)
	mustContain(run(1, "replay", paths[0], "--bin", oldBin), "still_failing@32")
	mustContain(run(0, "replay", "--json", paths[0], "--bin", newBin), `"verdict": "fixed"`)
	mustContain(run(0, "diff", paths[0], "--old", oldBin, "--new", newBin),
		"old       still_failing@32", "new       fixed", "first divergence at step seq 32", "missing amount",
		"variants  41 of 44 reproduce the incident on the old build · 41 of those pass", "verdict   fixed")
	mustContain(run(1, "diff", paths[0], "--old", newBin, "--new", oldBin), "verdict   still_failing@32")

	// A fix that passes the recorded incident but not its variants is rejected,
	// and the failing variant is saved for replay.
	keep := filepath.Join(dir, "variants")
	out := run(1, "diff", paths[0], "--old", oldBin, "--new", partialBin, "--keep", keep)
	mustContain(out, "new       fixed", `"type" "deposit" → "transfer"`, "verdict   variant_failed(6)@23")
	saved, _ := filepath.Glob(filepath.Join(keep, "*.kavach"))
	if len(saved) == 0 {
		t.Fatalf("no failing variants saved:\n%s", out)
	}
	mustContain(run(1, "replay", saved[0], "--bin", partialBin), "still_failing@23")
	mustContain(run(0, "diff", paths[0], "--old", oldBin, "--new", partialBin, "--variants", "0"), "verdict   fixed")
	mustContain(run(0, "diff", "--json", paths[0], "--old", oldBin, "--new", newBin), `"verdict": "fixed"`, `"reproducing": 41`)

	// The same loop through the MCP server, as an agent drives it.
	mcp := exec.Command(cli, "mcp")
	mcp.Stdin = strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		mcpCall(2, "kavach_list_incidents", map[string]any{"dir": fx}),
		mcpCall(3, "kavach_replay", map[string]any{"fixture": paths[0], "bin": oldBin}),
		mcpCall(4, "kavach_diff", map[string]any{"fixture": paths[0], "old": oldBin, "new": partialBin}),
		mcpCall(5, "kavach_diff", map[string]any{"fixture": paths[0], "old": oldBin, "new": newBin}),
	}, "\n"))
	mcpOut, err := mcp.Output()
	if err != nil {
		t.Fatalf("kavach mcp: %v", err)
	}
	var verdicts []string
	dec := json.NewDecoder(bytes.NewReader(mcpOut))
	for dec.More() {
		var resp struct {
			Result struct {
				Structured map[string]any `json:"structuredContent"`
				IsError    bool           `json:"isError"`
			}
		}
		if err := dec.Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Result.IsError {
			t.Fatalf("tool error in:\n%s", mcpOut)
		}
		if v, ok := resp.Result.Structured["verdict"].(string); ok {
			verdicts = append(verdicts, v)
		}
	}
	if got := strings.Join(verdicts, " "); got != "still_failing@32 variant_failed(6)@23 fixed" {
		t.Fatalf("MCP verdicts %q in:\n%s", got, mcpOut)
	}
	mustContain(string(mcpOut), `\"seq\": 32`, `"passed":false`, `"passed":true`)

	// Usage and environment errors.
	run(2, "replay")
	run(2, "diff", paths[0], "--old", oldBin)
	run(2, "frobnicate")
	mustContain(run(3, "replay", paths[0], "--bin", cli), "kavach.MaybeReplay")
	os.WriteFile(filepath.Join(dir, "junk.kavach"), []byte("this is a plain text file, not a journal"), 0o644)
	mustContain(run(3, "inspect", filepath.Join(dir, "junk.kavach")), "bad magic")
	mustContain(run(0, "version"), "journal format 0.2")
}

func mcpCall(id int, tool string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	return string(b)
}

// TestDeterminism replays the demo crash fixture 1,000 times and requires
// byte-identical results (BENCHMARKS.md).
func TestDeterminism(t *testing.T) {
	j, err := journal.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i := 0; i < 1000; i++ {
		res, err := kavach.Replay(j, newHandler)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Fatalf("replay %d differs from the first", i)
		}
	}
}

// BenchmarkReplayFixture measures in-process replay of the demo crash fixture,
// including reading and decoding the file.
func BenchmarkReplayFixture(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := kavach.ReplayFile(fixture, newHandler); err != nil {
			b.Fatal(err)
		}
	}
}
