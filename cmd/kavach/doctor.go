package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/kavachlabs/kavach/internal/onboard"
)

func cmdDoctor(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("doctor", stderr)
	dir := fs.String("dir", ".", "repository root directory to scan")
	asJSON := fs.Bool("json", false, "print the doctor report as JSON")

	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 1 {
		fmt.Fprintf(stderr, "kavach doctor: expected at most one directory argument, got %d\n", len(pos))
		return exitUsage
	}
	targetDir := *dir
	if len(pos) == 1 {
		targetDir = pos[0]
	}

	rep, err := onboard.ScanRepo(targetDir)
	if err != nil {
		fmt.Fprintf(stderr, "kavach doctor: %v\n", err)
		return exitError
	}

	if *asJSON {
		return writeJSON(stdout, stderr, rep)
	}

	u.printBanner(stdout)
	u.header(stdout, "KAVACH DOCTOR")
	fmt.Fprintf(stdout, "%s%s (%s · recommended mode: %s)\n", u.label("service", 10), u.paint(rep.Service, ansiBold), rep.Language, rep.RecommendedMode)
	fmt.Fprintf(stdout, "%s%d source files in %.1f ms\n", u.label("scanned", 10), rep.FilesScanned, rep.DurationMS)

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%s (%d)\n", u.paint("Detected drivers", ansiBold), len(rep.Drivers))
	if len(rep.Drivers) == 0 {
		fmt.Fprintln(stdout, "  (none detected — use `kavach init --mode scoped`)")
	} else {
		for _, d := range rep.Drivers {
			ver := ""
			if d.Version != "" {
				ver = " " + d.Version
			}
			fmt.Fprintf(stdout, "  %-14s %-9s -> %-14s (%s%s)\n", d.Name, d.Category, u.paint(d.Adapter, ansiCyan), d.Source, ver)
		}
	}

	printDoctorSites(u, stdout, "Driver init call sites", rep.InitSites, ansiGreen, 20)
	clockHeading := "Bare clock calls (time.Now / Since / Until)"
	if rep.Language == onboard.LangTypeScript {
		clockHeading = "Bare clock calls (Date.now / new Date / performance.now)"
	}
	printDoctorSites(u, stdout, clockHeading, rep.ClockCalls, ansiYellow, 20)
	printDoctorSites(u, stdout, "Bare rand / UUID calls", rep.RandCalls, ansiYellow, 20)
	if rep.Language != onboard.LangTypeScript || len(rep.DetachedContexts) > 0 {
		printDoctorSites(u, stdout, "Detached contexts (context.Background / TODO)", rep.DetachedContexts, ansiOrange, 20)
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%s (%d)\n", u.paint("Handler entrypoints", ansiBold), len(rep.Handlers))
	if len(rep.Handlers) == 0 {
		fmt.Fprintln(stdout, "  (none detected)")
	} else {
		limit := min(len(rep.Handlers), 15)
		for _, h := range rep.Handlers[:limit] {
			extra := ""
			if h.Route != "" {
				extra = " [" + h.Method + " " + h.Route + "]"
			}
			fmt.Fprintf(stdout, "  %s:%d  %-6s  %s%s\n", h.File, h.Line, h.Kind, h.Name, extra)
		}
		if len(rep.Handlers) > limit {
			fmt.Fprintf(stdout, "  ... and %d more (--json lists all)\n", len(rep.Handlers)-limit)
		}
	}

	if len(rep.Topics) > 0 {
		fmt.Fprintf(stdout, "\n%s %s\n", u.paint("Kafka topics:", ansiBold), strings.Join(rep.Topics, ", "))
	}

	if rep.SuggestedSnippet != "" {
		fmt.Fprintln(stdout)
		snippetHeading := "Suggested 1-line kavach-go wrap snippet:"
		if rep.Language == onboard.LangTypeScript {
			snippetHeading = "Suggested 1-line @kavach/* wrap snippet:"
		}
		fmt.Fprintf(stdout, "%s\n%s\n", u.paint(snippetHeading, ansiBold), rep.SuggestedSnippet)
	}
	return exitPass
}

func printDoctorSites(u ui, w io.Writer, heading string, sites []onboard.CallSite, color string, limit int) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s (%d)\n", u.paint(heading, ansiBold), len(sites))
	if len(sites) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	shown := min(len(sites), limit)
	for _, s := range sites[:shown] {
		fn := ""
		if s.Func != "" {
			fn = " in " + s.Func
		}
		extra := ""
		if s.Adapter != "" {
			extra = " -> " + s.Adapter
		} else if s.Detail != "" {
			extra = " — " + s.Detail
		}
		fmt.Fprintf(w, "  %s:%d  %s%s%s\n", s.File, s.Line, u.paint(s.Symbol, color), fn, extra)
	}
	if len(sites) > shown {
		fmt.Fprintf(w, "  ... and %d more (--json lists all)\n", len(sites)-shown)
	}
}
