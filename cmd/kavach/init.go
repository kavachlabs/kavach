package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/kavachlabs/kavach/internal/onboard"
)

func cmdInit(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("init", stderr)
	dir := fs.String("dir", ".", "repository root directory to initialize")
	service := fs.String("service", "", "override detected service name")
	mode := fs.String("mode", "", "onboarding mode: driver or scoped (default: auto-detected)")
	force := fs.Bool("force", false, "overwrite existing .kavach/ and workflow files")
	asJSON := fs.Bool("json", false, "print the initialization report as JSON")

	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 1 {
		fmt.Fprintf(stderr, "kavach init: expected at most one directory argument, got %d\n", len(pos))
		return exitUsage
	}
	targetDir := *dir
	if len(pos) == 1 {
		targetDir = pos[0]
	}

	res, err := onboard.Init(onboard.InitOptions{
		Dir:     targetDir,
		Service: *service,
		Mode:    *mode,
		Force:   *force,
		Version: version,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kavach init: %v\n", err)
		return exitError
	}

	if *asJSON {
		return writeJSON(stdout, stderr, res)
	}

	u.printBanner(stdout)
	u.header(stdout, "KAVACH INIT")
	fmt.Fprintf(stdout, "%s%s (%s · mode=%s)\n", u.label("service", 10), u.paint(res.Service, ansiBold), res.Language, res.Mode)
	if len(res.Drivers) > 0 {
		names := make([]string, 0, len(res.Drivers))
		for _, d := range res.Drivers {
			names = append(names, fmt.Sprintf("%s (%s)", d.Name, d.Adapter))
		}
		fmt.Fprintf(stdout, "%s%s\n", u.label("drivers", 10), strings.Join(names, ", "))
	} else {
		fmt.Fprintf(stdout, "%snone detected (using %s mode)\n", u.label("drivers", 10), res.Mode)
	}
	fmt.Fprintf(stdout, "%s%d nodes · %d edges · %d flows (scanned %d source files in %.1f ms)\n",
		u.label("topology", 10), res.MapNodes, res.MapEdges, res.MapFlows, res.Doctor.FilesScanned, res.DurationMS)

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%s\n", u.paint("Files:", ansiBold))
	for _, f := range res.Files {
		badge := u.paint(f.Status, ansiGreen)
		if f.Status == "skipped" {
			badge = u.paint(f.Status, ansiDim)
		} else if f.Status == "overwritten" {
			badge = u.paint(f.Status, ansiYellow)
		}
		fmt.Fprintf(stdout, "  %-11s  %s (%d bytes)\n", badge, f.Path, f.Bytes)
	}

	if res.Doctor != nil && (len(res.Doctor.ClockCalls) > 0 || len(res.Doctor.RandCalls) > 0 || len(res.Doctor.DetachedContexts) > 0) {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "%s %d bare clock · %d bare rand/uuid · %d detached context (run `kavach doctor` for locations)\n",
			u.paint("Doctor:", ansiBold),
			len(res.Doctor.ClockCalls), len(res.Doctor.RandCalls), len(res.Doctor.DetachedContexts))
	}

	if res.Doctor != nil && res.Doctor.SuggestedSnippet != "" {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "%s\n%s\n", u.paint("Next — wrap your drivers at client init:", ansiBold), res.Doctor.SuggestedSnippet)
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "%s\n", u.paint("Commands:", ansiBold))
	fmt.Fprintln(stdout, "  kavach map --open                                   # inspect Day-0 topology canvas")
	fmt.Fprintln(stdout, "  kavach doctor                                       # audit clock, rand, and context sites")
	if res.Language == onboard.LangTypeScript {
		fmt.Fprintln(stdout, "  kavach dev --cmd \"node dist/index.js\" --out .kavach/capsules/ # record local traffic")
		fmt.Fprintln(stdout, "  kavach diff .kavach/capsules/*.kavach --old \"node base/dist/index.js\" --new \"node dist/index.js\" --variants 25")
	} else {
		fmt.Fprintln(stdout, "  kavach dev --cmd \"go run .\" --out .kavach/capsules/ # record local traffic")
		fmt.Fprintln(stdout, "  kavach diff .kavach/capsules/*.kavach --old ./bin/base --new ./bin/pr --variants 25")
	}
	return exitPass
}
