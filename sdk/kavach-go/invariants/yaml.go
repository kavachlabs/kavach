// Package invariants parses declarative .kavach/invariants.yaml specifications
// with zero external dependencies and evaluates both declarative and code
// invariants across replay and recording steps.
package invariants

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Supported declarative invariant kinds (SPEC §4).
const (
	KindAtMostOnceEmit = "at_most_once_emit"
	KindAntiLobotomy   = "anti_lobotomy"
	KindNonNegative    = "non_negative"
	KindConservation   = "conservation"
)

// File represents a parsed .kavach/invariants.yaml document.
type File struct {
	Service    string `json:"service"`
	Invariants []Rule `json:"invariants"`
}

// Rule is one declarative invariant rule in .kavach/invariants.yaml.
type Rule struct {
	Name               string   `json:"name"`
	Kind               string   `json:"kind"`
	Sink               string   `json:"sink,omitempty"`
	Sinks              []string `json:"sinks,omitempty"`
	Key                string   `json:"key,omitempty"`
	Field              string   `json:"field,omitempty"`
	OnErrorEmitCount   *int     `json:"on_error_emit_count,omitempty"`
	MinRatioVsBaseline *float64 `json:"min_ratio_vs_baseline,omitempty"`
	Expr               string   `json:"expr,omitempty"`
}

// LoadFile reads and parses a .kavach/invariants.yaml file.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseYAML(b)
}

// ParseYAML parses a .kavach/invariants.yaml document using a clean zero-dep
// YAML subset parser supporting comments, quoted/unquoted scalars, inline
// flow sequences ([a, b]), and indented block sequences.
func ParseYAML(data []byte) (*File, error) {
	f := &File{}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	inInvariants := false
	inSinksList := false
	sinksIndent := 0
	var cur *Rule

	flushRule := func() {
		if cur != nil {
			f.Invariants = append(f.Invariants, *cur)
			cur = nil
		}
		inSinksList = false
	}

	for lineNo, raw := range lines {
		line := stripYAMLComment(raw)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "---" || trimmed == "..." {
			continue
		}
		indent := countIndent(line)

		if indent == 0 {
			inSinksList = false
			if strings.HasPrefix(trimmed, "invariants:") {
				flushRule()
				inInvariants = true
				rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "invariants:"))
				if rest == "[]" {
					inInvariants = false
				}
				continue
			}
			inInvariants = false
			k, v, ok := splitYAMLKV(trimmed)
			if !ok {
				return nil, fmt.Errorf("invariants.yaml:%d: expected key: value, got %q", lineNo+1, trimmed)
			}
			if k == "service" {
				f.Service = unquoteYAML(v)
			}
			continue
		}

		if !inInvariants {
			continue
		}

		// Block sequence item under sinks:
		if inSinksList && indent > sinksIndent && strings.HasPrefix(trimmed, "-") {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if cur != nil && item != "" {
				cur.Sinks = append(cur.Sinks, unquoteYAML(item))
			}
			continue
		}
		inSinksList = false

		// Start of a new invariant item ("- name: ..." or bare "-")
		if strings.HasPrefix(trimmed, "-") && (len(trimmed) == 1 || trimmed[1] == ' ' || trimmed[1] == '\t') {
			flushRule()
			cur = &Rule{}
			rest := strings.TrimSpace(trimmed[1:])
			if rest != "" {
				if err := applyRuleField(cur, rest, lineNo+1, &inSinksList, &sinksIndent, indent); err != nil {
					return nil, err
				}
			}
			continue
		}

		if cur == nil {
			return nil, fmt.Errorf("invariants.yaml:%d: unexpected field outside invariant entry: %q", lineNo+1, trimmed)
		}
		if err := applyRuleField(cur, trimmed, lineNo+1, &inSinksList, &sinksIndent, indent); err != nil {
			return nil, err
		}
	}
	flushRule()

	for i, r := range f.Invariants {
		if r.Name == "" {
			return nil, fmt.Errorf("invariants.yaml: invariant #%d missing required field \"name\"", i+1)
		}
		if r.Kind == "" {
			return nil, fmt.Errorf("invariants.yaml: invariant %q missing required field \"kind\"", r.Name)
		}
	}
	return f, nil
}

func applyRuleField(r *Rule, text string, lineNo int, inSinks *bool, sinksIndent *int, indent int) error {
	k, v, ok := splitYAMLKV(text)
	if !ok {
		return fmt.Errorf("invariants.yaml:%d: expected key: value, got %q", lineNo, text)
	}
	switch k {
	case "name":
		r.Name = unquoteYAML(v)
	case "kind":
		r.Kind = unquoteYAML(v)
	case "sink":
		r.Sink = unquoteYAML(v)
	case "sinks":
		v = strings.TrimSpace(v)
		if v == "" {
			*inSinks = true
			*sinksIndent = indent
			return nil
		}
		items, err := parseInlineList(v)
		if err != nil {
			return fmt.Errorf("invariants.yaml:%d: invalid sinks list: %w", lineNo, err)
		}
		r.Sinks = items
	case "key":
		r.Key = unquoteYAML(v)
	case "field":
		r.Field = unquoteYAML(v)
	case "expr":
		r.Expr = unquoteYAML(v)
	case "on_error_emit_count":
		n, err := strconv.Atoi(unquoteYAML(v))
		if err != nil {
			return fmt.Errorf("invariants.yaml:%d: invalid on_error_emit_count %q: %w", lineNo, v, err)
		}
		r.OnErrorEmitCount = &n
	case "min_ratio_vs_baseline":
		f, err := strconv.ParseFloat(unquoteYAML(v), 64)
		if err != nil {
			return fmt.Errorf("invariants.yaml:%d: invalid min_ratio_vs_baseline %q: %w", lineNo, v, err)
		}
		r.MinRatioVsBaseline = &f
	}
	return nil
}

func parseInlineList(v string) ([]string, error) {
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return []string{unquoteYAML(v)}, nil
	}
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(inner) {
				i++
				cur.WriteByte(inner[i])
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == ',' {
			item := strings.TrimSpace(cur.String())
			if item != "" {
				out = append(out, item)
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in %q", v)
	}
	if item := strings.TrimSpace(cur.String()); item != "" {
		out = append(out, item)
	}
	return out, nil
}

func splitYAMLKV(s string) (key, val string, ok bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == ':' && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

func stripYAMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(line) {
				i++
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}

func countIndent(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' {
			n++
		} else if line[i] == '\t' {
			n += 2
		} else {
			break
		}
	}
	return n
}

func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			if s[0] == '"' {
				if u, err := strconv.Unquote(s); err == nil {
					return u
				}
			}
			return s[1 : len(s)-1]
		}
	}
	return s
}
