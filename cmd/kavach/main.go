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

  kavach inspect <fixture> [--json] [--full]
  kavach replay  <fixture> --bin <replay-binary> [--json]
  kavach diff    <fixture> --old <binary> --new <binary> [--json]
  kavach version

A replay binary is any Go binary whose main calls kavach.MaybeReplay. --bin
defaults to $KAVACH_BIN.

On a terminal, output is colored and starts with a banner; --no-banner or
KAVACH_NO_BANNER=1 drops the banner, NO_COLOR=1 drops all styling. Piped output
and --json are never styled.

Exit status: 0 when the replay (of the new binary, for diff) is ok or fixed,
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
		if f := res.Recorded; f != nil {
			fmt.Fprintf(stdout, "%s%s\n", u.label("recorded", 10), u.paint(fmt.Sprintf("%s at seq %d: %s", f.Kind, f.Seq, f.Message), markerColor(f.Kind)))
		} else {
			fmt.Fprintf(stdout, "%s%s\n", u.label("recorded", 10), "no failure")
		}
		fmt.Fprintf(stdout, "%s%s\n", u.label("result", 10), u.verdict(res))
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
	asJSON := fs.Bool("json", false, "print the comparison as JSON")
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
	oldRes, _, err := replayWith(*oldBin, path)
	if err != nil {
		fmt.Fprintf(stderr, "kavach diff: old: %v\n", err)
		return exitError
	}
	newRes, _, err := replayWith(*newBin, path)
	if err != nil {
		fmt.Fprintf(stderr, "kavach diff: new: %v\n", err)
		return exitError
	}
	d := kavach.Compare(oldRes, newRes)
	if *asJSON {
		if writeJSON(stdout, stderr, map[string]any{
			"fixture": path, "old": oldRes.String(), "new": newRes.String(),
			"new_detail": newRes.Detail, "divergence": d,
		}) != exitPass {
			return exitError
		}
	} else {
		u.printBanner(stdout)
		u.header(stdout, "KAVACH FIX VERIFICATION")
		fmt.Fprintf(stdout, "%s%s\n", u.label("fixture", 9), path)
		fmt.Fprintf(stdout, "%s%s\n", u.label("old", 9), u.verdict(oldRes))
		fmt.Fprintf(stdout, "%s%s\n", u.label("new", 9), u.verdict(newRes))
		if newRes.Detail != "" && !newRes.Passed() {
			fmt.Fprintf(stdout, "         %s\n", u.paint(newRes.Detail, verdictColor(newRes.Status)))
		}
		if d == nil {
			fmt.Fprintf(stdout, "%sidentical at every step\n", u.label("outputs", 9))
		} else {
			fmt.Fprintf(stdout, "%s\n", u.paint(fmt.Sprintf("first divergence at step seq %d: %s", d.Seq, d.Detail), ansiBold, ansiYellow))
			if d.Output >= 0 {
				fmt.Fprintf(stdout, "  %s  %s\n", u.paint("old", ansiBold, ansiRed), u.paint(showOutput(d.Old), ansiRed))
				fmt.Fprintf(stdout, "  %s  %s\n", u.paint("new", ansiBold, ansiGreen), u.paint(showOutput(d.New), ansiGreen))
			}
		}
	}
	if newRes.Passed() {
		return exitPass
	}
	return exitFail
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
