// Package kmap builds and renders interactive data-flow, call-flow, and
// invariant maps from a kavach map spec, recorded/replayed fixtures, diff
// reports, and generic stage overlays.
package kmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Spec is a decoded map.json specification.
type Spec struct {
	Version int     `json:"kavach_map"`
	Title   string  `json:"title"`
	Summary string  `json:"summary,omitempty"`
	Service string  `json:"service,omitempty"`
	Groups  []Group `json:"groups"`
	Nodes   []Node  `json:"nodes"`
	Edges   []Edge  `json:"edges"`
	Flows   []Flow  `json:"flows,omitempty"`

	// Money names the output/input fields that carry amounts so the replay
	// deck can show them as currency. Optional.
	Money *Money `json:"money,omitempty"`
	// StepNames gives steps short names, keyed by fixture (base name without
	// .kavach or .tokenized) and then by seq. Optional.
	StepNames map[string]map[string]string `json:"step_names,omitempty"`
}

// Money describes which payload fields are money and how to show them.
// Values are integers in minor units (2000 with minor_units 2 is 20.00).
type Money struct {
	Fields     []string `json:"fields"`
	MinorUnits int      `json:"minor_units,omitempty"`
	Currency   string   `json:"currency,omitempty"`
}

// Group is a visual column / layer of cards on the map.
type Group struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Plain    string `json:"plain,omitempty"`
	CodeBase string `json:"code_base,omitempty"`
}

// Node is one card on the map (entry op, function, state, sink, invariant, bug, etc.).
type Node struct {
	ID        string     `json:"id"`
	Group     string     `json:"group"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Plain     string     `json:"plain,omitempty"`
	Detail    string     `json:"detail,omitempty"`
	Code      []string   `json:"code,omitempty"`
	Invariant string     `json:"invariant,omitempty"`
	Match     []Rule     `json:"match,omitempty"`
	Links     []CodeLink `json:"links,omitempty"`
	Auto      bool       `json:"auto,omitempty"`
}

// CodeLink is a formatted source reference with an optional clickable URL.
type CodeLink struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

// Edge is a directed connection between two nodes.
type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind,omitempty"`
	Label string `json:"label,omitempty"`
	Auto  bool   `json:"auto,omitempty"`
	Count int    `json:"count,omitempty"`
}

// Flow is a guided step-by-step walkthrough across a sequence of nodes.
type Flow struct {
	ID      string     `json:"id"`
	Title   string     `json:"title"`
	Plain   string     `json:"plain,omitempty"`
	Fixture string     `json:"fixture,omitempty"`
	Steps   []FlowStep `json:"steps"`
}

// FlowStep is one step in a guided flow.
type FlowStep struct {
	Node string `json:"node"`
	Text string `json:"text"`
}

// Rule matches a node against a step's input, output, or failure.
// Fields inside one Rule are ANDed; multiple Rules on a Node are ORed.
type Rule struct {
	On       string         `json:"on,omitempty"`
	Source   string         `json:"source,omitempty"`
	Position string         `json:"position,omitempty"`
	Sink     string         `json:"sink,omitempty"`
	Kind     string         `json:"kind,omitempty"`
	JSON     map[string]any `json:"json,omitempty"`
	Contains string         `json:"contains,omitempty"`
	Regex    string         `json:"regex,omitempty"`

	re *regexp.Regexp
}

var validNodeKinds = map[string]bool{
	"entry":     true,
	"function":  true,
	"state":     true,
	"sink":      true,
	"invariant": true,
	"bug":       true,
	"external":  true,
	"note":      true,
}

var validEdgeKinds = map[string]bool{
	"calls":  true,
	"reads":  true,
	"writes": true,
	"emits":  true,
	"checks": true,
	"breaks": true,
	"flows":  true,
}

// LoadSpec reads, decodes (strictly), normalizes, and validates a map spec JSON file.
func LoadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("decode %s: unexpected trailing JSON data", path)
	}
	if err := s.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// NormalizeAndValidate fills defaults, builds code links, compiles rule regexes,
// and returns all validation issues joined via errors.Join.
func (s *Spec) NormalizeAndValidate() error {
	var errs []error

	if s.Version != 1 {
		errs = append(errs, fmt.Errorf("kavach_map must be 1, got %d", s.Version))
	}

	groupByID := make(map[string]Group, len(s.Groups))
	for i := range s.Groups {
		g := &s.Groups[i]
		if g.ID == "" {
			errs = append(errs, fmt.Errorf("group[%d]: id is required", i))
			continue
		}
		if _, dup := groupByID[g.ID]; dup {
			errs = append(errs, fmt.Errorf("duplicate group id %q", g.ID))
			continue
		}
		if g.CodeBase != "" && !strings.HasSuffix(g.CodeBase, "/") {
			g.CodeBase += "/"
		}
		groupByID[g.ID] = *g
	}

	nodeIDs := make(map[string]bool, len(s.Nodes))
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if n.ID == "" {
			errs = append(errs, fmt.Errorf("node[%d]: id is required", i))
		} else if nodeIDs[n.ID] {
			errs = append(errs, fmt.Errorf("duplicate node id %q", n.ID))
		} else {
			nodeIDs[n.ID] = true
		}

		if n.Group == "" {
			errs = append(errs, fmt.Errorf("node %q: group is required", n.ID))
		} else if _, ok := groupByID[n.Group]; !ok {
			errs = append(errs, fmt.Errorf("node %q: unknown group %q", n.ID, n.Group))
		}

		if !validNodeKinds[n.Kind] {
			errs = append(errs, fmt.Errorf("node %q: unknown kind %q", n.ID, n.Kind))
		}
		if n.Kind == "invariant" && n.Invariant == "" {
			errs = append(errs, fmt.Errorf("node %q: invariant name is required when kind is \"invariant\"", n.ID))
		}

		codeBase := groupByID[n.Group].CodeBase
		n.Links = buildCodeLinks(n.Code, codeBase)

		for j := range n.Match {
			r := &n.Match[j]
			if err := r.prepare(); err != nil {
				errs = append(errs, fmt.Errorf("node %q match[%d]: %w", n.ID, j, err))
			}
		}
	}

	for i := range s.Edges {
		e := &s.Edges[i]
		if e.Kind == "" {
			e.Kind = "flows"
		} else if !validEdgeKinds[e.Kind] {
			errs = append(errs, fmt.Errorf("edge[%d] (%s -> %s): unknown kind %q", i, e.From, e.To, e.Kind))
		}
		if !nodeIDs[e.From] {
			errs = append(errs, fmt.Errorf("edge[%d]: unknown from node %q", i, e.From))
		}
		if !nodeIDs[e.To] {
			errs = append(errs, fmt.Errorf("edge[%d]: unknown to node %q", i, e.To))
		}
	}

	flowIDs := make(map[string]bool, len(s.Flows))
	for i := range s.Flows {
		f := &s.Flows[i]
		if f.ID == "" {
			errs = append(errs, fmt.Errorf("flow[%d]: id is required", i))
		} else if flowIDs[f.ID] {
			errs = append(errs, fmt.Errorf("duplicate flow id %q", f.ID))
		} else {
			flowIDs[f.ID] = true
		}
		for j, st := range f.Steps {
			if !nodeIDs[st.Node] {
				errs = append(errs, fmt.Errorf("flow %q step[%d]: unknown node %q", f.ID, j, st.Node))
			}
		}
	}

	if s.Money != nil {
		if len(s.Money.Fields) == 0 {
			errs = append(errs, errors.New("money: fields must not be empty"))
		}
		for i, f := range s.Money.Fields {
			if strings.TrimSpace(f) == "" {
				errs = append(errs, fmt.Errorf("money: fields[%d] is empty", i))
			}
		}
		if s.Money.MinorUnits < 0 || s.Money.MinorUnits > 8 {
			errs = append(errs, fmt.Errorf("money: minor_units must be between 0 and 8, got %d", s.Money.MinorUnits))
		}
		s.Money.Currency = strings.ToUpper(strings.TrimSpace(s.Money.Currency))
	}

	if len(s.StepNames) > 0 {
		names := make(map[string]map[string]string, len(s.StepNames))
		for fx, bySeq := range s.StepNames {
			key := FixtureKey(fx)
			if key == "" {
				errs = append(errs, fmt.Errorf("step_names: empty fixture key %q", fx))
				continue
			}
			if _, dup := names[key]; dup {
				errs = append(errs, fmt.Errorf("step_names: fixture %q given more than once", key))
				continue
			}
			out := make(map[string]string, len(bySeq))
			for seq, name := range bySeq {
				if !isDigits(seq) {
					errs = append(errs, fmt.Errorf("step_names %q: seq %q is not a number", key, seq))
					continue
				}
				n, _ := strconv.ParseUint(seq, 10, 64)
				out[strconv.FormatUint(n, 10)] = name
			}
			names[key] = out
		}
		s.StepNames = names
	}

	return errors.Join(errs...)
}

// FixtureKey is the name a fixture is known by in step_names and flows: its
// base name without the .kavach and .tokenized suffixes.
func FixtureKey(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	p = strings.TrimSuffix(p, ".kavach")
	p = strings.TrimSuffix(p, ".tokenized")
	return p
}

func (r *Rule) prepare() error {
	if r.On == "" {
		switch {
		case r.Sink != "":
			r.On = "output"
		case r.Source != "" || r.Position != "":
			r.On = "input"
		case r.Kind != "":
			r.On = "failure"
		default:
			return errors.New("rule has no \"on\" and none could be inferred")
		}
	}
	switch r.On {
	case "input", "output", "failure":
	default:
		return fmt.Errorf("unknown rule \"on\" %q", r.On)
	}
	if r.Regex != "" {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return fmt.Errorf("invalid regex %q: %w", r.Regex, err)
		}
		r.re = re
	}
	return nil
}

func buildCodeLinks(refs []string, codeBase string) []CodeLink {
	if len(refs) == 0 {
		return nil
	}
	out := make([]CodeLink, 0, len(refs))
	for _, ref := range refs {
		link := CodeLink{Text: ref}
		if codeBase != "" {
			path, lineA, lineB := splitCodeRef(ref)
			u := codeBase + strings.TrimPrefix(path, "/")
			if lineA != "" {
				u += "#L" + lineA
				if lineB != "" {
					u += "-L" + lineB
				}
			}
			link.URL = u
		}
		out = append(out, link)
	}
	return out
}

func splitCodeRef(ref string) (path, lineA, lineB string) {
	idx := strings.LastIndexByte(ref, ':')
	if idx <= 0 || idx == len(ref)-1 {
		return ref, "", ""
	}
	suffix := ref[idx+1:]
	if dash := strings.IndexByte(suffix, '-'); dash >= 0 {
		a, b := suffix[:dash], suffix[dash+1:]
		if isDigits(a) && isDigits(b) {
			return ref[:idx], a, b
		}
		return ref, "", ""
	}
	if isDigits(suffix) {
		return ref[:idx], suffix, ""
	}
	return ref, "", ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (r *Rule) matchInput(source, position string, data []byte) bool {
	if r.On != "input" {
		return false
	}
	if r.Source != "" && r.Source != source {
		return false
	}
	if r.Position != "" && !matchPosition(r.Position, position) {
		return false
	}
	return r.matchPayload(data)
}

func (r *Rule) matchOutput(sink string, data []byte) bool {
	if r.On != "output" {
		return false
	}
	if r.Sink != "" && r.Sink != sink {
		return false
	}
	return r.matchPayload(data)
}

func (r *Rule) matchFailure(kind, message string) bool {
	if r.On != "failure" {
		return false
	}
	if r.Kind != "" && r.Kind != kind {
		return false
	}
	return r.matchPayload([]byte(message))
}

func (r *Rule) matchPayload(data []byte) bool {
	if len(r.JSON) > 0 {
		obj, ok := parseJSONObject(data)
		if !ok {
			return false
		}
		for dotPath, want := range r.JSON {
			got, found := resolveDotPath(obj, dotPath)
			if !found || normScalar(got) != normScalar(want) {
				return false
			}
		}
	}
	if r.Contains != "" && !strings.Contains(string(data), r.Contains) {
		return false
	}
	if r.re != nil && !r.re.Match(data) {
		return false
	}
	return true
}

func matchPosition(pattern, actual string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(actual, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == actual
}

func parseJSONObject(data []byte) (map[string]any, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func resolveDotPath(obj map[string]any, dotPath string) (any, bool) {
	parts := strings.Split(dotPath, ".")
	var cur any = obj
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, exists := m[p]
		if !exists {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func normScalar(v any) string {
	switch x := v.(type) {
	case json.Number:
		if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
			return fmt.Sprint(f)
		}
		return x.String()
	case float64:
		return fmt.Sprint(x)
	case float32:
		return fmt.Sprint(float64(x))
	case int:
		return fmt.Sprint(float64(x))
	case int64:
		return fmt.Sprint(float64(x))
	case uint64:
		return fmt.Sprint(float64(x))
	default:
		return fmt.Sprint(x)
	}
}
