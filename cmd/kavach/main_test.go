package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/journal"
)

const fixture = "../../examples/ledger/testdata/null-amount.kavach"

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestPlainWhenNotATerminal(t *testing.T) {
	t.Setenv("FORCE_COLOR", "")
	code, out, _ := runCLI(t, "inspect", fixture)
	if code != exitPass || strings.Contains(out, "\033[") || strings.Contains(out, "⣠") {
		t.Fatalf("exit %d, styled output when piped:\n%s", code, out)
	}
	if !strings.Contains(out, "    32  input     file:events.jsonl @ 8") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestForceColor(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	_, out, _ := runCLI(t, "inspect", fixture)
	for _, want := range []string{"⣠⣴⠶⠾⠷⠶⣦⡄", "=== [KAVACH INSPECT] ===", ansiRed + "panic"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	_, out, _ = runCLI(t, "--no-banner", "inspect", fixture)
	if strings.Contains(out, "⣠") || !strings.Contains(out, "\033[") {
		t.Fatalf("--no-banner should keep color but drop the banner:\n%s", out)
	}

	t.Setenv("KAVACH_NO_BANNER", "1")
	if _, out, _ = runCLI(t, "version"); strings.Contains(out, "⣠") {
		t.Fatalf("KAVACH_NO_BANNER ignored:\n%s", out)
	}
}

func TestNoColorWins(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("NO_COLOR", "1")
	if _, out, _ := runCLI(t, "inspect", fixture); strings.Contains(out, "\033[") || strings.Contains(out, "⣠") {
		t.Fatalf("NO_COLOR ignored:\n%s", out)
	}
}

func TestJSONIsNeverStyled(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	_, out, _ := runCLI(t, "inspect", "--json", fixture)
	if strings.Contains(out, "\033[") || !strings.HasPrefix(out, "{") {
		t.Fatalf("styled JSON:\n%.200s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"inspect"}, {"inspect", "a", "b"}, {"replay", "--bogus"}, {"diff", fixture}} {
		if code, _, _ := runCLI(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
	if code, out, _ := runCLI(t, "help"); code != exitPass || !strings.Contains(out, "kavach inspect") {
		t.Fatalf("help: %d\n%s", code, out)
	}
	for _, flag := range []string{"version", "--version", "-version", "-v"} {
		if code, out, _ := runCLI(t, flag); code != exitPass || !strings.Contains(out, "kavach ") {
			t.Fatalf("%s: %d\n%s", flag, code, out)
		}
	}
	if code, _, errs := runCLI(t, "replay", fixture, "--bin", ""); code != exitError || !strings.Contains(errs, "KAVACH_BIN") {
		t.Fatalf("missing bin: %d %s", code, errs)
	}
}

func TestReplayUntilAndProbesCLI(t *testing.T) {
	host, work := storeContract(t)
	traffic := work + "/traffic.kavach"

	// Replay up to seq 3 (after SET greeting=hello at seq 1 and SET n=41 at seq 3),
	// probe GET greeting via --probe and GET n via --hold stdin.
	var out, errb bytes.Buffer
	u := newUI(&out, true)
	stdin := strings.NewReader("{\"argv\":[\"GET\",\"n\"]}\n")
	code := cmdReplay(u, []string{
		traffic, "--bin", host + " base", "--until", "3",
		"--probe", `{"argv":["GET","greeting"]}`, "--hold",
	}, stdin, &out, &errb)
	if code != exitPass {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"2 steps replayed",
		"until     seq 3 (stopped at step seq 3)",
		"result    ok",
		`probe 1   {"argv":["GET","greeting"]} -> ok`,
		"reply  hello",
		`probe 2   {"argv":["GET","n"]} -> ok`,
		"reply  41",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestDiffJSONIncludesKindAndReproducingByKind(t *testing.T) {
	host, work := storeContract(t)
	setIn, _ := json.Marshal(map[string]any{"argv": []string{"SET", "greeting", "hello"}})
	incrIn, _ := json.Marshal(map[string]any{"argv": []string{"INCR", "greeting"}})
	recs := []journal.Record{
		{Seq: 0, Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Facts: []journal.Fact{}},
		{Seq: 1, Type: journal.TypeInput, Source: "test", Data: setIn},
		{Seq: 2, Type: journal.TypeOutput, Sink: "reply", Data: []byte("OK")},
		{Seq: 3, Type: journal.TypeInput, Source: "test", Data: incrIn},
		{Seq: 4, Type: journal.TypeMarker, Kind: journal.MarkerInvariant, Message: "counter-holds-integer"},
	}
	incPath := filepath.Join(work, "incident.kavach")
	meta := journal.Meta{Service: "store", Compression: journal.CompressionNone, Start: journal.StartGenesis}
	if err := journal.WriteFile(incPath, meta, recs); err != nil {
		t.Fatal(err)
	}

	code, out, errs := runCLI(t, "diff", "--json", "--variants", "3", "--regressions", "0", incPath, "--old", host+" sloppy", "--new", host+" good")
	if code != exitPass {
		t.Fatalf("diff --json exit %d:\n%s%s", code, out, errs)
	}
	var report struct {
		Verdict  string `json:"verdict"`
		Variants struct {
			Reproducing       int            `json:"reproducing"`
			ReproducingByKind map[string]int `json:"reproducing_by_kind"`
			Checks            []struct {
				ID   int    `json:"id"`
				Kind string `json:"kind"`
			} `json:"checks"`
		} `json:"variants"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal diff json: %v\n%s", err, out)
	}
	if report.Verdict != "fixed" {
		t.Fatalf("verdict = %q, want fixed", report.Verdict)
	}
	if len(report.Variants.ReproducingByKind) == 0 {
		t.Fatalf("missing variants.reproducing_by_kind in diff JSON: %s", out)
	}
	sum := 0
	for _, n := range report.Variants.ReproducingByKind {
		sum += n
	}
	if sum != report.Variants.Reproducing {
		t.Fatalf("reproducing_by_kind sum %d != reproducing %d", sum, report.Variants.Reproducing)
	}
	if len(report.Variants.Checks) == 0 {
		t.Fatal("expected variant checks in diff JSON")
	}
	for _, c := range report.Variants.Checks {
		if c.Kind == "" {
			t.Fatalf("variant check %d missing kind in diff JSON: %s", c.ID, out)
		}
	}
}
