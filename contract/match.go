package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Match selects inputs by a value inside their JSON data. An expression is a
// path, "==", and a JSON value: $.argv[0]=="HSETEX", $.type=="order" or
// $["event type"]==3. A path is $ followed by .name, ["name"] or [index]
// segments. Strings compare exactly; an input that is not JSON, or lacks the
// path, does not match.
type Match struct {
	expr string
	path []any // string keys and int indexes
	want any
}

// ParseMatch parses a match expression.
func ParseMatch(expr string) (*Match, error) {
	lhs, rhs, ok := strings.Cut(expr, "==")
	if !ok {
		return nil, fmt.Errorf("feature match %q: want PATH==VALUE", expr)
	}
	m := &Match{expr: expr}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rhs)), &m.want); err != nil {
		return nil, fmt.Errorf("feature match %q: the value is not JSON: %v", expr, err)
	}
	p := strings.TrimSpace(lhs)
	if !strings.HasPrefix(p, "$") {
		return nil, fmt.Errorf("feature match %q: the path must start with $", expr)
	}
	p = p[1:]
	for p != "" {
		switch {
		case p[0] == '.':
			end := strings.IndexAny(p[1:], ".[")
			if end < 0 {
				end = len(p) - 1
			}
			name := p[1 : 1+end]
			if name == "" {
				return nil, fmt.Errorf("feature match %q: empty name in path", expr)
			}
			m.path = append(m.path, name)
			p = p[1+end:]
		case p[0] == '[':
			end := strings.IndexByte(p, ']')
			if end < 0 {
				return nil, fmt.Errorf("feature match %q: unclosed [ in path", expr)
			}
			seg := p[1:end]
			if strings.HasPrefix(seg, `"`) {
				var name string
				if err := json.Unmarshal([]byte(seg), &name); err != nil {
					return nil, fmt.Errorf("feature match %q: bad name %s in path", expr, seg)
				}
				m.path = append(m.path, name)
			} else {
				i, err := strconv.Atoi(seg)
				if err != nil || i < 0 {
					return nil, fmt.Errorf("feature match %q: bad index %s in path", expr, seg)
				}
				m.path = append(m.path, i)
			}
			p = p[end+1:]
		default:
			return nil, fmt.Errorf("feature match %q: unexpected %q in path", expr, p)
		}
	}
	return m, nil
}

func (m *Match) String() string { return m.expr }

// Matches reports whether data is JSON holding the wanted value at the path.
func (m *Match) Matches(data []byte) bool {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return false
	}
	for _, seg := range m.path {
		switch s := seg.(type) {
		case string:
			o, ok := v.(map[string]any)
			if !ok {
				return false
			}
			if v, ok = o[s]; !ok {
				return false
			}
		case int:
			a, ok := v.([]any)
			if !ok || s >= len(a) {
				return false
			}
			v = a[s]
		}
	}
	got, _ := json.Marshal(v)
	want, _ := json.Marshal(m.want)
	if n, ok := v.(json.Number); ok {
		if f, err := n.Float64(); err == nil {
			got, _ = json.Marshal(f)
		}
	}
	return bytes.Equal(got, want)
}
