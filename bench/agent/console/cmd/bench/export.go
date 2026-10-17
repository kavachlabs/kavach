package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
	"github.com/kavachlabs/kavach/bench/agent/console/web"
)

// cmdExport writes a read-only copy of the web console for a static host:
// the app, and the response to every GET it makes for the chosen runs, each
// as a file. bench api starts runs and holds API keys, so it never leaves
// 127.0.0.1; the copy can only show what was run.
func cmdExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	o.register(fs)
	out := fs.String("out", "", "the directory to write the copy to (required; must be empty or not exist)")
	jobs := fs.String("job", "", "comma-separated runs to include (default: every finished run)")
	landing := fs.String("landing", "", "the run the copy opens on (default: the newest included)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(stderr, "bench export: -out is required")
		return 2
	}
	cfg, err := o.config()
	if err != nil {
		fmt.Fprintf(stderr, "bench export: %v\n", err)
		return 2
	}
	cfg.Web = web.Files
	var ids []string
	if *jobs != "" {
		ids = strings.Split(*jobs, ",")
	}
	e := &exporter{h: api.New(cfg), out: *out, scrub: scrubber(cfg)}
	n, err := e.export(ids, *landing)
	if err != nil {
		fmt.Fprintf(stderr, "bench export: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "bench export: %d runs, %d files in %s\n", len(e.jobs), n, *out)
	if home, err := os.UserHomeDir(); err == nil && e.leaks > 0 {
		fmt.Fprintf(stderr, "bench export: %d files still name %s; read them before publishing\n", e.leaks, home)
	}
	return 0
}

// scrubber rewrites the machine in what the copy holds: the repository's
// paths become relative to it, the home directory becomes ~, and the host
// name and user name that fixtures recorded here hold (host.hostname,
// host.user) become placeholders.
func scrubber(cfg api.Config) *strings.Replacer {
	var pairs []string
	add := func(dir, as string) {
		if dir != "" {
			pairs = append(pairs, dir+"/", as+"/", dir, as)
		}
	}
	add(cfg.JobsDir, "bench/agent/results/harbor")
	add(cfg.TasksDir, "bench/agent/harbor/tasks")
	add(cfg.Root, ".")
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "~")
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		pairs = append(pairs, h, "host")
	}
	if u, err := user.Current(); err == nil && len(u.Username) > 2 {
		pairs = append(pairs, u.Username, "user")
	}
	return strings.NewReplacer(pairs...)
}

type exporter struct {
	h     http.Handler
	out   string
	scrub *strings.Replacer
	jobs  []harbor.Job
	files int
	leaks int // files that name the home directory after scrubbing
}

// export writes the app and the runs named by ids (every finished run when
// empty), and returns how many files it wrote.
func (e *exporter) export(ids []string, landing string) (int, error) {
	if entries, err := os.ReadDir(e.out); err == nil && len(entries) > 0 {
		return 0, fmt.Errorf("%s is not empty", e.out)
	}
	var all []harbor.Job
	if err := e.getJSON("/api/jobs", &all); err != nil {
		return 0, err
	}
	for _, j := range all {
		if ids == nil && j.Status == "done" || slices.Contains(ids, j.ID) {
			e.jobs = append(e.jobs, j)
		}
	}
	for _, id := range ids {
		if !slices.ContainsFunc(e.jobs, func(j harbor.Job) bool { return j.ID == id }) {
			return 0, fmt.Errorf("no run named %q", id)
		}
	}
	if len(e.jobs) == 0 {
		return 0, errors.New("no runs to export")
	}
	if landing == "" {
		landing = e.jobs[0].ID // the newest: /api/jobs lists newest first
	} else if !slices.ContainsFunc(e.jobs, func(j harbor.Job) bool { return j.ID == landing }) {
		return 0, fmt.Errorf("-landing %q is not one of the runs exported", landing)
	}
	if err := e.app(landing); err != nil {
		return 0, err
	}
	for i := range e.jobs {
		readOnly(&e.jobs[i])
	}
	if err := e.putJSON("/api/jobs", e.jobs); err != nil {
		return 0, err
	}
	if err := e.copy("/api/tasks"); err != nil {
		return 0, err
	}
	for _, j := range e.jobs {
		if err := e.job(j.ID); err != nil {
			return 0, fmt.Errorf("run %s: %w", j.ID, err)
		}
	}
	return e.files, nil
}

// readOnly clears what offers to change a run: the copy has no server.
func readOnly(j *harbor.Job) {
	j.Stoppable, j.Resumable, j.Retryable = false, false, false
}

// app writes the web app, with index.html marked as a copy that opens on
// landing.
func (e *exporter) app(landing string) error {
	return fs.WalkDir(web.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := web.Files.ReadFile(p)
		if err != nil {
			return err
		}
		if p == "index.html" {
			meta := fmt.Sprintf(`<meta charset="utf-8">`+"\n"+`<meta name="bench-static" content="%s">`, landing)
			b = bytes.Replace(b, []byte(`<meta charset="utf-8">`), []byte(meta), 1)
		}
		return e.write(p, b)
	})
}

func (e *exporter) job(id string) error {
	base := "/api/jobs/" + url.PathEscape(id)
	var j harbor.Job
	if err := e.getJSON(base, &j); err != nil {
		return err
	}
	readOnly(&j)
	if err := e.putJSON(base, j); err != nil {
		return err
	}
	if err := e.copy(base + "/compare"); err != nil {
		return err
	}
	for _, t := range j.Trials {
		if t.ID == "" {
			continue
		}
		if err := e.trial(id, t.ID); err != nil {
			return fmt.Errorf("attempt %s: %w", t.ID, err)
		}
	}
	return nil
}

// trial writes what the attempt inspector reads. An attempt lacks some of
// it (no patch, no contract); what the server answers 404 is left out, and
// the copy answers 404 too.
func (e *exporter) trial(job, id string) error {
	base := "/api/trials/" + url.PathEscape(job) + "/" + url.PathEscape(id)
	for _, p := range []string{"", "/patch", "/grade", "/incident"} {
		if err := e.copy(base + p); err != nil {
			return err
		}
	}
	var steps []struct {
		Contract *struct {
			Invariants []string `json:"invariants"`
			Declared   []string `json:"declared"`
		} `json:"contract"`
	}
	if err := e.getJSON(base+"/steps", &steps); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if err := e.copy(base + "/steps"); err != nil {
		return err
	}
	var variants harbor.VariantList
	err := e.getJSON(base+"/variants", &variants)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if err == nil {
		if err := e.copy(base + "/variants"); err != nil {
			return err
		}
		for _, v := range variants.Variants {
			if err := e.copy(fmt.Sprintf("%s/variants/%d/%d", base, v.Round, v.ID)); err != nil {
				return err
			}
		}
	}
	// The invariants view asks for the names its checks report, in the
	// order invariants.js's loadDefs finds them.
	var names []string
	for _, st := range steps {
		if st.Contract == nil {
			continue
		}
		for _, n := range slices.Concat(st.Contract.Invariants, st.Contract.Declared) {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return nil // no contract checks: the inspector has no invariants tab
	}
	inv := base + "/invariants?names=" + url.QueryEscape(strings.Join(names, ","))
	var defs harbor.Invariants
	if err := e.getJSON(inv, &defs); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return err
	}
	// Without the trial's image or container, the invariants are read from
	// the patches alone, and the note says why in terms of this machine
	// (build the image here), which a reader of the copy can't act on.
	if defs.Source == "patch" {
		defs.SourceNote = ""
	}
	if err := e.putJSON(inv, defs); err != nil {
		return err
	}
	for i := range defs.Sources {
		if err := e.copy(base + "/invariants/source?i=" + strconv.Itoa(i)); err != nil {
			return err
		}
	}
	for _, f := range defs.Files {
		if err := e.copy(base + "/invariants/file?path=" + url.QueryEscape(f)); err != nil {
			return err
		}
	}
	return nil
}

var errNotFound = errors.New("not found")

// get asks the API for target, as the browser would.
func (e *exporter) get(target string) ([]byte, error) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	switch rec.Code {
	case http.StatusOK:
		return rec.Body.Bytes(), nil
	case http.StatusNotFound:
		return nil, errNotFound
	}
	return nil, fmt.Errorf("GET %s: %d %s", target, rec.Code, strings.TrimSpace(rec.Body.String()))
}

func (e *exporter) getJSON(target string, v any) error {
	b, err := e.get(target)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// copy writes the API's answer to target, unless it has none (404).
func (e *exporter) copy(target string) error {
	b, err := e.get(target)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.write(staticPath(target), b)
}

func (e *exporter) putJSON(target string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return e.write(staticPath(target), append(b, '\n'))
}

func (e *exporter) write(rel string, b []byte) error {
	s := e.scrub.Replace(string(b))
	if home, err := os.UserHomeDir(); err == nil && strings.Contains(s, home) {
		e.leaks++
	}
	p := filepath.Join(e.out, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	e.files++
	return os.WriteFile(p, []byte(s), 0o644)
}

// staticPath is the file the copy holds an API response in, where app.js's
// apiURL looks for it: path/index.json, or for the invariants' texts
// path/<i>.txt (?i=) and path/<FNV-1a of the file's path>.txt (?path=).
// ?names= is dropped: the copy holds the answer to the names the app asks.
func staticPath(target string) string {
	p, q, _ := strings.Cut(target, "?")
	p = strings.TrimPrefix(p, "/")
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	v, _ := url.ParseQuery(q)
	if v.Has("i") {
		return p + "/" + v.Get("i") + ".txt"
	}
	if v.Has("path") {
		h := fnv.New32a()
		h.Write([]byte(v.Get("path")))
		return fmt.Sprintf("%s/%08x.txt", p, h.Sum32())
	}
	return p + "/index.json"
}
