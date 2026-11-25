package kconfig

import (
	"fmt"
	"strconv"
	"strings"
)

type yamlKind int

const (
	yamlScalar yamlKind = iota
	yamlMap
	yamlSeq
)

type yamlNode struct {
	kind   yamlKind
	scalar string
	quoted bool
	line   int
	keys   []string
	fields map[string]*yamlNode
	items  []*yamlNode
}

type yamlLine struct {
	num    int
	indent int
	text   string
}

func parseYAML(data []byte) (*yamlNode, error) {
	lines, err := preprocessYAML(string(data))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return &yamlNode{kind: yamlMap, fields: map[string]*yamlNode{}}, nil
	}
	node, next, err := parseBlock(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if next < len(lines) {
		return nil, fmt.Errorf("line %d: unexpected content %q", lines[next].num, lines[next].text)
	}
	return node, nil
}

func preprocessYAML(src string) ([]yamlLine, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	rawLines := strings.Split(src, "\n")
	var out []yamlLine
	for i, raw := range rawLines {
		lineNum := i + 1
		// Count leading spaces; reject tabs in indentation.
		indent := 0
		for indent < len(raw) {
			if raw[indent] == ' ' {
				indent++
			} else if raw[indent] == '\t' {
				return nil, fmt.Errorf("line %d: tabs are not allowed for YAML indentation", lineNum)
			} else {
				break
			}
		}
		trimmed := stripInlineComment(raw[indent:])
		trimmed = strings.TrimRight(trimmed, " \t")
		if trimmed == "" || trimmed == "---" || trimmed == "..." {
			continue
		}
		out = append(out, yamlLine{num: lineNum, indent: indent, text: trimmed})
	}
	return out, nil
}

// stripInlineComment removes a trailing YAML comment (# preceded by whitespace
// or at index 0) while preserving # inside single or double quotes.
func stripInlineComment(s string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return s[:i]
			}
		}
	}
	return s
}

func parseBlock(lines []yamlLine, pos int, baseIndent int) (*yamlNode, int, error) {
	if pos >= len(lines) {
		return &yamlNode{kind: yamlScalar}, pos, nil
	}
	if isSeqItem(lines[pos].text) {
		return parseSeq(lines, pos, baseIndent)
	}
	return parseMap(lines, pos, baseIndent)
}

func isSeqItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

func parseSeq(lines []yamlLine, pos int, baseIndent int) (*yamlNode, int, error) {
	seq := &yamlNode{kind: yamlSeq, line: lines[pos].num}
	for pos < len(lines) {
		ln := lines[pos]
		if ln.indent < baseIndent {
			break
		}
		if ln.indent > baseIndent {
			return nil, pos, fmt.Errorf("line %d: unexpected indentation %d (expected %d)", ln.num, ln.indent, baseIndent)
		}
		if !isSeqItem(ln.text) {
			break
		}
		rest := ""
		if len(ln.text) > 1 {
			rest = strings.TrimLeft(ln.text[1:], " ")
		}
		itemIndent := ln.indent + 2
		if len(ln.text) > 1 {
			// Compute exact column of first non-space char after '-'.
			spaces := len(ln.text) - 1 - len(rest)
			if spaces > 0 {
				itemIndent = ln.indent + 1 + spaces
			}
		}

		if rest == "" {
			// Item starts on the next line.
			pos++
			if pos >= len(lines) || lines[pos].indent <= baseIndent {
				seq.items = append(seq.items, &yamlNode{kind: yamlScalar, line: ln.num})
				continue
			}
			child, next, err := parseBlock(lines, pos, lines[pos].indent)
			if err != nil {
				return nil, next, err
			}
			seq.items = append(seq.items, child)
			pos = next
			continue
		}

		// Inline key: value on the same line as '-' starts a mapping item.
		if k, v, ok := splitKeyValue(rest); ok {
			m := &yamlNode{kind: yamlMap, line: ln.num, fields: map[string]*yamlNode{}}
			pos++
			valNode, next, err := parseValue(k, v, ln.num, itemIndent, lines, pos)
			if err != nil {
				return nil, next, err
			}
			m.addField(k, valNode)
			pos = next
			// Continue reading additional keys indented at itemIndent.
			if pos < len(lines) && lines[pos].indent >= itemIndent && !isSeqItem(lines[pos].text) {
				if lines[pos].indent != itemIndent {
					return nil, pos, fmt.Errorf("line %d: mapping indentation %d does not match %d", lines[pos].num, lines[pos].indent, itemIndent)
				}
				var err error
				pos, err = parseMapInto(m, lines, pos, itemIndent)
				if err != nil {
					return nil, pos, err
				}
			}
			seq.items = append(seq.items, m)
			continue
		}

		// Scalar or flow value on the same line as '-'.
		val, err := parseInlineValue(rest, ln.num)
		if err != nil {
			return nil, pos, err
		}
		seq.items = append(seq.items, val)
		pos++
	}
	return seq, pos, nil
}

func parseMap(lines []yamlLine, pos int, baseIndent int) (*yamlNode, int, error) {
	m := &yamlNode{kind: yamlMap, line: lines[pos].num, fields: map[string]*yamlNode{}}
	next, err := parseMapInto(m, lines, pos, baseIndent)
	return m, next, err
}

func parseMapInto(m *yamlNode, lines []yamlLine, pos int, baseIndent int) (int, error) {
	for pos < len(lines) {
		ln := lines[pos]
		if ln.indent < baseIndent {
			break
		}
		if ln.indent > baseIndent {
			return pos, fmt.Errorf("line %d: unexpected indentation %d (expected %d)", ln.num, ln.indent, baseIndent)
		}
		if isSeqItem(ln.text) {
			break
		}
		k, v, ok := splitKeyValue(ln.text)
		if !ok {
			return pos, fmt.Errorf("line %d: expected key: value, got %q", ln.num, ln.text)
		}
		pos++
		valNode, next, err := parseValue(k, v, ln.num, baseIndent, lines, pos)
		if err != nil {
			return next, err
		}
		m.addField(k, valNode)
		pos = next
	}
	return pos, nil
}

func parseValue(key, rawVal string, lineNum, parentIndent int, lines []yamlLine, pos int) (*yamlNode, int, error) {
	rawVal = strings.TrimSpace(rawVal)
	if rawVal != "" {
		val, err := parseInlineValue(rawVal, lineNum)
		if err != nil {
			return nil, pos, err
		}
		return val, pos, nil
	}
	// Empty inline value: check if the next line begins a nested block or a
	// sequence at the same indentation (YAML allows seq items at parentIndent).
	if pos >= len(lines) {
		return &yamlNode{kind: yamlScalar, line: lineNum}, pos, nil
	}
	next := lines[pos]
	if next.indent > parentIndent {
		return parseBlock(lines, pos, next.indent)
	}
	if next.indent == parentIndent && isSeqItem(next.text) {
		return parseSeq(lines, pos, next.indent)
	}
	return &yamlNode{kind: yamlScalar, line: lineNum}, pos, nil
}

func (n *yamlNode) addField(k string, v *yamlNode) {
	if _, exists := n.fields[k]; !exists {
		n.keys = append(n.keys, k)
	}
	n.fields[k] = v
}

// splitKeyValue splits "key: value" or "key:" into (key, value, true).
// A colon only separates key from value if it is at the end of the line or
// followed by whitespace, so values like "kafka:events" remain intact.
func splitKeyValue(s string) (key, val string, ok bool) {
	if len(s) == 0 {
		return "", "", false
	}
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case ':':
			if i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t' {
				rawKey := strings.TrimSpace(s[:i])
				if rawKey == "" {
					return "", "", false
				}
				k, _, err := parseScalar(rawKey)
				if err != nil {
					return "", "", false
				}
				return k, strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func parseInlineValue(s string, lineNum int) (*yamlNode, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("line %d: unterminated flow sequence %q", lineNum, s)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		seq := &yamlNode{kind: yamlSeq, line: lineNum}
		if inner == "" {
			return seq, nil
		}
		parts, err := splitFlowItems(inner, lineNum)
		if err != nil {
			return nil, err
		}
		for _, p := range parts {
			item, err := parseInlineValue(p, lineNum)
			if err != nil {
				return nil, err
			}
			seq.items = append(seq.items, item)
		}
		return seq, nil
	}
	val, quoted, err := parseScalar(s)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", lineNum, err)
	}
	return &yamlNode{kind: yamlScalar, scalar: val, quoted: quoted, line: lineNum}, nil
}

func splitFlowItems(s string, lineNum int) ([]string, error) {
	var parts []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if inDouble {
			cur.WriteByte(c)
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			cur.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					cur.WriteByte(s[i])
					continue
				}
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
			cur.WriteByte(c)
		case '\'':
			inSingle = true
			cur.WriteByte(c)
		case '[', '{':
			depth++
			cur.WriteByte(c)
		case ']', '}':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("line %d: unexpected %q in flow sequence", lineNum, c)
			}
			cur.WriteByte(c)
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if inSingle || inDouble || depth != 0 {
		return nil, fmt.Errorf("line %d: unterminated string or bracket in flow sequence", lineNum)
	}
	tail := strings.TrimSpace(cur.String())
	if tail != "" {
		parts = append(parts, tail)
	}
	return parts, nil
}

func parseScalar(s string) (val string, quoted bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, nil
	}
	if s[0] == '"' {
		if len(s) < 2 || s[len(s)-1] != '"' {
			return "", true, fmt.Errorf("unterminated double-quoted string %q", s)
		}
		u, err := strconv.Unquote(s)
		if err != nil {
			return "", true, fmt.Errorf("invalid double-quoted string %q: %w", s, err)
		}
		return u, true, nil
	}
	if s[0] == '\'' {
		if len(s) < 2 || s[len(s)-1] != '\'' {
			return "", true, fmt.Errorf("unterminated single-quoted string %q", s)
		}
		inner := s[1 : len(s)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\'' {
				if i+1 < len(inner) && inner[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				return "", true, fmt.Errorf("unescaped single quote in %q", s)
			}
			b.WriteByte(inner[i])
		}
		return b.String(), true, nil
	}
	return s, false, nil
}

func (n *yamlNode) isNull() bool {
	if n == nil {
		return true
	}
	if n.kind != yamlScalar || n.quoted {
		return false
	}
	s := strings.ToLower(n.scalar)
	return s == "" || s == "null" || s == "~"
}

func (n *yamlNode) asString() (string, error) {
	if n == nil {
		return "", nil
	}
	if n.kind != yamlScalar {
		return "", fmt.Errorf("line %d: expected scalar string", n.line)
	}
	if n.isNull() {
		return "", nil
	}
	return n.scalar, nil
}

func (n *yamlNode) asInt() (int, error) {
	if n == nil || n.isNull() {
		return 0, nil
	}
	if n.kind != yamlScalar {
		return 0, fmt.Errorf("line %d: expected integer", n.line)
	}
	v, err := strconv.Atoi(n.scalar)
	if err != nil {
		return 0, fmt.Errorf("line %d: invalid integer %q", n.line, n.scalar)
	}
	return v, nil
}

func (n *yamlNode) asFloat64() (float64, error) {
	if n == nil || n.isNull() {
		return 0, nil
	}
	if n.kind != yamlScalar {
		return 0, fmt.Errorf("line %d: expected number", n.line)
	}
	v, err := strconv.ParseFloat(n.scalar, 64)
	if err != nil {
		return 0, fmt.Errorf("line %d: invalid number %q", n.line, n.scalar)
	}
	return v, nil
}

func (n *yamlNode) asStringSlice() ([]string, error) {
	if n == nil || n.isNull() {
		return nil, nil
	}
	if n.kind != yamlSeq {
		return nil, fmt.Errorf("line %d: expected list", n.line)
	}
	out := make([]string, 0, len(n.items))
	for _, item := range n.items {
		s, err := item.asString()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// formatYAMLScalar formats a Go string as a clean YAML scalar, quoting only
// when necessary so values like "kafka:events_charged_in_advance" and
// "$.transaction_id" remain unquoted and human-readable.
func formatYAMLScalar(s string) string {
	if s == "" {
		return `""`
	}
	if needsYAMLQuotes(s) {
		return strconv.Quote(s)
	}
	return s
}

func needsYAMLQuotes(s string) bool {
	if strings.TrimSpace(s) != s {
		return true
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "~", "yes", "no", "on", "off":
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	switch s[0] {
	case '-', '?', ':', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`', ',':
		return true
	}
	if strings.HasSuffix(s, ":") {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c == '"' || c == '\'' || c == '\n' || c == '\r' || c == '\t' ||
			c == '[' || c == ']' || c == '{' || c == '}' || c == ',' || c == '#' {
			return true
		}
		if c == ':' && i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
			return true
		}
	}
	return false
}

func formatInlineList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = formatYAMLScalar(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
