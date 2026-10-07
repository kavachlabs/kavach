package bench_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/bench"
	"github.com/kavachlabs/kavach/journal"
)

// TestEndToEnd runs the whole agent loop for every benchmark bug through real
// binaries: the buggy service records a fixture, the kavach CLI inspects and
// replays it, and `kavach diff` accepts the correct fix and rejects the narrow
// one, with the failing variant saved and reproducible. The same verdicts are
// then checked over `kavach mcp`.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 31 binaries")
	}
	dir := t.TempDir()
	build := func(t *testing.T, out string, args ...string) string {
		t.Helper()
		cmd := exec.Command("go", append([]string{"build", "-o", out}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", args, err, b)
		}
		return out
	}
	cli := build(t, filepath.Join(dir, "kavach"), "github.com/kavachlabs/kavach/cmd/kavach")
	svc := func(t *testing.T, bug, mode string) string {
		return build(t, filepath.Join(dir, "svc-"+bug+"-"+mode), "-ldflags", "-X main.bug="+bug+" -X main.mode="+mode,
			"github.com/kavachlabs/kavach/bench/cmd/benchsvc")
	}
	run := func(t *testing.T, wantCode int, name string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = &out, &out
		code := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		if code != wantCode {
			t.Fatalf("%s %v: exit %d, want %d\n%s", filepath.Base(name), args, code, wantCode, out.String())
		}
		return out.String()
	}
	has := func(t *testing.T, out string, subs ...string) {
		t.Helper()
		for _, s := range subs {
			if !strings.Contains(out, s) {
				t.Fatalf("output lacks %q:\n%s", s, out)
			}
		}
	}

	type outcome struct{ fixture, buggy, fixed, narrow string }
	bugs := bench.Bugs()
	all := make([]outcome, len(bugs))
	// Parallel subtests finish before the enclosing t.Run returns.
	t.Run("bugs", func(t *testing.T) {
		for i, b := range bugs {
			t.Run(b.Name, func(t *testing.T) {
				t.Parallel()
				buggy, fixed, narrow := svc(t, b.Name, "buggy"), svc(t, b.Name, "fixed"), svc(t, b.Name, "narrow")

				// The buggy service records its own incident.
				fx := strings.TrimSpace(run(t, 0, buggy, "record", filepath.Join(dir, "fx-"+b.Name)))
				j, err := journal.ReadFile(fx)
				if err != nil {
					t.Fatal(err)
				}
				last := j.Records[len(j.Records)-1]
				wantMarker := map[string]string{"panic": journal.MarkerPanic, "error": journal.MarkerError, "invariant": journal.MarkerInvariant}[b.Class]
				if last.Kind != wantMarker {
					t.Fatalf("last record kind %q, want %q", last.Kind, wantMarker)
				}
				has(t, run(t, 0, cli, "inspect", fx), "service bench-"+b.Name, "marker    "+wantMarker)
				if j.Header.Meta.Scrub == nil {
					t.Fatal("recorded fixture is not scrubbed by default")
				}

				// Reproduce, then verify the correct fix and the narrow one.
				wantOld := "still_failing@"
				if b.Class == "invariant" {
					wantOld = "invariant_violated("
				}
				var replay struct{ Verdict string }
				if err := json.Unmarshal([]byte(run(t, 1, cli, "replay", "--json", fx, "--bin", buggy)), &replay); err != nil {
					t.Fatal(err)
				}
				has(t, replay.Verdict, wantOld)
				run(t, 0, cli, "replay", fx, "--bin", fixed)

				var d struct {
					Verdict  string
					Variants struct{ Reproducing, Passing int }
				}
				if err := json.Unmarshal([]byte(run(t, 0, cli, "diff", "--json", fx, "--old", buggy, "--new", fixed)), &d); err != nil {
					t.Fatal(err)
				}
				if d.Verdict != "fixed" || d.Variants.Reproducing < 10 || d.Variants.Passing != d.Variants.Reproducing {
					t.Fatalf("correct fix: %+v", d)
				}

				keep := filepath.Join(dir, "kept-"+b.Name)
				has(t, run(t, 1, cli, "diff", fx, "--old", buggy, "--new", narrow, "--keep", keep), "verdict   variant_failed(")
				saved, _ := filepath.Glob(filepath.Join(keep, "*.kavach"))
				if len(saved) == 0 {
					t.Fatal("no failing variant saved")
				}
				// The saved variant still fails the narrow build and passes the correct one.
				run(t, 1, cli, "replay", saved[0], "--bin", narrow)
				run(t, 0, cli, "replay", saved[0], "--bin", fixed)
				// A fix is not a fix for a binary run against itself.
				run(t, 1, cli, "diff", fx, "--old", fixed, "--new", buggy)
				all[i] = outcome{fx, buggy, fixed, narrow}
			})
		}
	})

	// An unscrubbed fixture is scrubbed by the CLI, which checks that the
	// verdict survives and never touches the original.
	t.Run("scrub", func(t *testing.T) {
		t.Setenv("BENCH_NOSCRUB", "1")
		buggy := svc(t, "nil-email", "buggy")
		fx := strings.TrimSpace(run(t, 0, buggy, "record", filepath.Join(dir, "raw")))
		before, _ := os.ReadFile(fx)
		if !bytes.Contains(before, []byte("ann@example.com")) {
			t.Fatal("the NoScrub fixture should hold the raw email")
		}
		out := filepath.Join(dir, "clean.kavach")
		has(t, run(t, 0, cli, "scrub", fx, "-o", out, "--bin", buggy), "redacted", "still_failing@", "wrote")
		after, _ := os.ReadFile(fx)
		clean, _ := os.ReadFile(out)
		if !bytes.Equal(before, after) {
			t.Error("the original fixture was modified")
		}
		if bytes.Contains(clean, []byte("example.com")) && bytes.Contains(clean, []byte("ann@")) {
			t.Error("scrubbed fixture still holds a raw email")
		}
		has(t, run(t, 1, cli, "replay", out, "--bin", buggy), "still_failing@")
		has(t, run(t, 0, cli, "scrub", out), "already scrubbed")
		run(t, 2, cli, "scrub", fx, "-o", fx)
	})

	t.Run("mcp", func(t *testing.T) {
		for _, o := range all {
			if o.fixture == "" || o.narrow == "" {
				t.Skip("a bug's subtest failed before finishing")
			}
		}
		var msgs []string
		msgs = append(msgs,
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		for i, o := range all {
			for k, newBin := range []string{o.fixed, o.narrow} {
				args, _ := json.Marshal(map[string]any{"fixture": o.fixture, "old": o.buggy, "new": newBin})
				msgs = append(msgs, `{"jsonrpc":"2.0","id":`+itoa(100+2*i+k)+`,"method":"tools/call","params":{"name":"kavach_diff","arguments":`+string(args)+`}}`)
			}
		}
		cmd := exec.Command(cli, "mcp")
		cmd.Stdin = strings.NewReader(strings.Join(msgs, "\n"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("kavach mcp: %v", err)
		}
		got := map[int]string{}
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var r struct {
				ID     int
				Result struct {
					Structured struct{ Verdict string } `json:"structuredContent"`
					IsError    bool
				}
			}
			if err := dec.Decode(&r); err != nil {
				t.Fatal(err)
			}
			if r.Result.IsError {
				t.Fatalf("tool error: %s", out)
			}
			got[r.ID] = r.Result.Structured.Verdict
		}
		for i, b := range bugs {
			if v := got[100+2*i]; v != "fixed" {
				t.Errorf("%s: correct fix over MCP: %q", b.Name, v)
			}
			if v := got[100+2*i+1]; !strings.HasPrefix(v, "variant_failed(") {
				t.Errorf("%s: narrow fix over MCP: %q", b.Name, v)
			}
		}
	})
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
