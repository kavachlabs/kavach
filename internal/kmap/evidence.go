package kmap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Limits on what source and patch evidence may add to a Model.
const (
	// ExcerptContext is how many lines are shown on each side of a code ref.
	ExcerptContext = 3
	// ExcerptMaxLines caps the lines kept per code ref, context included.
	ExcerptMaxLines = 60
	// ExcerptMaxBytes caps the bytes of source text across all excerpts.
	ExcerptMaxBytes = 256 * 1024
	// HunkMaxLines caps the body lines kept per patch hunk.
	HunkMaxLines = 120
)

// SourceRoot is a local checkout that node code refs resolve against. An
// empty Group applies to every group.
type SourceRoot struct {
	Group string
	Dir   string
}

// ParseSourceRoots turns --source values into roots. A value is GROUP=DIR
// when the text before the first "=" names a group in spec; otherwise the
// whole value is a directory for every group. Each directory must exist.
func ParseSourceRoots(values []string, spec *Spec) ([]SourceRoot, error) {
	groups := map[string]bool{}
	if spec != nil {
		for _, g := range spec.Groups {
			groups[g.ID] = true
		}
	}
	var out []SourceRoot
	for _, v := range values {
		r := SourceRoot{Dir: v}
		if i := strings.IndexByte(v, '='); i > 0 && groups[v[:i]] {
			r = SourceRoot{Group: v[:i], Dir: v[i+1:]}
		}
		st, err := os.Stat(r.Dir)
		if err != nil || !st.IsDir() {
			if i := strings.IndexByte(v, '='); i > 0 && r.Group == "" {
				return nil, fmt.Errorf("--source %s: %q is not a group in the spec and %q is not a directory", v, v[:i], v)
			}
			return nil, fmt.Errorf("--source %s: %q is not a directory", v, r.Dir)
		}
		out = append(out, r)
	}
	return out, nil
}

// Excerpt is the source text around one code ref.
type Excerpt struct {
	Ref  string `json:"ref"`
	Path string `json:"path"`
	// Root is the group of the --source root the file was found under
	// ("" for a root given for every group).
	Root      string   `json:"root,omitempty"`
	Start     int      `json:"start"`
	HLStart   int      `json:"hl_start"`
	HLEnd     int      `json:"hl_end"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated,omitempty"`
	// Hunks index Model.Hunks: the hunks that touch this ref.
	Hunks []int `json:"hunks,omitempty"`
}

// Hunk is one hunk of a unified diff.
type Hunk struct {
	Patch    string   `json:"patch"`
	File     string   `json:"file"`
	OldStart int      `json:"old_start"`
	OldLines int      `json:"old_lines"`
	NewStart int      `json:"new_start"`
	NewLines int      `json:"new_lines"`
	Section  string   `json:"section,omitempty"`
	Lines    []string `json:"lines"`
	// Truncated is set when the body was cut at HunkMaxLines.
	Truncated bool `json:"truncated,omitempty"`
}

// NodeEvidence is the source and patch evidence attached to one node.
type NodeEvidence struct {
	Excerpts []Excerpt `json:"excerpts,omitempty"`
	// Hunks index Model.Hunks.
	Hunks []int `json:"hunks,omitempty"`
}

type codeRef struct {
	raw  string
	path string
	a, b int
}

func parseCodeRef(ref string) (codeRef, bool) {
	p, la, lb := splitCodeRef(ref)
	if la == "" {
		return codeRef{}, false
	}
	a, err := strconv.Atoi(la)
	if err != nil || a < 1 {
		return codeRef{}, false
	}
	b := a
	if lb != "" {
		if b, err = strconv.Atoi(lb); err != nil || b < a {
			return codeRef{}, false
		}
	}
	return codeRef{raw: ref, path: p, a: a, b: b}, true
}

// cleanRelPath cleans a slash path and rejects anything that could leave a
// root: absolute paths, volume names and ".." components.
func cleanRelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") || filepath.VolumeName(p) != "" || (len(p) > 1 && p[1] == ':') {
		return "", fmt.Errorf("path %q is absolute", p)
	}
	c := path.Clean(p)
	if c == "." {
		return "", fmt.Errorf("path %q is empty", p)
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q leaves the source root", p)
		}
	}
	return c, nil
}

type rootDir struct {
	group string
	abs   string // symlinks resolved
	base  string
}

type sourceReader struct {
	roots    []rootDir
	files    map[string][]string // abs path -> lines
	bytes    int
	capHit   bool
	warnings []string
}

func newSourceReader(roots []SourceRoot) (*sourceReader, error) {
	sr := &sourceReader{files: map[string][]string{}}
	for _, r := range roots {
		abs, err := filepath.Abs(r.Dir)
		if err != nil {
			return nil, err
		}
		if ev, err := filepath.EvalSymlinks(abs); err == nil {
			abs = ev
		}
		sr.roots = append(sr.roots, rootDir{group: r.Group, abs: abs, base: filepath.Base(abs)})
	}
	return sr, nil
}

// candidates orders roots for a group: the group's own, then unscoped ones,
// then the rest.
func (sr *sourceReader) candidates(group string) []rootDir {
	var own, all, rest []rootDir
	for _, r := range sr.roots {
		switch r.group {
		case group:
			own = append(own, r)
		case "":
			all = append(all, r)
		default:
			rest = append(rest, r)
		}
	}
	return append(append(own, all...), rest...)
}

// resolve finds rel under one of the roots. A leading segment equal to the
// root's own directory name is also tried stripped, so "upstream/x.go"
// resolves under a root named upstream. It returns the absolute file and the
// path relative to its root.
func (sr *sourceReader) resolve(group, rel string) (string, string, string, bool) {
	for _, r := range sr.candidates(group) {
		tries := []string{rel}
		if strings.HasPrefix(rel, r.base+"/") {
			tries = append(tries, strings.TrimPrefix(rel, r.base+"/"))
		}
		for _, t := range tries {
			full := filepath.Join(r.abs, filepath.FromSlash(t))
			ev, err := filepath.EvalSymlinks(full)
			if err != nil {
				continue
			}
			if !within(r.abs, ev) {
				continue
			}
			if st, err := os.Stat(ev); err != nil || !st.Mode().IsRegular() {
				continue
			}
			return ev, t, r.group, true
		}
	}
	return "", "", "", false
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (sr *sourceReader) lines(abs string) ([]string, error) {
	if ls, ok := sr.files[abs]; ok {
		return ls, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("binary file")
	}
	var ls []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ls = append(ls, strings.TrimRight(sc.Text(), "\r"))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sr.files[abs] = ls
	return ls, nil
}

func (sr *sourceReader) warnf(format string, args ...any) {
	sr.warnings = append(sr.warnings, fmt.Sprintf(format, args...))
}

// excerpt reads one ref. It returns the excerpt and its path relative to
// the root it was found under.
func (sr *sourceReader) excerpt(nodeID, group string, cr codeRef) (*Excerpt, string) {
	rel, err := cleanRelPath(cr.path)
	if err != nil {
		sr.warnf("source: node %q ref %q: %v", nodeID, cr.raw, err)
		return nil, ""
	}
	abs, relToRoot, rootGroup, ok := sr.resolve(group, rel)
	if !ok {
		sr.warnf("source: node %q ref %q: %s not found under any --source root", nodeID, cr.raw, rel)
		return nil, ""
	}
	ls, err := sr.lines(abs)
	if err != nil {
		sr.warnf("source: node %q ref %q: %v", nodeID, cr.raw, err)
		return nil, relToRoot
	}
	if cr.a > len(ls) {
		sr.warnf("source: node %q ref %q: %s has only %d lines", nodeID, cr.raw, relToRoot, len(ls))
		return nil, relToRoot
	}
	hlEnd := cr.b
	if hlEnd > len(ls) {
		hlEnd = len(ls)
	}
	start := cr.a - ExcerptContext
	if start < 1 {
		start = 1
	}
	end := hlEnd + ExcerptContext
	if end > len(ls) {
		end = len(ls)
	}
	truncated := false
	if end-start+1 > ExcerptMaxLines {
		end = start + ExcerptMaxLines - 1
		truncated = true
	}
	out := append([]string(nil), ls[start-1:end]...)
	size := 0
	for _, l := range out {
		size += len(l) + 1
	}
	if sr.bytes+size > ExcerptMaxBytes {
		if !sr.capHit {
			sr.warnf("source: excerpts reached the %d KB cap; later refs are shown without source", ExcerptMaxBytes/1024)
			sr.capHit = true
		}
		return nil, relToRoot
	}
	sr.bytes += size
	return &Excerpt{
		Ref:       cr.raw,
		Path:      relToRoot,
		Root:      rootGroup,
		Start:     start,
		HLStart:   cr.a,
		HLEnd:     hlEnd,
		Lines:     out,
		Truncated: truncated,
	}, relToRoot
}

// ParsePatch parses a unified diff into hunks. name labels them (usually the
// patch file's base name).
func ParsePatch(name string, data []byte) ([]Hunk, error) {
	var hunks []Hunk
	var oldFile, newFile string
	var cur *Hunk
	var oldLeft, newLeft int
	flush := func() {
		if cur != nil {
			hunks = append(hunks, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimRight(sc.Text(), "\r")
		if cur != nil && (oldLeft > 0 || newLeft > 0) {
			if line == "" {
				line = " "
			}
			switch line[0] {
			case ' ':
				oldLeft--
				newLeft--
			case '-':
				oldLeft--
			case '+':
				newLeft--
			case '\\':
			default:
				return nil, fmt.Errorf("%s:%d: unexpected line inside hunk", name, ln)
			}
			if oldLeft < 0 || newLeft < 0 {
				return nil, fmt.Errorf("%s:%d: hunk longer than its header says", name, ln)
			}
			if len(cur.Lines) < HunkMaxLines {
				cur.Lines = append(cur.Lines, line)
			} else {
				cur.Truncated = true
			}
			continue
		}
		if cur != nil && strings.HasPrefix(line, `\`) {
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff "):
			flush()
			oldFile, newFile = "", ""
		case strings.HasPrefix(line, "--- "):
			flush()
			oldFile = patchPath(line[4:], "a/")
		case strings.HasPrefix(line, "+++ "):
			flush()
			newFile = patchPath(line[4:], "b/")
		case strings.HasPrefix(line, "@@ "):
			flush()
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", name, ln, err)
			}
			file := newFile
			if file == "" {
				file = oldFile
			}
			if file == "" {
				return nil, fmt.Errorf("%s:%d: hunk before any file header", name, ln)
			}
			h.Patch = name
			h.File = file
			cur = &h
			oldLeft, newLeft = h.OldLines, h.NewLines
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cur != nil && (oldLeft > 0 || newLeft > 0) {
		return nil, fmt.Errorf("%s: last hunk is shorter than its header says", name)
	}
	flush()
	return hunks, nil
}

// patchPath strips a "---"/"+++" line to its path: drops a tab-separated
// timestamp, the a/ or b/ prefix, and maps /dev/null to "".
func patchPath(s, prefix string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			s = u
		}
	}
	if s == "/dev/null" {
		return ""
	}
	s = strings.TrimPrefix(s, prefix)
	return path.Clean(strings.TrimPrefix(s, "./"))
}

func parseHunkHeader(line string) (Hunk, error) {
	rest := strings.TrimPrefix(line, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return Hunk{}, fmt.Errorf("bad hunk header %q", line)
	}
	section := strings.TrimSpace(rest[end+3:])
	f := strings.Fields(rest[:end])
	if len(f) != 2 || !strings.HasPrefix(f[0], "-") || !strings.HasPrefix(f[1], "+") {
		return Hunk{}, fmt.Errorf("bad hunk header %q", line)
	}
	oStart, oLines, err := parseRange(f[0][1:])
	if err != nil {
		return Hunk{}, fmt.Errorf("bad hunk header %q: %w", line, err)
	}
	ns, nl, err := parseRange(f[1][1:])
	if err != nil {
		return Hunk{}, fmt.Errorf("bad hunk header %q: %w", line, err)
	}
	return Hunk{OldStart: oStart, OldLines: oLines, NewStart: ns, NewLines: nl, Section: section}, nil
}

func parseRange(s string) (int, int, error) {
	a, b, found := strings.Cut(s, ",")
	start, err := strconv.Atoi(a)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("bad range %q", s)
	}
	n := 1
	if found {
		if n, err = strconv.Atoi(b); err != nil || n < 0 {
			return 0, 0, fmt.Errorf("bad range %q", s)
		}
	}
	return start, n, nil
}

// samePath reports whether two slash paths name the same file, allowing
// either to carry extra leading directories.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

// overlaps reports whether the hunk's old-side lines meet [a, b]. A pure
// insertion (old_lines 0) sits after OldStart.
func (h Hunk) overlaps(a, b int) bool {
	lo, hi := h.OldStart, h.OldStart+h.OldLines-1
	if h.OldLines == 0 {
		lo, hi = h.OldStart, h.OldStart+1
	}
	return lo <= b && a <= hi
}

// attachEvidence reads excerpts and matches patch hunks for every node with
// line refs. It returns nil maps when there is nothing to attach.
func attachEvidence(nodes []Node, roots []SourceRoot, patches []string) (map[string]*NodeEvidence, []Hunk, []string, error) {
	if len(roots) == 0 && len(patches) == 0 {
		return nil, nil, nil, nil
	}
	var hunks []Hunk
	for _, p := range patches {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("patch: %w", err)
		}
		hs, err := ParsePatch(filepath.Base(p), data)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("patch: %w", err)
		}
		hunks = append(hunks, hs...)
	}
	sr, err := newSourceReader(roots)
	if err != nil {
		return nil, nil, nil, err
	}
	ev := map[string]*NodeEvidence{}
	used := map[int]bool{}
	for _, n := range nodes {
		var ne NodeEvidence
		seenHunk := map[int]bool{}
		for _, ref := range n.Code {
			cr, ok := parseCodeRef(ref)
			if !ok {
				continue
			}
			var rel string
			var ex *Excerpt
			if len(roots) > 0 {
				ex, rel = sr.excerpt(n.ID, n.Group, cr)
			}
			refPath, _ := cleanRelPath(cr.path)
			for hi, h := range hunks {
				if !h.overlaps(cr.a, cr.b) || !(samePath(h.File, rel) || samePath(h.File, refPath)) {
					continue
				}
				if ex != nil {
					ex.Hunks = append(ex.Hunks, hi)
				}
				if !seenHunk[hi] {
					seenHunk[hi] = true
					ne.Hunks = append(ne.Hunks, hi)
				}
			}
			if ex != nil {
				ne.Excerpts = append(ne.Excerpts, *ex)
			}
		}
		if len(ne.Excerpts) == 0 && len(ne.Hunks) == 0 {
			continue
		}
		sort.Ints(ne.Hunks)
		for _, hi := range ne.Hunks {
			used[hi] = true
		}
		cp := ne
		ev[n.ID] = &cp
	}
	warnings := sr.warnings
	for hi, h := range hunks {
		if !used[hi] {
			warnings = append(warnings, fmt.Sprintf("patch %s: hunk %s @@ -%d,%d matched no node code ref", h.Patch, h.File, h.OldStart, h.OldLines))
		}
	}
	if len(ev) == 0 {
		ev = nil
	}
	if len(hunks) == 0 {
		hunks = nil
	}
	return ev, hunks, warnings, nil
}
