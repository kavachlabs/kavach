package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Invariants is what a trial's service declares as invariants, read from its
// source: the task's Kavach integration patch, which every Kavach arm's image
// applies, and the agent's own patch on top of it. Kavach itself knows an
// invariant only by its name; what it means is in the code that checks it.
type Invariants struct {
	Task       string         `json:"task"`           // the Harbor task
	Sources    []string       `json:"sources"`        // absolute paths of the patches read, task's first
	Invariants []InvariantDef `json:"invariants"`     // in declaration order
	Note       string         `json:"note,omitempty"` // why nothing was found

	// Source says where Snippet.SrcFile come from: "artifacts", "container",
	// "image" or "patch" when there are no real files. SourceNote says why
	// not, or warns that the files may differ from the trial's. Files are the
	// repo-relative paths of the real files fetched.
	Source     string   `json:"source"`
	SourceNote string   `json:"source_note,omitempty"`
	Files      []string `json:"files"`
}

// InvariantDef is one invariant: what the source says about it, where it is
// declared, the function that checks it and the places it fails.
type InvariantDef struct {
	Name string `json:"name"`
	// Doc is the comment or docstring that describes it, when the source has
	// one: a definition list entry ("cookie-scope   every cookie…") or a
	// comment that starts with its name ("// alert-cooldown: …").
	Doc   string    `json:"doc,omitempty"`
	DocAt *Snippet  `json:"doc_at,omitempty"`
	Code  []Snippet `json:"code"`
	// Agent is set when the agent's patch adds or changes a line that names
	// the invariant or describes it.
	Agent bool `json:"agent,omitempty"`
}

// Snippet is a few lines of a source file.
type Snippet struct {
	Role  string `json:"role"` // declaration, check or fails
	Path  string `json:"path"`
	Line  int    `json:"line"` // of Text's first line, in the patched file
	Text  string `json:"text"`
	Agent bool   `json:"agent,omitempty"` // from the agent's patch
	// File is the absolute path of the patch the snippet was read from, one
	// of Invariants.Sources. FileLine is the line of that patch that shows
	// Text's first line, and FileHit the line that shows the first of
	// Text's lines naming the invariant (FileLine when none does). Removed
	// lines can sit between a snippet's lines in the patch.
	File     string `json:"file,omitempty"`
	FileLine int    `json:"file_line,omitempty"`
	FileHit  int    `json:"file_hit,omitempty"`

	// SrcFile is the absolute path of the real file the snippet is in, cached
	// on the host, with SrcLine the line of it that Text starts at and SrcHit
	// the line naming the invariant. Set when Invariants.Source is not "patch".
	SrcFile string `json:"src_file,omitempty"`
	SrcLine int    `json:"src_line,omitempty"`
	SrcHit  int    `json:"src_hit,omitempty"`

	plines []int // the patch line of each line of Text
}

// srcLine is a line of a patched file as a patch shows it: added or context.
type srcLine struct {
	path  string
	n     int
	text  string
	added bool
	agent bool
	pline int // the line of the patch file that shows it
}

// hunkFile is the lines of one file in one patch, in order, with gaps where
// the patch shows nothing.
type hunkFile struct {
	path  string
	agent bool
	file  string // the patch it is read from
	lines []srcLine
}

// parsePatch reads a unified diff, the file at path, into the new side of
// each file it changes.
func parsePatch(b []byte, path string, agent bool) []*hunkFile {
	var files []*hunkFile
	var cur *hunkFile
	n := 0
	hunk := regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	for i, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		pl := i + 1
		switch {
		case strings.HasPrefix(l, "diff --git "):
			cur = nil
		case strings.HasPrefix(l, "+++ "):
			p := strings.TrimPrefix(l, "+++ ")
			if p == "/dev/null" {
				cur = nil
				continue
			}
			cur = &hunkFile{path: strings.TrimPrefix(p, "b/"), agent: agent, file: path}
			files = append(files, cur)
		case cur == nil:
		case strings.HasPrefix(l, "@@"):
			if m := hunk.FindStringSubmatch(l); m != nil {
				n = atoi(m[1])
			}
		case strings.HasPrefix(l, "+"):
			cur.lines = append(cur.lines, srcLine{cur.path, n, l[1:], true, agent, pl})
			n++
		case strings.HasPrefix(l, " ") || l == "":
			cur.lines = append(cur.lines, srcLine{cur.path, n, strings.TrimPrefix(l, " "), false, agent, pl})
			n++
		}
	}
	return files
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

var (
	kebab     = `[a-z0-9]+(?:-[a-z0-9]+)+`
	quotedInv = regexp.MustCompile(`"(` + kebab + `)"`)
	constInv  = regexp.MustCompile(`\b(\w*[iI]nv\w*)\s*=\s*"(` + kebab + `)"`)
	termStart = regexp.MustCompile(`^(` + kebab + `)(:|\s{2,}|\s[-—]\s)`)
	// A name alone on its line, as an entry of a list of invariants:
	// for n in (\n # what it means\n "x",\n ...).
	bareInv = regexp.MustCompile(`^\s*"(` + kebab + `)",?\s*$`)
	// The function an invariant's declaration passes as its check:
	// Invariant("x", self._limits), Invariant("x", per_step("x")),
	// {"x", trimInvariant, NULL}, lambda: self._check("x").
	checkRef = regexp.MustCompile(`"` + kebab + `"\s*,\s*(?:lambda[^:]*:\s*)?(?:self\.|a\.|d\.)?([A-Za-z_]\w*)`)
)

// TrialInvariants reads the invariants a trial's service declares. names are
// invariants known from its contract checks; the source is searched for them
// and for any others it declares.
func (r *Reader) TrialInvariants(tasksDir, job, id string, names []string) (*Invariants, error) {
	task, srcs, err := r.invariantSources(tasksDir, job, id)
	if err != nil {
		return nil, err
	}
	out := &Invariants{Task: task, Sources: []string{}, Invariants: []InvariantDef{}}
	var files []*hunkFile
	for _, s := range srcs {
		files = append(files, parsePatch(s.text, s.path, s.agent)...)
		out.Sources = append(out.Sources, s.path)
	}
	if defs := findInvariants(files, names); defs != nil {
		out.Invariants = defs
	}
	if len(out.Sources) == 0 {
		out.Note = "Neither the task's Kavach integration patch nor the agent's patch was found."
	} else if len(out.Invariants) == 0 {
		out.Note = "The source read declares no invariants."
	}
	return out, nil
}

// patchSource is a patch TrialInvariants reads.
type patchSource struct {
	path  string // absolute
	text  []byte
	agent bool
}

// invariantSources reads the patches a trial's invariants are read from:
// the task's Kavach integration patch, then the agent's patch, each when it
// exists and is not empty.
func (r *Reader) invariantSources(tasksDir, job, id string) (task string, srcs []patchSource, err error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return "", nil, err
	}
	var cfg struct {
		Task struct {
			Path string `json:"path"`
		} `json:"task"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if patch := integrationPatch(tasksDir, cfg.Task.Path); patch != "" {
		if b, err := os.ReadFile(patch); err == nil {
			if abs, err := filepath.Abs(patch); err == nil {
				if !strings.HasSuffix(patch, ".patch") {
					b = newFilePatch(filepath.Base(patch), b)
				}
				srcs = append(srcs, patchSource{abs, b, false})
			}
		}
	}
	if b, err := r.Patch(job, id); err == nil && len(b) > 0 {
		abs, err := filepath.Abs(filepath.Join(dir, "artifacts", "logs", "artifacts", "patch.diff"))
		if err != nil {
			return "", nil, err
		}
		srcs = append(srcs, patchSource{abs, b, true})
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return "", nil, err
	}
	return filepath.Base(cfg.Task.Path), srcs, nil
}

// InvariantSource returns the text of the i'th of the patches
// TrialInvariants lists as a trial's sources. Nothing else can be read
// through it.
func (r *Reader) InvariantSource(tasksDir, job, id string, i int) ([]byte, error) {
	_, srcs, err := r.invariantSources(tasksDir, job, id)
	if err != nil {
		return nil, err
	}
	if i < 0 || i >= len(srcs) {
		return nil, ErrNotFound
	}
	return srcs[i].text, nil
}

// integrationPatch finds the patch that adds Kavach to a task's service: the
// *kavach*.patch next to the Dockerfile its compose file builds. A task
// whose image copies in a whole driver instead (kavach_kombu.py) has no
// patch; the driver is returned, and read as a patch that adds it.
func integrationPatch(tasksDir, taskPath string) string {
	for _, try := range []struct{ dir, name string }{
		{tasksDir, filepath.Base(taskPath)},
		{filepath.Dir(taskPath), filepath.Base(taskPath)},
	} {
		env, err := TaskEnvironment(try.dir, try.name)
		if err != nil {
			continue
		}
		for _, f := range env.Files {
			if f.Role != "dockerfile" || f.Missing {
				continue
			}
			m, _ := filepath.Glob(filepath.Join(filepath.Dir(f.Path), "*kavach*.patch"))
			if len(m) > 0 {
				return m[0]
			}
			m, _ = filepath.Glob(filepath.Join(filepath.Dir(f.Path), "kavach_*"))
			for _, p := range m {
				if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && !strings.HasSuffix(p, ".kavach") {
					return p
				}
			}
		}
	}
	return ""
}

// inInvariantList says whether line i of f follows, within 30 lines, code
// that names invariants (def invariants(self):), as the entries of a list
// of their names do.
func inInvariantList(f *hunkFile, i int) bool {
	for j := i - 1; j >= 0 && j >= i-30; j-- {
		l := f.lines[j]
		if l.n != f.lines[j+1].n-1 {
			return false // a gap in the patch: the code between is unknown
		}
		if !isComment(l.text) && strings.Contains(strings.ToLower(l.text), "invariant") {
			return true
		}
	}
	return false
}

// commentAbove is the comment block that ends on the line above line i of
// f, as one paragraph, and its first line.
func commentAbove(f *hunkFile, i int) (string, srcLine) {
	var parts []string
	var first srcLine
	for j := i - 1; j >= 0 && j >= i-16; j-- {
		l := f.lines[j]
		if l.n != f.lines[j+1].n-1 || !isComment(l.text) {
			break
		}
		_, b := commentBody(l.text)
		if b = strings.TrimSpace(b); b == "" {
			break
		}
		parts = append([]string{b}, parts...)
		first = l
	}
	return strings.Join(parts, " "), first
}

// newFilePatch is a unified diff that adds the file name with text b.
func newFilePatch(name string, b []byte) []byte {
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	var out strings.Builder
	fmt.Fprintf(&out, "diff --git a/%s b/%[1]s\nnew file mode 100644\n--- /dev/null\n+++ b/%[1]s\n@@ -0,0 +1,%d @@\n", name, len(lines))
	for _, l := range lines {
		out.WriteString("+" + l + "\n")
	}
	return []byte(out.String())
}

func findInvariants(files []*hunkFile, known []string) []InvariantDef {
	// Names: those passed in, then those the source declares, in order.
	names := slices.Clone(known)
	consts := map[string]string{} // name -> the identifier that holds it
	add := func(n string) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	for _, f := range files {
		for i, l := range f.lines {
			if m := bareInv.FindStringSubmatch(l.text); m != nil && inInvariantList(f, i) {
				add(m[1])
				continue
			}
			if m := constInv.FindStringSubmatch(l.text); m != nil {
				consts[m[2]] = m[1]
				add(m[2])
				continue
			}
			if strings.Contains(strings.ToLower(l.text), "invariant") && !isComment(l.text) {
				for _, m := range quotedInv.FindAllStringSubmatch(l.text, -1) {
					add(m[1])
				}
			}
		}
	}
	var defs []InvariantDef
	for _, name := range names {
		d := InvariantDef{Name: name, Code: []Snippet{}}
		refs := []string{`"` + name + `"`}
		if c := consts[name]; c != "" {
			refs = append(refs, c)
		}
		var decl *srcLine
		var declFile *hunkFile
		var fails []Snippet
		for _, f := range files {
			for i := range f.lines {
				l := &f.lines[i]
				if !containsAny(l.text, refs) {
					if d.Doc == "" && strings.Contains(l.text, name) {
						if doc := docAt(f, i, name); doc != "" {
							d.Doc = doc
							d.DocAt = &Snippet{Role: "doc", Path: l.path, Line: l.n, Agent: l.agent && l.added, File: f.file, FileLine: l.pline, FileHit: l.pline}
							d.Agent = d.Agent || l.agent && l.added
						}
					}
					continue
				}
				if l.agent && l.added {
					d.Agent = true
				}
				lower := strings.ToLower(l.text)
				switch {
				case decl == nil && (strings.Contains(lower, "invariant") || constInv.MatchString(l.text)) && !isComment(l.text),
					decl == nil && bareInv.MatchString(l.text) && inInvariantList(f, i):
					decl, declFile = l, f
					if d.Doc == "" {
						if doc, at := commentAbove(f, i); doc != "" {
							d.Doc = doc
							d.DocAt = &Snippet{Role: "doc", Path: at.path, Line: at.n, Agent: at.agent && at.added, File: f.file, FileLine: at.pline, FileHit: at.pline}
							d.Agent = d.Agent || at.agent && at.added
						}
					}
				case len(fails) < 4 && !isComment(l.text):
					fails = append(fails, around(f, i, 1, 1, "fails"))
				}
			}
		}
		if decl == nil && len(fails) == 0 && d.Doc == "" {
			continue // a name from a report that the source never mentions
		}
		if decl != nil {
			i := slices.IndexFunc(declFile.lines, func(l srcLine) bool { return l.n == decl.n && l.text == decl.text })
			d.Code = append(d.Code, around(declFile, i, 1, 1, "declaration"))
			if m := checkRef.FindStringSubmatch(decl.text); m != nil {
				if s, ok := funcBody(files, m[1]); ok {
					d.Code = append(d.Code, s)
				}
			}
		}
		d.Code = append(d.Code, fails...)
		for i := range d.Code {
			d.Code[i].FileHit = d.Code[i].FileLine
			for k, l := range strings.Split(d.Code[i].Text, "\n") {
				if strings.Contains(l, name) && k < len(d.Code[i].plines) {
					d.Code[i].FileHit = d.Code[i].plines[k]
					break
				}
			}
		}
		defs = append(defs, d)
	}
	return defs
}

// containsAny reports whether s holds one of refs: a quoted name, or the
// identifier of a constant that holds it.
func containsAny(s string, refs []string) bool {
	for _, ref := range refs {
		if strings.HasPrefix(ref, `"`) {
			if strings.Contains(s, ref) {
				return true
			}
		} else if regexp.MustCompile(`\b` + regexp.QuoteMeta(ref) + `\b`).MatchString(s) {
			return true
		}
	}
	return false
}

func isComment(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*")
}

// commentBody strips a line's comment marker and returns the marker and the
// text after it.
func commentBody(s string) (marker, body string) {
	t := strings.TrimLeft(s, " \t")
	for _, m := range []string{"//", "#", "/*", "*"} {
		if strings.HasPrefix(t, m) {
			return m, strings.TrimPrefix(t, m)
		}
	}
	return "", s
}

// docAt reads the description of name that starts at line i, if the line
// is a definition of it: in a docstring ("  name   what it is", or the
// name alone with what it is indented below, continued by lines indented
// past the name) or in a comment ("// name: what it is", continued by the
// comment's next lines).
func docAt(f *hunkFile, i int, name string) string {
	marker, body := commentBody(f.lines[i].text)
	trimmed := strings.TrimLeft(body, " \t")
	col := len(body) - len(trimmed)
	var parts []string
	if strings.TrimRight(trimmed, " \t:") == name {
		// A term on a line of its own, its definition indented below it.
	} else if m := termStart.FindStringSubmatch(trimmed); m != nil && m[1] == name {
		parts = append(parts, strings.TrimSpace(strings.TrimPrefix(trimmed[len(m[1]):], ":")))
	} else {
		return ""
	}
	for j := i + 1; j < len(f.lines) && j < i+16; j++ {
		if f.lines[j].n != f.lines[j-1].n+1 {
			break
		}
		mk, b := commentBody(f.lines[j].text)
		t := strings.TrimLeft(b, " \t")
		c := len(b) - len(t)
		if mk != marker || t == "" || strings.HasPrefix(t, `"""`) || termStart.MatchString(t) {
			break
		}
		if marker == "" && c <= col || marker != "" && c < col {
			break
		}
		parts = append(parts, strings.TrimSpace(t))
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// around returns lines i-before … i+after of f, within the run of
// consecutive lines i belongs to.
func around(f *hunkFile, i, before, after int, role string) Snippet {
	lo, hi := i, i
	for lo > 0 && i-lo < before && f.lines[lo-1].n == f.lines[lo].n-1 {
		lo--
	}
	for hi+1 < len(f.lines) && hi-i < after && f.lines[hi+1].n == f.lines[hi].n+1 {
		hi++
	}
	return snippet(f, lo, hi, role)
}

func snippet(f *hunkFile, lo, hi int, role string) Snippet {
	var b strings.Builder
	agent := false
	var plines []int
	for k := lo; k <= hi; k++ {
		b.WriteString(f.lines[k].text)
		if k < hi {
			b.WriteByte('\n')
		}
		agent = agent || f.lines[k].agent && f.lines[k].added
		plines = append(plines, f.lines[k].pline)
	}
	return Snippet{Role: role, Path: f.path, Line: f.lines[lo].n, Text: b.String(), Agent: agent,
		File: f.file, FileLine: f.lines[lo].pline, plines: plines}
}

// maxCheckLines bounds a check function's snippet.
const maxCheckLines = 40

// funcBody finds the function named name and returns it: a Python def to
// where its indentation ends, or a brace-delimited function to its closing
// brace.
func funcBody(files []*hunkFile, name string) (Snippet, bool) {
	py := regexp.MustCompile(`^(\s*)def ` + regexp.QuoteMeta(name) + `\b`)
	brace := regexp.MustCompile(`^\s*(?:func\s+(?:\([^)]*\)\s*)?|[\w\s\*]+\s\**)` + regexp.QuoteMeta(name) + `\s*\(`)
	for _, f := range files {
		for i, l := range f.lines {
			if m := py.FindStringSubmatch(l.text); m != nil {
				indent := len(m[1])
				j := i
				for j+1 < len(f.lines) && j-i < maxCheckLines && f.lines[j+1].n == f.lines[j].n+1 {
					t := f.lines[j+1].text
					if strings.TrimSpace(t) != "" && len(t)-len(strings.TrimLeft(t, " \t")) <= indent {
						break
					}
					j++
				}
				for j > i && strings.TrimSpace(f.lines[j].text) == "" {
					j--
				}
				return snippet(f, i, j, "check"), true
			}
			if brace.MatchString(l.text) && !strings.HasSuffix(strings.TrimSpace(l.text), ";") {
				depth, j, opened := 0, i, false
				for ; j < len(f.lines) && j-i < maxCheckLines; j++ {
					if j > i && f.lines[j].n != f.lines[j-1].n+1 {
						j--
						break
					}
					depth += strings.Count(f.lines[j].text, "{") - strings.Count(f.lines[j].text, "}")
					opened = opened || strings.Contains(f.lines[j].text, "{")
					if opened && depth <= 0 {
						break
					}
				}
				if j >= len(f.lines) {
					j = len(f.lines) - 1
				}
				return snippet(f, i, j, "check"), true
			}
		}
	}
	return Snippet{}, false
}
