// Command kavach inspects, replays and compares Kavach fixtures.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/journal"
)

// Exit codes.
const (
	exitPass  = 0 // replay ok or fixed
	exitFail  = 1 // replay ran but did not pass
	exitUsage = 2
	exitError = 3 // the fixture or binary could not be used
)

const usage = `kavach inspects, replays and compares Kavach fixtures.

Usage:
  kavach [--no-banner] <command> ...

  kavach list    [dir] [--json]
  kavach inspect <fixture> [--json] [--full]
  kavach replay  <fixture> --bin <replay-binary> [--json]
  kavach diff    <fixture> --old <binary> --new <binary> [--variants N] [--keep DIR] [--json]
  kavach mcp
  kavach version

list finds fixtures (*.kavach) under dir (default: the current directory).
mcp serves kavach_list_incidents, kavach_replay and kavach_diff to an AI agent
as a Model Context Protocol server on stdin/stdout.

A replay binary is any Go binary whose main calls kavach.MaybeReplay. --bin
defaults to $KAVACH_BIN.

diff accepts a fix only if the new binary replays the fixture as fixed and also
passes at least N (default 10) variants of the incident: the failing input with
fields changed, moved earlier, earlier inputs dropped or redelivered, the clock
shifted. A variant counts only if the old binary fails on it exactly as it did
in production. Variants the new binary fails are saved for kavach replay.

On a terminal, output is colored and starts with a banner; --no-banner or
KAVACH_NO_BANNER=1 drops the banner, NO_COLOR=1 drops all styling. Piped output
and --json are never styled.

Exit status: 0 when the replay is ok or fixed (for diff: the verdict is fixed),
1 when it is not, 2 for usage errors, 3 when a fixture or binary is unusable.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	noBanner := false
	rest := args[:0:0]
	for _, a := range args {
		if a == "--no-banner" || a == "-no-banner" {
			noBanner = true
			continue
		}
		rest = append(rest, a)
	}
	args = rest
	u := newUI(stdout, noBanner)
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "inspect":
		return cmdInspect(u, args, stdout, stderr)
	case "replay":
		return cmdReplay(u, args, stdout, stderr)
	case "diff":
		return cmdDiff(u, args, stdout, stderr)
	case "list":
		return cmdList(u, args, stdout, stderr)
	case "mcp":
		return cmdMCP(args, os.Stdin, stdout, stderr)
	case "version":
		u.printBanner(stdout)
		fmt.Fprintf(stdout, "kavach %s (journal format %d.%d)\n", kavach.Version, journal.Major, journal.Minor)
		return exitPass
	case "help", "-h", "--help":
		u.printBanner(stdout)
		fmt.Fprint(stdout, usage)
		return exitPass
	}
	fmt.Fprintf(stderr, "kavach: unknown command %q\n\n%s", cmd, usage)
	return exitUsage
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("kavach "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func oneFixture(name string, pos []string, stderr io.Writer) (string, bool) {
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "kavach %s: expected exactly one fixture, got %d arguments\n", name, len(pos))
		return "", false
	}
	return pos[0], true
}

func cmdInspect(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("inspect", stderr)
	asJSON := fs.Bool("json", false, "print the decoded journal as JSON")
	full := fs.Bool("full", false, "do not truncate data")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	path, ok := oneFixture("inspect", pos, stderr)
	if !ok {
		return exitUsage
	}
	j, err := journal.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "kavach inspect: %v\n", err)
		return exitError
	}
	if *asJSON {
		return writeJSON(stdout, stderr, map[string]any{
			"major": j.Header.Major, "minor": j.Header.Minor, "meta": j.Header.Meta,
			"records": j.Records, "truncated": j.Truncated,
		})
	}

	m := j.Header.Meta
	u.printBanner(stdout)
	u.header(stdout, "KAVACH INSPECT")
	fmt.Fprintf(stdout, "%s  %s\n", u.paint(path, ansiBold), u.paint(fmt.Sprintf("format %d.%d", j.Header.Major, j.Header.Minor), ansiDim))
	fmt.Fprintf(stdout, "%s %s · start %s · %d records\n", u.paint("service", ansiBold), m.Service, m.Start, len(j.Records))
	if m.Handler != "" || m.Producer != "" || m.RecordedAt != "" {
		fmt.Fprintln(stdout, u.paint(fmt.Sprintf("handler %s · producer %s · recorded %s", orDash(m.Handler), orDash(m.Producer), orDash(m.RecordedAt)), ansiDim))
	}
	printEnv(u, stdout, m.Env)
	if v := m.Variant; v != nil {
		fmt.Fprintln(stdout, u.paint(fmt.Sprintf("variant %d: %s · expected failure at seq %d: %s", v.ID, v.Mutation, v.Incident, v.Failure), ansiYellow))
	}
	if j.Truncated {
		fmt.Fprintln(stdout, u.paint("warning: file ends inside a record; showing complete records only", ansiYellow))
	}
	fmt.Fprintln(stdout)
	limit := 96
	if *full {
		limit = -1
	}
	for _, r := range j.Records {
		ty := fmt.Sprintf("%-8s", r.Type)
		desc := describeRecord(r, limit)
		if r.Type == journal.TypeMarker {
			desc = u.paint(desc, markerColor(r.Kind))
		}
		fmt.Fprintf(stdout, "%s  %s  %s\n", u.paint(fmt.Sprintf("%6d", r.Seq), ansiDim), u.paint(ty, ansiBold, typeColor[r.Type.String()]), desc)
	}
	return exitPass
}

func describeRecord(r journal.Record, limit int) string {
	switch r.Type {
	case journal.TypeInput:
		return fmt.Sprintf("%s @ %s  %s", r.Source, orDash(r.Position), showData(r.Data, limit))
	case journal.TypeClock:
		return time.Unix(0, r.UnixNanos).UTC().Format(time.RFC3339Nano)
	case journal.TypeRand:
		return fmt.Sprintf("%d bytes  %x", len(r.Data), r.Data)
	case journal.TypeOutput:
		return fmt.Sprintf("%s  %s", r.Sink, showData(r.Data, limit))
	case journal.TypeMarker:
		s := fmt.Sprintf("%s  %s", r.Kind, r.Message)
		if len(r.Data) > 0 && limit < 0 {
			s += "\n" + indent(string(r.Data))
		} else if len(r.Data) > 0 {
			s += fmt.Sprintf("  (+%d bytes detail, --full to show)", len(r.Data))
		}
		return s
	case journal.TypeSnapshot:
		return fmt.Sprintf("%d bytes  %s", len(r.Data), showData(r.Data, limit))
	}
	return fmt.Sprintf("%d bytes", len(r.Data))
}

func showData(b []byte, limit int) string {
	if len(b) == 0 {
		return "(empty)"
	}
	trunc := limit >= 0 && len(b) > limit
	if trunc {
		b = b[:limit]
	}
	var s string
	if utf8.Valid(b) && !bytes.ContainsAny(b, "\x00") {
		s = string(b)
	} else {
		s = fmt.Sprintf("%x", b)
	}
	if trunc {
		s += "…"
	}
	return s
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "          " + l
	}
	return strings.Join(lines, "\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	stdout.Write(append(b, '\n'))
	return exitPass
}

// replayWith runs bin as a replay binary on fixture and returns its result.
func replayWith(bin, fixture string) (*kavach.Result, time.Duration, error) {
	if bin == "" {
		return nil, 0, errors.New("no replay binary: pass --bin or set KAVACH_BIN")
	}
	if !strings.ContainsRune(bin, filepath.Separator) {
		if p, err := exec.LookPath(bin); err == nil {
			bin = p
		}
	}
	dir, err := os.MkdirTemp("", "kavach-replay-")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "result.json")

	var stderr bytes.Buffer
	cmd := exec.Command(bin, kavach.ReplayCommand, fixture, out)
	cmd.Stderr = &stderr
	if j, err := journal.ReadFile(fixture); err == nil {
		cmd.Env = kavach.ReplayEnv(os.Environ(), j.Header.Meta.Env)
	}
	start := time.Now()
	err = cmd.Run()
	wall := time.Since(start)
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, wall, fmt.Errorf("%s: %s", bin, msg)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, wall, fmt.Errorf("%s did not write a replay result; does its main call kavach.MaybeReplay?", bin)
	}
	var res kavach.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, wall, fmt.Errorf("%s wrote an invalid replay result: %v", bin, err)
	}
	return &res, wall, nil
}

func cmdReplay(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("replay", stderr)
	bin := fs.String("bin", os.Getenv("KAVACH_BIN"), "replay binary (a Go binary calling kavach.MaybeReplay)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	path, ok := oneFixture("replay", pos, stderr)
	if !ok {
		return exitUsage
	}
	res, wall, err := replayWith(*bin, path)
	if err != nil {
		fmt.Fprintf(stderr, "kavach replay: %v\n", err)
		return exitError
	}
	if *asJSON {
		if writeJSON(stdout, stderr, map[string]any{
			"fixture": path, "verdict": res.String(), "wall_ms": ms(wall), "result": res,
		}) != exitPass {
			return exitError
		}
	} else {
		u.printBanner(stdout)
		u.header(stdout, "KAVACH DETERMINISTIC REPLAY")
		fmt.Fprintf(stdout, "%s%s\n", u.label("fixture", 10), path)
		fmt.Fprintf(stdout, "%s%s · start %s · %d records · %d steps replayed\n", u.label("service", 10), res.Service, res.Start, res.Records, len(res.Steps))
		if res.Variant != "" {
			fmt.Fprintf(stdout, "%s%s\n", u.label("variant", 10), u.paint(res.Variant, ansiYellow))
		} else if f := res.Recorded; f != nil {
			fmt.Fprintf(stdout, "%s%s\n", u.label("recorded", 10), u.paint(fmt.Sprintf("%s at seq %d: %s", f.Kind, f.Seq, f.Message), markerColor(f.Kind)))
		} else {
			fmt.Fprintf(stdout, "%s%s\n", u.label("recorded", 10), "no failure")
		}
		fmt.Fprintf(stdout, "%s%s\n", u.label("result", 10), u.verdict(res.Status, res.String()))
		if res.Detail != "" {
			fmt.Fprintf(stdout, "          %s\n", u.paint(res.Detail, verdictColor(res.Status)))
		}
		if n := synthesized(res); n > 0 {
			fmt.Fprintf(stdout, "%s%d clock/random reads past the recorded failure were synthesized\n", u.label("note", 10), n)
		}
		fmt.Fprintf(stdout, "%s%s\n", u.label("wall", 10), u.paint(fmt.Sprintf("%.1f ms (including process start)", ms(wall)), ansiYellow))
		if u.color && res.Status == kavach.StatusStillFailing {
			fmt.Fprintf(stdout, "\n%s\n", u.paint(fmt.Sprintf("next: fix the handler, build it, then run kavach diff %s --old %s --new <new-binary>", path, *bin), ansiDim))
		}
	}
	if res.Passed() {
		return exitPass
	}
	return exitFail
}

func synthesized(res *kavach.Result) int {
	n := 0
	for _, s := range res.Steps {
		n += s.Synthesized
	}
	return n
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func cmdDiff(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("diff", stderr)
	oldBin := fs.String("old", "", "replay binary built from the code that failed")
	newBin := fs.String("new", "", "replay binary built from the candidate fix")
	minVariants := fs.Int("variants", kavach.DefaultMinVariants, "variants of the incident the fix must also pass; 0 checks only the recorded journal")
	keep := fs.String("keep", "", "directory to save failing variants in (default: a new temporary directory)")
	asJSON := fs.Bool("json", false, "print the verification as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	path, ok := oneFixture("diff", pos, stderr)
	if !ok {
		return exitUsage
	}
	if *oldBin == "" || *newBin == "" {
		fmt.Fprintln(stderr, "kavach diff: both --old and --new are required")
		return exitUsage
	}
	if *minVariants < 0 {
		fmt.Fprintln(stderr, "kavach diff: --variants must not be negative")
		return exitUsage
	}
	o, err := verifyFix(path, *oldBin, *newBin, *minVariants, *keep)
	if err != nil {
		fmt.Fprintf(stderr, "kavach diff: %v\n", err)
		return exitError
	}
	v, saved, d, wall := o.v, o.saved, o.divergence, o.wall

	if *asJSON {
		if writeJSON(stdout, stderr, o.report()) != exitPass {
			return exitError
		}
	} else {
		u.printBanner(stdout)
		u.header(stdout, "KAVACH FIX VERIFICATION")
		fmt.Fprintf(stdout, "%s%s\n", u.label("fixture", 10), path)
		fmt.Fprintf(stdout, "%s%s\n", u.label("old", 10), u.verdict(v.Old.Status, v.Old.String()))
		fmt.Fprintf(stdout, "%s%s\n", u.label("new", 10), u.verdict(v.New.Status, v.New.String()))
		if v.New.Detail != "" && !v.New.Passed() {
			fmt.Fprintf(stdout, "          %s\n", u.paint(v.New.Detail, verdictColor(v.New.Status)))
		}
		if d == nil {
			fmt.Fprintf(stdout, "%sidentical at every step\n", u.label("outputs", 10))
		} else {
			fmt.Fprintf(stdout, "%s\n", u.paint(fmt.Sprintf("first divergence at step seq %d: %s", d.Seq, d.Detail), ansiBold, ansiYellow))
			if d.Output >= 0 {
				fmt.Fprintf(stdout, "  %s  %s\n", u.paint("old", ansiBold, ansiRed), u.paint(showOutput(d.Old), ansiRed))
				fmt.Fprintf(stdout, "  %s  %s\n", u.paint("new", ansiBold, ansiGreen), u.paint(showOutput(d.New), ansiGreen))
			}
		}
		if v.Candidates > 0 {
			fmt.Fprintf(stdout, "%s%d of %d reproduce the incident on the old build · %d of those pass on the new build\n",
				u.label("variants", 10), v.Reproducing, v.Candidates, v.Passing)
			shown := 0
			for _, c := range v.Variants {
				if !c.Reproduces || c.Passed {
					continue
				}
				if shown == 5 {
					fmt.Fprintf(stdout, "          %s\n", u.paint(fmt.Sprintf("… and %d more (--json lists them all)", v.Reproducing-v.Passing-shown), ansiDim))
					break
				}
				shown++
				fmt.Fprintf(stdout, "  %s %s\n", u.paint(fmt.Sprintf("✗ %2d", c.ID), ansiBold, ansiRed), c.Mutation)
				fmt.Fprintf(stdout, "       %s\n", u.paint(c.Detail, ansiRed))
				if f := saved[c.ID]; f != "" {
					fmt.Fprintf(stdout, "       %s\n", u.paint("saved "+f, ansiDim))
				}
			}
		}
		fmt.Fprintf(stdout, "%s%s\n", u.label("verdict", 10), u.verdict(v.Status, v.String()))
		if v.Status == kavach.StatusVariantFailed || v.Status == kavach.StatusUnverified {
			fmt.Fprintf(stdout, "          %s\n", u.paint(v.Detail, verdictColor(v.Status)))
		}
		fmt.Fprintf(stdout, "%s%s\n", u.label("wall", 10), u.paint(fmt.Sprintf("%.1f ms for %d replays", ms(wall), replays(v)), ansiYellow))
		if u.color && v.Status == kavach.StatusVariantFailed {
			if f := saved[v.Variant]; f != "" {
				fmt.Fprintf(stdout, "\n%s\n", u.paint(fmt.Sprintf("next: kavach replay %s --bin %s", f, *newBin), ansiDim))
			}
		}
	}
	if v.Passed() {
		return exitPass
	}
	return exitFail
}

// diffOutcome is a verified fix, as kavach diff and the MCP server report it.
type diffOutcome struct {
	fixture    string
	v          *kavach.Verification
	saved      map[int]string // failing variants written to disk, by ID
	divergence *kavach.Divergence
	wall       time.Duration
}

// verifyFix checks the fix in newBin against the incident in fixture.
// minVariants 0 checks only the recorded journal.
func verifyFix(fixture, oldBin, newBin string, minVariants int, keep string) (*diffOutcome, error) {
	j, err := journal.ReadFile(fixture)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "kavach-diff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	opts := kavach.VerifyOptions{MinVariants: minVariants, Parallel: runtime.NumCPU()}
	if minVariants == 0 {
		opts.MinVariants = -1
	}
	start := time.Now()
	v, err := kavach.VerifyWith(j, binReplayer(oldBin, fixture, work), binReplayer(newBin, fixture, work), opts)
	wall := time.Since(start)
	if err != nil {
		return nil, err
	}
	saved, err := saveFailingVariants(v, fixture, keep)
	if err != nil {
		return nil, fmt.Errorf("saving failing variants: %w", err)
	}
	return &diffOutcome{fixture, v, saved, kavach.Compare(v.Old, v.New), wall}, nil
}

// report is the JSON form of a diff outcome.
func (o *diffOutcome) report() map[string]any {
	v := o.v
	checks := make([]map[string]any, len(v.Variants))
	for i, c := range v.Variants {
		m := map[string]any{"id": c.ID, "mutation": c.Mutation, "incident_seq": c.Incident,
			"reproduces": c.Reproduces, "old": c.Old, "passed": c.Passed}
		if c.Reproduces {
			m["new"] = c.New
		}
		if c.Detail != "" {
			m["detail"] = c.Detail
		}
		if f := o.saved[c.ID]; f != "" {
			m["file"] = f
		}
		checks[i] = m
	}
	return map[string]any{
		"fixture": o.fixture, "verdict": v.String(), "status": v.Status, "detail": v.Detail,
		"old": v.Old.String(), "new": v.New.String(), "new_detail": v.New.Detail, "divergence": o.divergence,
		"variants": map[string]any{
			"required": v.MinVariants, "candidates": v.Candidates, "reproducing": v.Reproducing,
			"passing": v.Passing, "checks": checks,
		},
		"wall_ms": ms(o.wall),
	}
}

// binReplayer replays journals with a replay binary: the fixture at path
// itself, and anything else (variants) after writing it under dir.
func binReplayer(bin, path, dir string) kavach.ReplayFunc {
	return func(j *journal.Journal) (*kavach.Result, error) {
		f := path
		if v := j.Header.Meta.Variant; v != nil {
			f = filepath.Join(dir, fmt.Sprintf("variant-%02d.kavach", v.ID))
			if _, err := os.Stat(f); err != nil {
				if err := journal.WriteFile(f, j.Header.Meta, j.Records); err != nil {
					return nil, err
				}
			}
		}
		res, _, err := replayWith(bin, f)
		return res, err
	}
}

// saveFailingVariants writes the variants the new build failed to dir (a new
// temporary directory if empty) and returns their paths by variant ID.
func saveFailingVariants(v *kavach.Verification, fixture, dir string) (map[int]string, error) {
	saved := map[int]string{}
	for _, c := range v.Variants {
		if !c.Reproduces || c.Passed {
			continue
		}
		if dir == "" {
			var err error
			if dir, err = os.MkdirTemp("", "kavach-variants-"); err != nil {
				return nil, err
			}
		} else if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		base := strings.TrimSuffix(filepath.Base(fixture), ".kavach")
		f := filepath.Join(dir, fmt.Sprintf("%s.variant-%02d.kavach", base, c.ID))
		if err := journal.WriteFile(f, c.Journal.Header.Meta, c.Journal.Records); err != nil {
			return nil, err
		}
		saved[c.ID] = f
	}
	return saved, nil
}

func replays(v *kavach.Verification) int {
	return 2 + v.Candidates + v.Reproducing
}

func showOutput(o *kavach.Output) string {
	if o == nil {
		return "(none)"
	}
	return o.Sink + "  " + showData(o.Data, 160)
}

func markerColor(kind string) string {
	switch kind {
	case journal.MarkerPanic, journal.MarkerError:
		return ansiRed
	case journal.MarkerInvariant:
		return ansiMagenta
	}
	return ansiYellow
}

// printEnv summarises the recorded environment and how the current one differs.
// Values are never printed.
func printEnv(u ui, w io.Writer, e *journal.Env) {
	if e == nil {
		return
	}
	var sys string
	if s := e.System; s != nil {
		sys = fmt.Sprintf("%s/%s · %d cpus · GOMAXPROCS %d · %s", s.OS, s.Arch, s.CPUs, s.GOMAXPROCS, orDash(s.Timezone))
	}
	state := "unscrubbed"
	if e.Scrubbed {
		state = "scrubbed"
	}
	fmt.Fprintln(w, u.paint(fmt.Sprintf("env %d vars (%s) · %s", len(e.Vars), state, sys), ansiDim))
	for _, d := range kavach.EnvDrift(e, kavach.CaptureEnv()) {
		fmt.Fprintln(w, u.paint("drift: "+d, ansiYellow))
	}
}
