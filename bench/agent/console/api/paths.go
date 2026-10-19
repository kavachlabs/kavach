package api

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
)

// pathRule says that the directory dir is written to as a placeholder.
type pathRule struct{ dir, to []byte }

// pathRules are the server's directories, the longest first so that a folder
// inside another is named by its own placeholder.
func pathRules(jobs, tasks, root string) []pathRule {
	var rules []pathRule
	add := func(dir, to string) {
		if len(dir) < 2 {
			return
		}
		rules = append(rules, pathRule{[]byte(dir), []byte(to)})
		if q, _ := json.Marshal(dir); string(q) != `"`+dir+`"` {
			rules = append(rules, pathRule{q[1 : len(q)-1], []byte(to)})
		}
	}
	add(jobs, "<jobs>")
	add(tasks, "<tasks>")
	add(root, "<root>")
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "~")
	}
	slices.SortStableFunc(rules, func(a, b pathRule) int { return len(b.dir) - len(a.dir) })
	return rules
}

// scrubPaths replaces each directory by its placeholder where it is a whole
// path prefix: not in the middle of a longer name, so with a home of /root
// the text /rootfs is left alone.
func scrubPaths(rules []pathRule, b []byte) []byte {
	for _, r := range rules {
		b = replacePath(b, r)
	}
	return b
}

func replacePath(b []byte, r pathRule) []byte {
	if !bytes.Contains(b, r.dir) {
		return b
	}
	var out []byte
	i := 0
	for {
		j := bytes.Index(b[i:], r.dir)
		if j < 0 {
			break
		}
		j += i
		end := j + len(r.dir)
		if pathStart(b, j) && pathEnd(b, end) {
			out = append(out, b[i:j]...)
			out = append(out, r.to...)
			i = end
		} else {
			out = append(out, b[i:j+1]...)
			i = j + 1
		}
	}
	return append(out, b[i:]...)
}

func pathChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == '/' || c == '~'
}

// pathStart reports whether a path may begin at b[j]: the byte before is not
// part of a name, except the n of a JSON \n and the like, or the slashes of a
// URL scheme.
func pathStart(b []byte, j int) bool {
	if j == 0 || !pathChar(b[j-1]) {
		return true
	}
	// file:///home/x: the slash that begins the path follows the scheme's.
	if j >= 3 && string(b[j-3:j]) == "://" {
		return true
	}
	return j >= 2 && b[j-2] == '\\' && b[j-1] >= 'a' && b[j-1] <= 'z'
}

// pathEnd reports whether a path ends at b[k]: the end, a slash, a byte that
// is not part of a name, or a full stop that ends a sentence.
func pathEnd(b []byte, k int) bool {
	if k >= len(b) || b[k] == '/' || !pathChar(b[k]) {
		return true
	}
	return b[k] == '.' && (k+1 >= len(b) || !pathChar(b[k+1]))
}
