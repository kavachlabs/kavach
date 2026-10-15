package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/contract"
	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
)

const contractUsage = `kavach contract checks a build against a behaviour contract: traffic recorded
on the build before a feature, the steps that reach the feature, and the
invariants the feature must keep (SPEC.md section 6.4).

Usage:
  kavach contract init    --spec FILE --traffic FIXTURE [--feature-steps 40-52,60] [--feature-match EXPR]...
                          [--invariant NAME]... [--invariant-source FILE]... [--dir DIR] [--force]
  kavach contract show    [--dir DIR] [--json]
  kavach contract approve [--dir DIR] [--yes]
  kavach contract check   --new <host-command> [--old <host-command>] [--dir DIR] [--no-variants] [--json]

DIR defaults to ./contract. A match expression selects inputs by a value in
their JSON data: '$.argv[0]=="HSETEX"'. A step whose output depends on the
feature, such as a read of a key the feature writes, is a feature step too.

approve locks the contract: it asks for confirmation at a terminal, and
otherwise needs --yes, which the lock records. An agent must not approve its
own contract; it shows it with kavach contract show and asks.

check replays the traffic on the new build. Steps other than feature steps
must produce the recorded outputs; feature steps should change; invariants
must hold, on the traffic and on variants of the feature steps. Verdicts:
pass, regression@N, still_failing@N, invariant_violated(X)@N,
nondeterministic@N, variant_failed(K)@N, unreached@N, missing_invariant(X),
baseline_mismatch(V), unapproved, stale_lock. Exit status 0 only for pass.
Each check is a round in DIR/rounds.jsonl; "streak" counts rounds in a row
with the same outcome. The round's failing variants are saved to DIR/variants/
(replaced by each check) and kept in DIR/rounds/N/: variant-K.kavach and
variant-K.json, the new build's replay of it.
`

func cmdContract(u ui, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, contractUsage)
		return exitUsage
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "init":
		return contractInit(u, args, stdout, stderr)
	case "show":
		return contractShow(u, args, stdout, stderr)
	case "approve":
		return contractApprove(u, args, stdin, stdout, stderr)
	case "check":
		return contractCheck(u, args, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, contractUsage)
		return exitPass
	}
	fmt.Fprintf(stderr, "kavach contract: unknown command %q\n\n%s", sub, contractUsage)
	return exitUsage
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func contractInit(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("contract init", stderr)
	dir := fs.String("dir", contract.DefaultDir, "contract directory")
	spec := fs.String("spec", "", "the specification the contract is written from")
	traffic := fs.String("traffic", "", "journal recorded on the base build")
	steps := fs.String("feature-steps", "", "seqs of feature inputs, e.g. 40-52,60")
	force := fs.Bool("force", false, "replace an existing contract")
	var match, invs, srcs stringList
	fs.Var(&match, "feature-match", "select feature inputs by a JSON value, e.g. '$.argv[0]==\"HSETEX\"' (repeatable)")
	fs.Var(&invs, "invariant", "an invariant the build must declare (repeatable)")
	fs.Var(&srcs, "invariant-source", "a file that declares invariants; the lock covers it (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 0 || *spec == "" || *traffic == "" {
		fmt.Fprintln(stderr, "kavach contract init: --spec and --traffic are required, and nothing else is positional")
		return exitUsage
	}
	seqs, err := contract.ParseSteps(*steps)
	if err != nil {
		fmt.Fprintf(stderr, "kavach contract init: %v\n", err)
		return exitUsage
	}
	d, err := contract.Init(*dir, contract.InitOptions{
		Spec: *spec, Traffic: *traffic, FeatureSteps: seqs, FeatureMatch: match,
		Invariants: invs, InvariantSources: srcs, Force: *force,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kavach contract init: %v\n", err)
		return exitError
	}
	printContract(u, stdout, d)
	fmt.Fprintf(stdout, "\nnext: check the base build with kavach contract check --dir %s --old <base> --new <base>\n(expect unreached@N and no other problem), then show the contract to a person to approve.\n", d.Path)
	return exitPass
}

func contractShow(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("contract show", stderr)
	dir := fs.String("dir", contract.DefaultDir, "contract directory")
	asJSON := fs.Bool("json", false, "print the contract as JSON")
	if pos, err := parse(fs, args); err != nil || len(pos) != 0 {
		return exitUsage
	}
	d, err := contract.Load(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "kavach contract show: %v\n", err)
		return exitError
	}
	if *asJSON {
		return writeJSON(stdout, stderr, showReport(d))
	}
	printContract(u, stdout, d)
	return exitPass
}

func showReport(d *contract.Dir) map[string]any {
	var steps []map[string]any
	for _, r := range d.Traffic.Records {
		if r.Type == journal.TypeInput {
			steps = append(steps, map[string]any{"seq": r.Seq, "feature": d.Features[r.Seq], "input": showData(r.Data, -1)})
		}
	}
	return map[string]any{"dir": d.Path, "contract": d.Contract, "lock": d.Lock(), "feature_steps": d.FeatureSeqs(), "steps": steps}
}

func printContract(u ui, w io.Writer, d *contract.Dir) {
	u.printBanner(w)
	u.header(w, "KAVACH CONTRACT")
	c := d.Contract
	n := 0
	for _, r := range d.Traffic.Records {
		if r.Type == journal.TypeInput {
			n++
		}
	}
	fmt.Fprintf(w, "%s%s\n", u.label("dir", 12), d.Path)
	fmt.Fprintf(w, "%s%s\n", u.label("spec", 12), c.Spec)
	fmt.Fprintf(w, "%s%s · %d steps · %d feature steps\n", u.label("traffic", 12), contract.FileTraffic, n, len(d.Features))
	for _, m := range c.FeatureMatch {
		fmt.Fprintf(w, "%s%s\n", u.label("match", 12), m)
	}
	fmt.Fprintf(w, "%s%s\n", u.label("invariants", 12), orDash(strings.Join(c.Invariants, ", ")))
	if len(c.InvariantSources) > 0 {
		fmt.Fprintf(w, "%s%s\n", u.label("declared in", 12), strings.Join(c.InvariantSources, ", "))
	}
	l := d.Lock()
	state := l.State
	switch {
	case l.State == "approved" && !l.Interactive:
		state += " (with --yes, not at a terminal)"
	case l.State == "stale":
		state += fmt.Sprintf(": changed since approval: %s", strings.Join(l.Changed, ", "))
	}
	fmt.Fprintf(w, "%s%s\n\n", u.label("lock", 12), state)
	for _, r := range d.Traffic.Records {
		if r.Type != journal.TypeInput {
			continue
		}
		mark, style := "  ", ansiDim
		if d.Features[r.Seq] {
			mark, style = "F ", ansiYellow
		}
		fmt.Fprintf(w, "%s%s  %s\n", u.paint(mark, ansiBold, style), u.paint(fmt.Sprintf("%6d", r.Seq), ansiDim), showData(r.Data, 100))
	}
}

func contractApprove(u ui, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("contract approve", stderr)
	dir := fs.String("dir", contract.DefaultDir, "contract directory")
	yes := fs.Bool("yes", false, "approve without asking; the lock records that no one was asked")
	if pos, err := parse(fs, args); err != nil || len(pos) != 0 {
		return exitUsage
	}
	d, err := contract.Load(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "kavach contract approve: %v\n", err)
		return exitError
	}
	interactive := false
	if !*yes {
		f, ok := stdin.(*os.File)
		if !ok || !isTerminal(f) {
			fmt.Fprintln(stderr, "kavach contract approve: not at a terminal; a person approves a contract. Pass --yes only if one has.")
			return exitUsage
		}
		printContract(u, stdout, d)
		fmt.Fprint(stdout, "\nApprove this contract? [y/N] ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "not approved")
			return exitFail
		}
		interactive = true
	}
	if _, err := d.Approve(interactive, time.Now()); err != nil {
		fmt.Fprintf(stderr, "kavach contract approve: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "approved: %s\n", d.Path)
	return exitPass
}

func contractCheck(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("contract check", stderr)
	dir := fs.String("dir", contract.DefaultDir, "contract directory")
	oldBin := fs.String("old", "", "host command for the base build (checks that the traffic matches it)")
	newBin := fs.String("new", "", "host command for the build under test")
	noVariants := fs.Bool("no-variants", false, "skip variants of the feature steps")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if pos, err := parse(fs, args); err != nil || len(pos) != 0 {
		return exitUsage
	}
	if *newBin == "" {
		fmt.Fprintln(stderr, "kavach contract check: --new is required")
		return exitUsage
	}
	r, wall, err := checkContract(*dir, *oldBin, *newBin, *noVariants)
	if err != nil {
		fmt.Fprintf(stderr, "kavach contract check: %v\n", err)
		return exitError
	}
	if *asJSON {
		out := contractReport(*dir, r)
		out["wall_ms"] = ms(wall)
		if writeJSON(stdout, stderr, out) != exitPass {
			return exitError
		}
	} else {
		printCheck(u, stdout, *dir, r, wall)
	}
	if r.Passed {
		return exitPass
	}
	return exitFail
}

func checkContract(dir, oldBin, newBin string, noVariants bool) (*contract.Report, time.Duration, error) {
	d, err := contract.Load(dir)
	if err != nil {
		return nil, 0, err
	}
	o := contract.CheckOptions{New: hostContractReplayer(newBin), NoVariants: noVariants, Parallel: runtime.NumCPU(), Build: newBin}
	if oldBin != "" {
		o.Old = hostContractReplayer(oldBin)
	}
	start := time.Now()
	r, err := d.Check(o)
	return r, time.Since(start), err
}

func hostContractReplayer(bin string) contract.ReplayFunc {
	return func(j *journal.Journal, opts replay.Options) (*replay.Result, error) {
		return replay.RunHostWith(j, bin, opts)
	}
}

func contractReport(dir string, r *contract.Report) map[string]any {
	return map[string]any{"dir": dir, "report": r, "verdict": r.Verdict, "passed": r.Passed}
}

func printCheck(u ui, w io.Writer, dir string, r *contract.Report, wall time.Duration) {
	u.printBanner(w)
	u.header(w, "KAVACH CONTRACT CHECK")
	fmt.Fprintf(w, "%s%s · lock %s\n", u.label("contract", 10), dir, r.Lock.State)
	if r.Baseline != "" {
		fmt.Fprintf(w, "%s%s\n", u.label("baseline", 10), r.Baseline)
	}
	if r.New != "" {
		fmt.Fprintf(w, "%s%s\n", u.label("new", 10), r.New)
	}
	fmt.Fprintf(w, "%s%d of %d feature steps changed\n", u.label("features", 10), len(r.Changed), len(r.Features))
	for _, c := range r.Changed {
		fmt.Fprintf(w, "  %s\n", u.paint(fmt.Sprintf("step %d", c.Seq), ansiBold, ansiYellow))
		for i := 0; i < len(c.Recorded) || i < len(c.Replay); i++ {
			if i < len(c.Recorded) {
				fmt.Fprintf(w, "    %s %s\n", u.paint("was", ansiRed), showOutput(&c.Recorded[i]))
			}
			if i < len(c.Replay) {
				fmt.Fprintf(w, "    %s %s\n", u.paint("now", ansiGreen), showOutput(&c.Replay[i]))
			}
		}
	}
	if len(r.Unreached) > 0 {
		fmt.Fprintf(w, "%s%v\n", u.label("unreached", 10), r.Unreached)
	}
	if r.VariantCandidates > 0 {
		fmt.Fprintf(w, "%s%d of %d pass", u.label("variants", 10), r.VariantsPassed, r.VariantCandidates)
		if r.VariantsPreexisting > 0 {
			fmt.Fprintf(w, " · %d fail on the base build too", r.VariantsPreexisting)
		}
		fmt.Fprintln(w)
		for i, f := range r.VariantFailures {
			if i == 5 {
				fmt.Fprintf(w, "          %s\n", u.paint(fmt.Sprintf("… and %d more (--json lists them all)", len(r.VariantFailures)-i), ansiDim))
				break
			}
			fmt.Fprintf(w, "  %s %s\n       %s\n       %s\n", u.paint(fmt.Sprintf("✗ %2d", f.ID), ansiBold, ansiRed), f.Mutation, u.paint(f.Verdict+": "+f.Detail, ansiRed), u.paint("saved "+f.File, ansiDim))
		}
	}
	style := ansiRed
	if r.Passed {
		style = ansiGreen
	}
	fmt.Fprintf(w, "%s%s\n", u.label("verdict", 10), u.paint(r.Verdict, ansiBold, style))
	if r.Detail != "" && !r.Passed {
		fmt.Fprintf(w, "          %s\n", u.paint(r.Detail, style))
	}
	fmt.Fprintf(w, "%s%d (same outcome %d in a row)\n", u.label("round", 10), r.Round, r.Streak)
	fmt.Fprintf(w, "%s%s\n", u.label("wall", 10), u.paint(fmt.Sprintf("%.1f ms", ms(wall)), ansiYellow))
	if r.Streak >= 3 && !r.Passed {
		fmt.Fprintf(w, "\n%s\n", u.paint("The same outcome three rounds in a row: stop, and report what fails instead of trying another theory.", ansiBold, ansiYellow))
	}
}
