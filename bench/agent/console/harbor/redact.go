package harbor

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// minSecret is the shortest value redacted: a shorter one would blot out
// ordinary text.
const minSecret = 8

// redactWriter passes writes to w with every secret replaced by [redacted].
// A secret split across two writes is still found: the last len(longest
// secret)-1 bytes are held back until the next write or Close.
type redactWriter struct {
	mu      sync.Mutex
	w       io.Writer
	secrets [][]byte
	hold    []byte
	keep    int
	hits    int // replacements made
}

// newRedactWriter redacts the values given, and their JSON-escaped forms.
func newRedactWriter(w io.Writer, values []string) *redactWriter {
	r := &redactWriter{w: w}
	for _, v := range values {
		if len(v) < minSecret {
			continue
		}
		r.add([]byte(v))
		if q, _ := json.Marshal(v); string(q) != `"`+v+`"` {
			r.add(q[1 : len(q)-1])
		}
	}
	return r
}

func (r *redactWriter) add(s []byte) {
	r.secrets = append(r.secrets, s)
	r.keep = max(r.keep, len(s)-1)
}

func (r *redactWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf := r.scrub(append(r.hold, p...))
	cut := max(len(buf)-r.keep, 0)
	r.hold = bytes.Clone(buf[cut:])
	if _, err := r.w.Write(buf[:cut]); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close writes what was held back.
func (r *redactWriter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.w.Write(r.scrub(r.hold))
	r.hold = nil
	return err
}

func (r *redactWriter) scrub(b []byte) []byte {
	for _, s := range r.secrets {
		r.hits += bytes.Count(b, s)
		b = bytes.ReplaceAll(b, s, []byte("[redacted]"))
	}
	return b
}

// ScrubTree replaces the secrets in every regular file under dir with
// [redacted], in place. A file is read through twice, the first time to see
// whether it holds one, so memory stays bounded for any size and a file
// without a secret is not rewritten; one that does is rewritten to a
// temporary file in its folder, given the old mode, and renamed over it.
func ScrubTree(dir string, secrets []string) error {
	var first error
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			err = scrubFile(path, secrets)
		}
		if err != nil && first == nil {
			first = err
		}
		return nil
	})
	return first
}

func scrubFile(path string, secrets []string) error {
	hits, err := redactCopy(path, io.Discard, secrets)
	if err != nil || hits == 0 {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".scrub-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := redactCopy(path, tmp, secrets); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func redactCopy(path string, dst io.Writer, secrets []string) (hits int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := newRedactWriter(dst, secrets)
	if _, err := io.Copy(w, f); err != nil {
		return 0, err
	}
	return w.hits, w.Close()
}
