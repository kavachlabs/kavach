package invariants

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ExtractJSONPath extracts one or more values addressed by a JSONPath expression
// such as "$.transaction_id", "transaction_id", "$.nested.amount", "$.items[0].id",
// or "$.entries[*].delta".
func ExtractJSONPath(data []byte, expr string) ([]any, bool) {
	if len(data) == 0 {
		return nil, false
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false
	}
	tokens := parsePathTokens(expr)
	if len(tokens) == 0 {
		return []any{root}, true
	}
	cur := []any{root}
	for _, tok := range tokens {
		var next []any
		for _, node := range cur {
			switch tok.kind {
			case tokField:
				if m, ok := node.(map[string]any); ok {
					if v, exists := m[tok.field]; exists {
						next = append(next, v)
					}
				}
			case tokIndex:
				if arr, ok := node.([]any); ok && tok.index >= 0 && tok.index < len(arr) {
					next = append(next, arr[tok.index])
				}
			case tokWildcard:
				if arr, ok := node.([]any); ok {
					next = append(next, arr...)
				} else if m, ok := node.(map[string]any); ok {
					for _, v := range m {
						next = append(next, v)
					}
				}
			}
		}
		if len(next) == 0 {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// ExtractJSONPathString extracts the first value at expr formatted as a string.
func ExtractJSONPathString(data []byte, expr string) (string, bool) {
	vals, ok := ExtractJSONPath(data, expr)
	if !ok || len(vals) == 0 {
		return "", false
	}
	switch v := vals[0].(type) {
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	case nil:
		return "", false
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v), true
		}
		return string(b), true
	}
}

// ExtractJSONPathFloats extracts all numeric values addressed by expr.
func ExtractJSONPathFloats(data []byte, expr string) ([]float64, bool) {
	vals, ok := ExtractJSONPath(data, expr)
	if !ok || len(vals) == 0 {
		return nil, false
	}
	var out []float64
	for _, v := range vals {
		switch x := v.(type) {
		case float64:
			out = append(out, x)
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				out = append(out, f)
			}
		case []any:
			for _, elem := range x {
				if f, ok := elem.(float64); ok {
					out = append(out, f)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

type tokenKind uint8

const (
	tokField tokenKind = iota
	tokIndex
	tokWildcard
)

type pathToken struct {
	kind  tokenKind
	field string
	index int
}

func parsePathTokens(expr string) []pathToken {
	s := strings.TrimSpace(expr)
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return nil
	}
	var tokens []pathToken
	for i := 0; i < len(s); {
		if s[i] == '.' {
			i++
			continue
		}
		if s[i] == '[' {
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				break
			}
			inside := strings.TrimSpace(s[i+1 : i+end])
			if inside == "*" {
				tokens = append(tokens, pathToken{kind: tokWildcard})
			} else if idx, err := strconv.Atoi(inside); err == nil {
				tokens = append(tokens, pathToken{kind: tokIndex, index: idx})
			} else {
				tokens = append(tokens, pathToken{kind: tokField, field: strings.Trim(inside, `"'`)})
			}
			i += end + 1
			continue
		}
		j := i
		for j < len(s) && s[j] != '.' && s[j] != '[' {
			j++
		}
		if j > i {
			field := s[i:j]
			if field == "*" {
				tokens = append(tokens, pathToken{kind: tokWildcard})
			} else {
				tokens = append(tokens, pathToken{kind: tokField, field: field})
			}
		}
		i = j
	}
	return tokens
}
