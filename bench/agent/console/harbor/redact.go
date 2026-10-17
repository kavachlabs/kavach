package harbor

import (
	"bytes"
	"encoding/json"
	"io"
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
		b = bytes.ReplaceAll(b, s, []byte("[redacted]"))
	}
	return b
}
