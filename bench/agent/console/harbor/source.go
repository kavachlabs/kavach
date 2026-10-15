package harbor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Docker is what reading a trial's real source files needs from Docker.
type Docker interface {
	// Running reports whether the named container is running.
	Running(name string) bool
	// ImageCreated is when the image was built; an error when it doesn't exist.
	ImageCreated(image string) (time.Time, error)
	// Start starts a throwaway container with no network and returns its id.
	Start(image string) (string, error)
	// Exec runs a command in a container, feeding it stdin.
	Exec(container string, stdin []byte, args ...string) ([]byte, error)
	// Remove deletes a container started by Start.
	Remove(container string)
}

// CLIDocker is Docker over the docker command.
type CLIDocker struct{}

func docker(stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (CLIDocker) Running(name string) bool {
	out, err := docker(nil, "container", "inspect", "-f", "{{.State.Running}}", name)
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (CLIDocker) ImageCreated(image string) (time.Time, error) {
	out, err := docker(nil, "image", "inspect", "-f", "{{.Created}}", image)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(string(out)))
}

func (CLIDocker) Start(image string) (string, error) {
	out, err := docker(nil, "run", "-d", "--rm", "--network", "none", "--entrypoint", "sleep", image, "600")
	return strings.TrimSpace(string(out)), err
}

func (CLIDocker) Exec(container string, stdin []byte, args ...string) ([]byte, error) {
	return docker(stdin, append([]string{"exec", "-i", container}, args...)...)
}

func (CLIDocker) Remove(container string) { docker(nil, "rm", "-f", container) }

// Where a trial's real source came from.
const (
	SourceContainer = "container" // the trial's own container, still running
	SourceImage     = "image"     // the task's image with the trial's patch applied
	SourcePatch     = "patch"     // neither: only the patches' lines are known
)

// srcLocks serialises the work on one trial, so two requests don't both
// start a container for it.
var srcLocks sync.Map

// RealSource adds to inv the real files its snippets live in: the whole
// files as the agent left them, cached under r.CacheDir, with each snippet's
// line in them. When they can't be had, inv.Source is "patch" and
// inv.SourceNote says why, and the snippets are as they were.
func (r *Reader) RealSource(tasksDir, job, id string, inv *Invariants) error {
	inv.Source, inv.Files = SourcePatch, []string{}
	_, srcs, err := r.invariantSources(tasksDir, job, id)
	if err != nil {
		return err
	}
	if inv.Files = snippetFiles(inv); inv.Files == nil {
		inv.Files = []string{}
	}
	if len(inv.Files) == 0 {
		return nil
	}
	if r.Docker == nil || r.CacheDir == "" {
		inv.SourceNote = "Docker isn't configured, so only the patches are shown."
		return nil
	}
	mu, _ := srcLocks.LoadOrStore(job+"/"+id, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	dir := filepath.Join(r.CacheDir, job, id)
	var agentPatch []byte
	for _, s := range srcs {
		if s.agent {
			agentPatch = s.text
		}
	}
	source, note, err := r.fetchSource(tasksDir, job, id, dir, inv.Files, agentPatch)
	if err != nil {
		inv.SourceNote = err.Error()
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte(source), 0o644); err != nil {
		inv.SourceNote = err.Error()
		return nil
	}
	inv.Source, inv.SourceNote = source, note
	mapSnippets(inv, filepath.Join(dir, source), srcs)
	return nil
}

// snippetFiles lists the repo-relative files the snippets are in.
func snippetFiles(inv *Invariants) []string {
	var files []string
	add := func(s *Snippet) {
		if s != nil && fs.ValidPath(s.Path) && !slices.Contains(files, s.Path) {
			files = append(files, s.Path)
		}
	}
	for i := range inv.Invariants {
		add(inv.Invariants[i].DocAt)
		for j := range inv.Invariants[i].Code {
			add(&inv.Invariants[i].Code[j])
		}
	}
	slices.Sort(files)
	return files
}

func (r *Reader) fetchSource(tasksDir, job, id, dir string, files []string, agentPatch []byte) (source, note string, err error) {
	if name := strings.ToLower(id) + "__env-main-1"; r.Docker.Running(name) {
		out := filepath.Join(dir, SourceContainer)
		os.RemoveAll(out)
		if err := copyOut(r.Docker, name, out, files); err != nil {
			return "", "", fmt.Errorf("Couldn't read the running container: %v", err)
		}
		return SourceContainer, "Read from the trial's running container; the agent may still be changing it.", nil
	}
	image, build, err := r.trialImage(tasksDir, job, id)
	if err != nil {
		return "", "", err
	}
	out := filepath.Join(dir, SourceImage)
	if b, err := os.ReadFile(filepath.Join(out, ".done")); err == nil {
		var done struct{ Note string }
		missing := slices.ContainsFunc(files, func(f string) bool { _, err := os.Stat(filepath.Join(out, filepath.FromSlash(f))); return err != nil })
		if json.Unmarshal(b, &done) == nil && !missing {
			return SourceImage, done.Note, nil
		}
	}
	created, err := r.Docker.ImageCreated(image)
	if err != nil {
		return "", "", fmt.Errorf("The image %s isn't available (%v); build it from the repository root with: %s", image, headLine(err.Error()), build)
	}
	id0, err := r.Docker.Start(image)
	if err != nil {
		return "", "", err
	}
	defer r.Docker.Remove(id0)
	if err := applyPatch(r.Docker, id0, agentPatch); err != nil {
		return "", "", fmt.Errorf("The agent's patch doesn't apply to %s (%v); it may have been rebuilt since the trial ran. To rebuild it: %s", image, headLine(err.Error()), build)
	}
	os.RemoveAll(out)
	if err := copyOut(r.Docker, id0, out, files); err != nil {
		return "", "", err
	}
	if !anyFile(out) {
		return "", "", fmt.Errorf("The trial's tree has none of these files (%s); the patches show code its arm doesn't contain.", strings.Join(files, ", "))
	}
	if started := r.trialStart(job, id); !started.IsZero() && created.After(started) {
		note = fmt.Sprintf("%s was built at %s, after this trial started (%s); its files may differ from the trial's.", image, created.Local().Format("2006-01-02 15:04"), started.Local().Format("2006-01-02 15:04"))
	}
	b, _ := json.Marshal(struct{ Note string }{note})
	if err := os.WriteFile(filepath.Join(out, ".done"), b, 0o644); err != nil {
		return "", "", err
	}
	return SourceImage, note, nil
}

func headLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// applyPatch puts a container's /work in the state the agent left it. The
// collect hook diffs against the Kavach integration commit in the Kavach
// arms and against the first commit in the others, so the patch applies on
// the image's HEAD; patches from other hooks apply on the first commit.
func applyPatch(d Docker, c string, patch []byte) error {
	if len(patch) == 0 {
		return nil
	}
	git := []string{"git", "-c", "safe.directory=/work", "-C", "/work"}
	run := func(stdin []byte, args ...string) error {
		_, err := d.Exec(c, stdin, append(slices.Clone(git), args...)...)
		return err
	}
	if err := run(nil, "reset", "-q", "--hard", "HEAD"); err != nil {
		return err
	}
	run(nil, "clean", "-fdq")
	err := run(patch, "apply", "-")
	if err == nil {
		return nil
	}
	root, rerr := d.Exec(c, nil, append(slices.Clone(git), "rev-list", "--max-parents=0", "HEAD")...)
	if rerr != nil {
		return err
	}
	if rerr := run(nil, "reset", "-q", "--hard", strings.TrimSpace(string(root))); rerr != nil {
		return err
	}
	return run(patch, "apply", "-")
}

func anyFile(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, _ error) error {
		found = found || (d != nil && !d.IsDir())
		return nil
	})
	return found
}

// copyOut writes the files of a container's /work under dir. A file the
// tree doesn't have, such as one the agent deleted, is skipped.
func copyOut(d Docker, c, dir string, files []string) error {
	for _, f := range files {
		b, err := d.Exec(c, nil, "cat", "--", "/work/"+f)
		if err != nil {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// trialImage names the image a trial's container was built from, which the
// benchmark keeps as kavach-bench/<task>:<arm>, and the command that builds it.
func (r *Reader) trialImage(tasksDir, job, id string) (image, build string, err error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return "", "", err
	}
	var cfg struct {
		Task struct {
			Path string `json:"path"`
		} `json:"task"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	for _, try := range []struct{ dir, name string }{
		{tasksDir, filepath.Base(cfg.Task.Path)},
		{filepath.Dir(cfg.Task.Path), filepath.Base(cfg.Task.Path)},
	} {
		env, err := TaskEnvironment(try.dir, try.name)
		if err != nil {
			continue
		}
		for _, f := range env.Files {
			if f.Role != "agent" || f.Dockerfile == "" || f.Target == "" {
				continue
			}
			base := filepath.Base(filepath.Dir(f.Dockerfile))
			image = fmt.Sprintf("kavach-bench/%s:%s", base, f.Target)
			return image, fmt.Sprintf("docker build -f %s --target %s -t %s .", relFrom(f.Dockerfile), f.Target, image), nil
		}
	}
	return "", "", errors.New("The task's compose file doesn't say which image the trial ran in.")
}

// relFrom shortens a Dockerfile's path to bench/agent/tasks/… when it is
// under a checkout of the repository, as the command runs from its root.
func relFrom(p string) string {
	if _, rest, ok := strings.Cut(filepath.ToSlash(p), "/bench/agent/"); ok {
		return "bench/agent/" + rest
	}
	return p
}

func (r *Reader) trialStart(job, id string) time.Time {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return time.Time{}
	}
	var res struct {
		StartedAt string `json:"started_at"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "result.json")); err == nil {
		json.Unmarshal(b, &res)
	}
	if t := parseTime(res.StartedAt); t != nil {
		return *t
	}
	return time.Time{}
}

// mapSnippets finds each snippet's lines in the cached files. The agent's
// patch is against the tree it started from, so its snippets' lines are the
// final ones; the integration patch's are shifted by the agent's hunks. A
// line that isn't what the snippet shows is searched for nearby, and a
// snippet whose line can't be found keeps to the patch.
func mapSnippets(inv *Invariants, root string, srcs []patchSource) {
	var agent []byte
	for _, s := range srcs {
		if s.agent {
			agent = s.text
		}
	}
	isAgent := func(file string) bool {
		return slices.ContainsFunc(srcs, func(s patchSource) bool { return s.path == file && s.agent })
	}
	cache := map[string][]string{}
	lines := func(p string) []string {
		if l, ok := cache[p]; ok {
			return l
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		var l []string
		if err == nil {
			l = strings.Split(string(b), "\n")
		}
		cache[p] = l
		return l
	}
	place := func(s *Snippet, name string) {
		if s == nil || !fs.ValidPath(s.Path) {
			return
		}
		real := lines(s.Path)
		if real == nil {
			return
		}
		// The line to show: the first naming the invariant, else the first.
		hit, want := s.Line, ""
		if s.Text != "" {
			ls := strings.Split(s.Text, "\n")
			k := slices.IndexFunc(ls, func(l string) bool { return strings.Contains(l, name) })
			hit, want = s.Line+max(k, 0), strings.TrimSpace(ls[max(k, 0)])
		}
		first := s.Line
		if !isAgent(s.File) {
			first, hit = shiftLine(agent, s.Path, s.Line), shiftLine(agent, s.Path, hit)
		}
		if hit == 0 {
			return
		}
		ok := func(n int) bool {
			if n < 1 || n > len(real) {
				return false
			}
			if want != "" {
				return strings.TrimSpace(real[n-1]) == want
			}
			return strings.Contains(real[n-1], name)
		}
		if !ok(hit) {
			found := false
			for d := 1; d <= 200 && !found; d++ {
				for _, n := range []int{hit - d, hit + d} {
					if ok(n) {
						first += n - hit
						hit, found = n, true
						break
					}
				}
			}
			if !found {
				return
			}
		}
		s.SrcFile = filepath.Join(root, filepath.FromSlash(s.Path))
		s.SrcLine, s.SrcHit = max(first, 1), hit
	}
	for i := range inv.Invariants {
		d := &inv.Invariants[i]
		place(d.DocAt, d.Name)
		for j := range d.Code {
			place(&d.Code[j], d.Name)
		}
	}
}

var hunkHead = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// shiftLine maps line n of path before patch to its line after, or 0 when
// the patch removes it.
func shiftLine(patch []byte, path string, n int) int {
	if len(patch) == 0 {
		return n
	}
	delta, inFile := 0, false
	oldN, newN := 0, 0
	for _, l := range strings.Split(string(patch), "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			inFile = false
		case strings.HasPrefix(l, "+++ "):
			inFile = strings.TrimPrefix(l, "+++ b/") == path
			delta = 0
		case !inFile:
		case strings.HasPrefix(l, "@@"):
			m := hunkHead.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, oc, nc := atoi(m[1]), 1, 1
			if m[2] != "" {
				oc = atoi(m[2])
			}
			if m[4] != "" {
				nc = atoi(m[4])
			}
			if oc == 0 {
				start++ // an insertion follows the line the header names
			}
			if n < start {
				return n + delta
			}
			if n >= start+oc {
				delta += nc - oc
				continue
			}
			oldN, newN = start, atoi(m[3])
		case strings.HasPrefix(l, "-"):
			if oldN == n {
				return 0
			}
			oldN++
		case strings.HasPrefix(l, "+"):
			newN++
		case strings.HasPrefix(l, "\\"):
		default:
			if oldN == n {
				return newN
			}
			oldN++
			newN++
		}
	}
	return n + delta
}

// RealFile returns a cached real source file of a trial: one a patch of the
// trial touches, which RealSource has fetched. Nothing else can be read
// through it.
func (r *Reader) RealFile(tasksDir, job, id, path string) ([]byte, error) {
	_, srcs, err := r.invariantSources(tasksDir, job, id)
	if err != nil {
		return nil, err
	}
	if r.CacheDir == "" || !fs.ValidPath(path) {
		return nil, ErrNotFound
	}
	known := false
	for _, s := range srcs {
		for _, f := range parsePatch(s.text, s.path, s.agent) {
			known = known || f.path == path
		}
	}
	if !known {
		return nil, ErrNotFound
	}
	dir := filepath.Join(r.CacheDir, job, id)
	cur, err := os.ReadFile(filepath.Join(dir, "current"))
	if err != nil || (string(cur) != SourceContainer && string(cur) != SourceImage) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(dir, string(cur), filepath.FromSlash(path)))
	if err != nil {
		return nil, ErrNotFound
	}
	return b, nil
}
