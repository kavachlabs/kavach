package onboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/internal/kconfig"
	"github.com/kavachlabs/kavach/internal/kmap"
)

// InitOptions configures `kavach init` repository scaffolding.
type InitOptions struct {
	Dir     string
	Service string
	Mode    string // "" (auto), "driver", or "scoped"
	Force   bool
	Version string
}

// FileAction describes a file created, overwritten, or skipped during Init.
type FileAction struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "created", "overwritten", "skipped"
	Bytes  int    `json:"bytes,omitempty"`
}

// InitResult summarizes the outcome of Init.
type InitResult struct {
	Dir        string        `json:"dir"`
	Service    string        `json:"service"`
	Language   string        `json:"language"`
	Mode       string        `json:"mode"`
	Drivers    []Driver      `json:"drivers"`
	Files      []FileAction  `json:"files"`
	MapNodes   int           `json:"map_nodes"`
	MapEdges   int           `json:"map_edges"`
	MapFlows   int           `json:"map_flows"`
	DurationMS float64       `json:"duration_ms"`
	Doctor     *DoctorReport `json:"doctor"`
}

// Init detects drivers, runs AST diagnostics, and scaffolds .kavach/ and
// .github/workflows/kavach.yml inside opts.Dir.
func Init(opts InitOptions) (*InitResult, error) {
	start := time.Now()
	if opts.Dir == "" {
		opts.Dir = "."
	}
	absDir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}

	rep, err := ScanRepo(absDir)
	if err != nil {
		return nil, err
	}

	service := strings.TrimSpace(opts.Service)
	if service == "" {
		service = rep.Service
	}
	rep.Service = service
	rep.SuggestedSnippet = buildSuggestedSnippet(rep)

	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	switch mode {
	case "":
		mode = rep.RecommendedMode
	case "driver", "scoped":
	default:
		return nil, fmt.Errorf("invalid mode %q (expected driver or scoped)", opts.Mode)
	}

	kavachDir := filepath.Join(absDir, ".kavach")
	capsulesDir := filepath.Join(kavachDir, "capsules")
	if err := os.MkdirAll(capsulesDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", capsulesDir, err)
	}

	var actions []FileAction
	writeIfNeeded := func(relPath string, content []byte) error {
		full := filepath.Join(absDir, filepath.FromSlash(relPath))
		existed := false
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			existed = true
			if !opts.Force {
				actions = append(actions, FileAction{Path: relPath, Status: "skipped", Bytes: int(st.Size())})
				return nil
			}
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return err
		}
		status := "created"
		if existed {
			status = "overwritten"
		}
		actions = append(actions, FileAction{Path: relPath, Status: status, Bytes: len(content)})
		return nil
	}

	// 1. .kavach/config.yaml
	cfgYAML := renderConfigYAML(service, mode, rep)
	if err := writeIfNeeded(".kavach/config.yaml", []byte(cfgYAML)); err != nil {
		return nil, err
	}

	// 2. .kavach/invariants.yaml
	invYAML := renderInvariantsYAML(service, rep)
	if err := writeIfNeeded(".kavach/invariants.yaml", []byte(invYAML)); err != nil {
		return nil, err
	}

	// 3. .kavach/map.json (synthesized from Go AST + .kavach/invariants.yaml)
	invPath := filepath.Join(absDir, ".kavach", "invariants.yaml")
	spec, err := kmap.AutoSpecFromDir(absDir, kmap.AutoOptions{
		Service:        service,
		InvariantsPath: invPath,
	})
	if err != nil {
		return nil, fmt.Errorf("auto-map scan: %w", err)
	}

	mapJSONPath := filepath.Join(absDir, ".kavach", "map.json")
	mapJSONExisted := fileExists(mapJSONPath)
	if !mapJSONExisted || opts.Force {
		if err := kmap.WriteSpecFile(mapJSONPath, spec); err != nil {
			return nil, fmt.Errorf("write .kavach/map.json: %w", err)
		}
		st, _ := os.Stat(mapJSONPath)
		sz := 0
		if st != nil {
			sz = int(st.Size())
		}
		status := "created"
		if mapJSONExisted {
			status = "overwritten"
		}
		actions = append(actions, FileAction{Path: ".kavach/map.json", Status: status, Bytes: sz})
	} else {
		st, _ := os.Stat(mapJSONPath)
		sz := 0
		if st != nil {
			sz = int(st.Size())
		}
		actions = append(actions, FileAction{Path: ".kavach/map.json", Status: "skipped", Bytes: sz})
		if loaded, err := kmap.LoadSpec(mapJSONPath); err == nil {
			spec = loaded
		}
	}

	// 4. .kavach/map.html (Day-0 interactive Flow Canvas using kmap renderer)
	capsules := findCapsules(capsulesDir)
	model, err := kmap.Build(kmap.Input{
		Spec:     spec,
		Fixtures: capsules,
		MaxData:  512,
		BaseDir:  kavachDir,
		Version:  opts.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("build map model: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := kmap.RenderHTML(&htmlBuf, model); err != nil {
		return nil, fmt.Errorf("render .kavach/map.html: %w", err)
	}
	if err := writeIfNeeded(".kavach/map.html", htmlBuf.Bytes()); err != nil {
		return nil, err
	}

	// 5. .github/workflows/kavach.yml (read-only PR merge gate)
	wfYAML := renderGitHubWorkflowYAML(service, rep)
	if err := writeIfNeeded(".github/workflows/kavach.yml", []byte(wfYAML)); err != nil {
		return nil, err
	}

	return &InitResult{
		Dir:        absDir,
		Service:    service,
		Language:   rep.Language,
		Mode:       mode,
		Drivers:    rep.Drivers,
		Files:      actions,
		MapNodes:   len(model.Nodes),
		MapEdges:   len(model.Edges),
		MapFlows:   len(model.Flows),
		DurationMS: float64(time.Since(start).Microseconds()) / 1000.0,
		Doctor:     rep,
	}, nil
}

func renderConfigYAML(service, mode string, rep *DoctorReport) string {
	cfg := kconfig.DefaultConfig(service)
	cfg.Mode = mode
	cfg.PII = kconfig.PIIScrubStandard
	cfg.RingPath = fmt.Sprintf("/dev/shm/kavach/%s.shm", service)
	cfg.VaultPath = kconfig.DefaultCapsulesDir
	cfg.Variants = 25
	for _, d := range rep.Drivers {
		cfg.Drivers = append(cfg.Drivers, d.Name)
	}
	raw, err := kconfig.MarshalConfig(cfg)
	if err != nil {
		return fmt.Sprintf("service: %s\nmode: %s\npii: standard\nvault_path: .kavach/capsules\nvariants: 25\n", service, mode)
	}
	return "# Kavach project configuration (git-native — commit this file).\n" + string(raw)
}

func renderInvariantsYAML(service string, rep *DoctorReport) string {
	inv := &kconfig.InvariantsFile{Service: service}

	hasKafka := hasCategory(rep.Drivers, "queue")
	hasHTTP := hasCategory(rep.Drivers, "http")
	hasDB := hasCategory(rep.Drivers, "database")

	switch {
	case hasKafka:
		primarySink := "kafka:events_out"
		var allSinks []string
		if len(rep.Topics) > 0 {
			var produceSinks []string
			for _, t := range rep.Topics {
				s := "kafka:" + t
				allSinks = append(allSinks, s)
				lower := strings.ToLower(t)
				if !strings.HasSuffix(lower, "_raw") && !strings.HasSuffix(lower, "_in") &&
					!strings.HasSuffix(lower, "_input") && !strings.HasSuffix(lower, "_incoming") {
					produceSinks = append(produceSinks, s)
				}
			}
			if len(produceSinks) > 0 {
				allSinks = produceSinks
			}
			primarySink = allSinks[0]
		} else {
			allSinks = []string{primarySink}
		}
		inv.Invariants = append(inv.Invariants,
			kconfig.InvariantRule{
				Name:             "atomic_single_emit_on_retry",
				Kind:             kconfig.KindAtMostOnceEmit,
				Sink:             primarySink,
				Key:              "$.transaction_id",
				OnErrorEmitCount: kconfig.IntPtr(0),
			},
			kconfig.InvariantRule{
				Name:               "no_silent_output_drop",
				Kind:               kconfig.KindAntiLobotomy,
				Sinks:              allSinks,
				MinRatioVsBaseline: 1.0,
			},
		)

	case hasHTTP:
		sinks := []string{"http:response"}
		if hasDB {
			sinks = append(sinks, "pg:exec")
		}
		inv.Invariants = append(inv.Invariants, kconfig.InvariantRule{
			Name:               "no_silent_response_drop",
			Kind:               kconfig.KindAntiLobotomy,
			Sinks:              sinks,
			MinRatioVsBaseline: 1.0,
		})
		if hasDB {
			inv.Invariants = append(inv.Invariants, kconfig.InvariantRule{
				Name:             "atomic_single_write_on_retry",
				Kind:             kconfig.KindAtMostOnceEmit,
				Sink:             "pg:exec",
				Key:              "$.idempotency_key",
				OnErrorEmitCount: kconfig.IntPtr(0),
			})
		}

	case hasDB:
		inv.Invariants = append(inv.Invariants, kconfig.InvariantRule{
			Name:               "no_silent_db_write_drop",
			Kind:               kconfig.KindAntiLobotomy,
			Sinks:              []string{"pg:exec"},
			MinRatioVsBaseline: 1.0,
		})

	default:
		inv.Invariants = append(inv.Invariants, kconfig.InvariantRule{
			Name:               "no_silent_output_drop",
			Kind:               kconfig.KindAntiLobotomy,
			Sinks:              []string{"service:output"},
			MinRatioVsBaseline: 1.0,
		})
	}

	raw, err := kconfig.MarshalInvariants(inv)
	if err != nil {
		return fmt.Sprintf("service: %s\ninvariants: []\n", service)
	}

	var b strings.Builder
	b.WriteString("# Declarative Kavach invariants — checked on every replay and mutated variant.\n")
	b.WriteString("# Kinds: at_most_once_emit, anti_lobotomy, non_negative, conservation\n")
	b.Write(raw)
	b.WriteString("\n  # Optional financial / state-machine examples (uncomment to activate):\n")
	b.WriteString("  # - name: balances_non_negative\n")
	b.WriteString("  #   kind: non_negative\n")
	b.WriteString("  #   sink: pg:exec\n")
	b.WriteString("  #   field: $.balance_cents\n")
	b.WriteString("  #\n")
	b.WriteString("  # - name: ledger_money_conserved\n")
	b.WriteString("  #   kind: conservation\n")
	b.WriteString("  #   sink: ledger.entries\n")
	b.WriteString("  #   field: $.delta_cents\n")
	return b.String()
}

func renderGitHubWorkflowYAML(service string, rep *DoctorReport) string {
	binOld := fmt.Sprintf("./bin/%s-base", service)
	binNew := fmt.Sprintf("./bin/%s-pr", service)
	buildNewCmd := fmt.Sprintf("go build -o %s .", binNew)
	tsEntry := ""
	if rep.Language == LangTypeScript {
		tsEntry = detectTSEntrypoint(rep.Dir)
		binOld = fmt.Sprintf("node /tmp/kavach-base/%s", tsEntry)
		binNew = fmt.Sprintf("node %s", tsEntry)
	} else if rep.Language == LangRust {
		buildNewCmd = "cargo build --release"
		binOld = fmt.Sprintf("./target-base/release/%s", service)
		binNew = fmt.Sprintf("./target/release/%s", service)
	}

	var b strings.Builder
	b.WriteString("# Kavach deterministic PR merge gate (read-only, offline, zero external DBs).\n")
	b.WriteString("name: Kavach Replay Gate\n\n")
	b.WriteString("on:\n")
	b.WriteString("  pull_request:\n")
	b.WriteString("  workflow_dispatch:\n\n")
	b.WriteString("permissions:\n")
	b.WriteString("  contents: read\n\n")
	b.WriteString("jobs:\n")
	b.WriteString("  kavach-verify:\n")
	b.WriteString("    runs-on: ubuntu-latest\n")
	b.WriteString("    steps:\n")
	b.WriteString("      - name: Checkout PR head\n")
	b.WriteString("        uses: actions/checkout@v4\n")
	b.WriteString("        with:\n")
	b.WriteString("          fetch-depth: 0\n\n")
	switch rep.Language {
	case LangTypeScript:
		b.WriteString("      - name: Set up Node.js\n")
		b.WriteString("        uses: actions/setup-node@v4\n")
		b.WriteString("        with:\n")
		b.WriteString("          node-version: '22'\n\n")
	case LangGo, LangUnknown:
		b.WriteString("      - name: Set up Go\n")
		b.WriteString("        uses: actions/setup-go@v5\n")
		b.WriteString("        with:\n")
		b.WriteString("          go-version-file: go.mod\n\n")
	}
	b.WriteString("      - name: Install Kavach CLI\n")
	b.WriteString("        run: curl -fsSL https://www.thekavachlabs.com/install.sh | KAVACH_SKIP_SKILLS=1 sh\n\n")
	b.WriteString("      - name: Build base and PR host binaries\n")
	b.WriteString("        shell: bash\n")
	b.WriteString("        run: |\n")
	b.WriteString("          set -euo pipefail\n")
	switch rep.Language {
	case LangTypeScript:
		b.WriteString("          npm ci\n")
		b.WriteString("          npm run build --if-present\n")
		b.WriteString("          git worktree add /tmp/kavach-base \"${{ github.event.pull_request.base.sha || 'HEAD' }}\"\n")
		b.WriteString("          (cd /tmp/kavach-base && npm ci && npm run build --if-present)\n\n")
	case LangGo, LangUnknown:
		b.WriteString("          mkdir -p ./bin\n")
		fmt.Fprintf(&b, "          %s\n", buildNewCmd)
		b.WriteString("          git worktree add /tmp/kavach-base \"${{ github.event.pull_request.base.sha || 'HEAD' }}\"\n")
		fmt.Fprintf(&b, "          (cd /tmp/kavach-base && go build -o \"$GITHUB_WORKSPACE/bin/%s-base\" .)\n", service)
		b.WriteString("          git worktree remove --force /tmp/kavach-base\n\n")
	default:
		fmt.Fprintf(&b, "          %s\n\n", buildNewCmd)
	}
	b.WriteString("      - name: Verify capsules across 25 mutated failure schedules\n")
	b.WriteString("        shell: bash\n")
	b.WriteString("        run: |\n")
	b.WriteString("          set -euo pipefail\n")
	b.WriteString("          shopt -s nullglob\n")
	b.WriteString("          capsules=(.kavach/capsules/*.kavach)\n")
	b.WriteString("          if [[ ${#capsules[@]} -eq 0 ]]; then\n")
	b.WriteString("            echo \"No capsules in .kavach/capsules/ yet. Record one with: kavach dev --cmd ...\"\n")
	b.WriteString("            exit 0\n")
	b.WriteString("          fi\n")
	fmt.Fprintf(&b, "          kavach diff .kavach/capsules/*.kavach --old %q --new %q --variants 25\n", binOld, binNew)
	if rep.Language == LangTypeScript {
		b.WriteString("          git worktree remove --force /tmp/kavach-base || true\n")
	}
	return b.String()
}

func detectTSEntrypoint(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err == nil {
		var pkg struct {
			Main string `json:"main"`
		}
		if json.Unmarshal(data, &pkg) == nil && strings.TrimSpace(pkg.Main) != "" {
			return strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(pkg.Main)), "./")
		}
	}
	for _, cand := range []string{"dist/index.js", "src/index.ts", "index.ts", "index.js"} {
		if fileExists(filepath.Join(dir, filepath.FromSlash(cand))) {
			return cand
		}
	}
	return "dist/index.js"
}

func hasCategory(drivers []Driver, cat string) bool {
	for _, d := range drivers {
		if d.Category == cat {
			return true
		}
	}
	return false
}

func findCapsules(dir string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.kavach"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
