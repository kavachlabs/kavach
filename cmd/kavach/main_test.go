package main

import (
	"bytes"
	"strings"
	"testing"
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
	if !strings.Contains(out, "    31  input     file:events.jsonl @ 8") {
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
	if code, _, errs := runCLI(t, "replay", fixture, "--bin", ""); code != exitError || !strings.Contains(errs, "KAVACH_BIN") {
		t.Fatalf("missing bin: %d %s", code, errs)
	}
}
