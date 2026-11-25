package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/internal/kmap"
	"github.com/kavachlabs/kavach/replay"
)

// openBrowserFn is injectable for unit tests.
var openBrowserFn = openInBrowser

func cmdMap(u ui, args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cmdMapWithContext(ctx, u, args, stdout, stderr)
}

func cmdMapWithContext(ctx context.Context, u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("map", stderr)
	bin := fs.String("bin", "", "host command to replay fixtures against (no env default)")
	newBin := fs.String("new", "", "candidate fixed host command (requires --bin)")
	var diffs stringList
	fs.Var(&diffs, "diff", "kavach diff --json report to attach (may be repeated)")
	var overlays stringList
	fs.Var(&overlays, "overlay", "stage overlay JSON file to attach (may be repeated)")
	var watchDirs stringList
	fs.Var(&watchDirs, "watch", "recorder or fixtures directory to watch live (may be repeated)")
	var sources stringList
	fs.Var(&sources, "source", "source root for code excerpts: DIR, or GROUP=DIR for one spec group (may be repeated)")
	var patches stringList
	fs.Var(&patches, "patch", "unified diff whose hunks attach to the nodes they touch (may be repeated)")
	listenAddr := fs.String("listen", "127.0.0.1:8090", "HTTP listen address for live map server")
	interval := fs.Duration("interval", 500*time.Millisecond, "poll interval for --watch")
	var outPath string
	fs.StringVar(&outPath, "o", "", "output HTML file (default kavach-map.html)")
	fs.StringVar(&outPath, "out", "", "output HTML file (default kavach-map.html)")
	asJSON := fs.Bool("json", false, "print the built map model as JSON")
	maxData := fs.Int("max-data", 512, "max bytes of input/output payload previews (0 omits payloads)")
	title := fs.String("title", "", "override the map title")
	autoGen := fs.Bool("auto", false, "synthesize .kavach/map.json from static Go AST and .kavach/invariants.yaml")
	scanDir := fs.String("dir", ".", "repository directory to scan when --auto or default .kavach/ layout is used")
	openBrowser := fs.Bool("open", false, "open generated HTML map in default browser (disabled when KAVACH_NO_OPEN=1)")

	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}

	var specPath string
	var fixtures []string
	for _, p := range pos {
		lower := strings.ToLower(p)
		switch {
		case strings.HasSuffix(lower, ".json"):
			if specPath != "" {
				fmt.Fprintln(stderr, "kavach map: expected at most one map spec (*.json)")
				return exitUsage
			}
			specPath = p
		case strings.HasSuffix(lower, ".kavach"):
			fixtures = append(fixtures, p)
		default:
			fmt.Fprintf(stderr, "kavach map: unrecognized positional argument %q (expected *.json or *.kavach)\n", p)
			return exitUsage
		}
	}

	usedKavachDefaults := false
	if *autoGen {
		absDir, err := filepath.Abs(*scanDir)
		if err != nil {
			fmt.Fprintf(stderr, "kavach map: %v\n", err)
			return exitError
		}
		if specPath == "" {
			specPath = filepath.Join(absDir, kconfig.DefaultMapJSONFile)
		}
		invPath := kconfig.FindInvariantsFile(absDir)
		autoSpec, err := kmap.AutoSpecFromDir(absDir, kmap.AutoOptions{
			Title:          *title,
			InvariantsPath: invPath,
		})
		if err != nil {
			fmt.Fprintf(stderr, "kavach map --auto: %v\n", err)
			return exitError
		}
		if err := kmap.WriteSpecFile(specPath, autoSpec); err != nil {
			fmt.Fprintf(stderr, "kavach map --auto: write %s: %v\n", specPath, err)
			return exitError
		}
		if len(fixtures) == 0 {
			fixtures = globCapsules(filepath.Join(absDir, kconfig.DefaultCapsulesDir))
		}
		usedKavachDefaults = true
	} else if specPath == "" && len(fixtures) == 0 && len(overlays) == 0 && len(watchDirs) == 0 {
		// Clean zero-arg default in initialized repos: .kavach/map.json + .kavach/capsules/*.kavach -> .kavach/map.html
		absDir, err := filepath.Abs(*scanDir)
		if err == nil {
			candSpec := filepath.Join(absDir, kconfig.DefaultMapJSONFile)
			if st, serr := os.Stat(candSpec); serr == nil && !st.IsDir() {
				specPath = candSpec
				usedKavachDefaults = true
			}
			if caps := globCapsules(filepath.Join(absDir, kconfig.DefaultCapsulesDir)); len(caps) > 0 {
				fixtures = caps
				usedKavachDefaults = true
			}
		}
	}

	if specPath == "" && len(fixtures) == 0 && len(overlays) == 0 && len(watchDirs) == 0 {
		fmt.Fprintln(stderr, "kavach map: pass a map.json spec, at least one *.kavach fixture, --auto, --overlay, or --watch (or run 'kavach init')")
		return exitUsage
	}
	if *newBin != "" && *bin == "" {
		fmt.Fprintln(stderr, "kavach map: --new requires --bin")
		return exitUsage
	}
	if *maxData < 0 {
		fmt.Fprintln(stderr, "kavach map: --max-data must not be negative")
		return exitUsage
	}
	if *interval <= 0 {
		fmt.Fprintln(stderr, "kavach map: --interval must be positive")
		return exitUsage
	}
	if len(watchDirs) > 0 {
		if *asJSON {
			fmt.Fprintln(stderr, "kavach map: --watch cannot be combined with --json")
			return exitUsage
		}
		if outPath != "" {
			fmt.Fprintln(stderr, "kavach map: --watch cannot be combined with -o/--out")
			return exitUsage
		}
		baseDir, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "kavach map: %v\n", err)
			return exitError
		}
		return runMapLive(ctx, u, kmap.LiveConfig{
			WatchDirs: watchDirs,
			Fixtures:  fixtures,
			SpecPath:  specPath,
			Bin:       *bin,
			New:       *newBin,
			Diffs:     diffs,
			Overlays:  overlays,
			Sources:   sources,
			Patches:   patches,
			MaxData:   *maxData,
			BaseDir:   baseDir,
			Version:   version,
			Title:     *title,
			Interval:  *interval,
		}, *listenAddr, stdout, stderr)
	}

	writeHTML := !*asJSON || outPath != ""
	htmlDest := outPath
	if htmlDest == "" && writeHTML {
		if usedKavachDefaults {
			absDir, _ := filepath.Abs(*scanDir)
			htmlDest = filepath.Join(absDir, kconfig.DefaultMapHTMLFile)
		} else {
			htmlDest = "kavach-map.html"
		}
	}

	baseDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "kavach map: %v\n", err)
		return exitError
	}
	if outPath != "" {
		absOut, err := filepath.Abs(outPath)
		if err != nil {
			fmt.Fprintf(stderr, "kavach map: %v\n", err)
			return exitError
		}
		baseDir = filepath.Dir(absOut)
	}

	var spec *kmap.Spec
	if specPath != "" {
		spec, err = kmap.LoadSpec(specPath)
		if err != nil {
			fmt.Fprintf(stderr, "kavach map: %v\n", err)
			return exitError
		}
	}

	if (len(sources) > 0 || len(patches) > 0) && spec == nil {
		fmt.Fprintln(stderr, "kavach map: --source and --patch need a map spec (*.json)")
		return exitUsage
	}
	roots, err := kmap.ParseSourceRoots(sources, spec)
	if err != nil {
		fmt.Fprintf(stderr, "kavach map: %v\n", err)
		return exitUsage
	}

	m, err := kmap.Build(kmap.Input{
		Sources:  roots,
		Patches:  patches,
		Spec:     spec,
		Fixtures: fixtures,
		Bin:      *bin,
		New:      *newBin,
		Diffs:    diffs,
		Overlays: overlays,
		MaxData:  *maxData,
		BaseDir:  baseDir,
		Version:  version,
		Title:    *title,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kavach map: %v\n", err)
		return exitError
	}

	var replayErrs []string
	for _, fx := range m.Fixtures {
		for _, rn := range fx.Runs {
			if rn.Error != "" {
				replayErrs = append(replayErrs, fmt.Sprintf("%s (%s): %s", fx.Label, rn.ID, rn.Error))
			}
		}
	}

	var htmlBytes int
	if writeHTML {
		var buf bytes.Buffer
		if err := kmap.RenderHTML(&buf, m); err != nil {
			fmt.Fprintf(stderr, "kavach map: %v\n", err)
			return exitError
		}
		htmlBytes = buf.Len()
		if dir := filepath.Dir(htmlDest); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(stderr, "kavach map: %v\n", err)
				return exitError
			}
		}
		if err := os.WriteFile(htmlDest, buf.Bytes(), 0o644); err != nil {
			fmt.Fprintf(stderr, "kavach map: %v\n", err)
			return exitError
		}
		if htmlBytes > 5*1024*1024 {
			fmt.Fprintf(stderr, "kavach map: warning: %s is %.1f MB (> 5 MB)\n", htmlDest, float64(htmlBytes)/(1024*1024))
		}
	}

	if *asJSON {
		if writeJSON(stdout, stderr, m) != exitPass {
			return exitError
		}
	} else {
		u.printBanner(stdout)
		u.header(stdout, "KAVACH MAP")
		specLabel := specPath
		if specLabel == "" {
			specLabel = "auto"
		}
		fmt.Fprintf(stdout, "%s%s · %d nodes · %d edges · %d flows\n",
			u.label("map", 10), specLabel, len(m.Nodes), len(m.Edges), len(m.Flows))
		for _, fx := range m.Fixtures {
			var parts []string
			for _, rn := range fx.Runs {
				v := rn.Verdict
				switch {
				case rn.Error != "":
					v = u.paint("error: "+rn.Error, ansiRed)
				case rn.Mode == "replay":
					v = u.verdict(replay.Status(rn.Status), rn.Verdict)
				case rn.Status == "ok":
					v = u.verdict(replay.StatusOK, rn.Verdict)
				default:
					v = u.paint(rn.Verdict, markerColor(rn.Status))
				}
				parts = append(parts, rn.ID+"="+v)
			}
			fmt.Fprintf(stdout, "%s%s · %s\n", u.label("fixture", 10), fx.Label, strings.Join(parts, " · "))
		}
		for _, ov := range m.Overlays {
			fmt.Fprintf(stdout, "%s%s · %d lanes\n", u.label("overlays", 10), ov.Mode, len(ov.Lanes))
		}
		if len(m.Warnings) > 0 {
			for _, w := range m.Warnings {
				fmt.Fprintf(stdout, "%s%s\n", u.label("warning", 10), u.paint(w, ansiYellow))
			}
		}
		if writeHTML {
			fmt.Fprintf(stdout, "%s%s (%.1f KB)\n", u.label("wrote", 10), htmlDest, float64(htmlBytes)/1024)
		}
	}

	if *openBrowser && writeHTML && os.Getenv("KAVACH_NO_OPEN") != "1" {
		if absHTML, err := filepath.Abs(htmlDest); err == nil {
			if err := openBrowserFn(absHTML); err != nil {
				fmt.Fprintf(stderr, "kavach map: open browser: %v\n", err)
			}
		}
	}

	if len(replayErrs) > 0 {
		for _, re := range replayErrs {
			fmt.Fprintf(stderr, "kavach map: replay %s\n", re)
		}
		return exitFail
	}
	return exitPass
}

func globCapsules(dir string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.kavach"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

func openInBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
