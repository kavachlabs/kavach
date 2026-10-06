package journal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// File is an append-only journal stored in a file. Append assigns sequence
// numbers; Iterate reads the file from the start.
type File struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
	jw   *Writer
	next uint64
	sync bool
}

// FileOptions configure OpenFile.
type FileOptions struct {
	// Sync makes Append fsync after every record. Without it, records are
	// buffered until Flush or Close.
	Sync bool
}

// OpenFile opens the journal at path for appending, creating it with meta if it
// does not exist. Opening an existing journal keeps its header and cuts off a
// record left half-written by a crash. Only genesis journals can be appended to.
func OpenFile(path string, meta Meta, opts FileOptions) (*File, error) {
	if meta.Start == "" {
		meta.Start = StartGenesis
	}
	if meta.Start != StartGenesis {
		return nil, errors.New("journal: OpenFile only supports genesis journals")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	jf := &File{path: path, f: f, sync: opts.Sync}
	if err := jf.init(meta); err != nil {
		f.Close()
		return nil, err
	}
	return jf, nil
}

func (jf *File) init(meta Meta) error {
	st, err := jf.f.Stat()
	if err != nil {
		return err
	}
	jf.w = bufio.NewWriter(jf.f)
	if st.Size() == 0 {
		jw, err := NewWriter(jf.w, meta)
		if err != nil {
			return err
		}
		jf.jw = jw
		return jf.w.Flush()
	}

	// Existing journal: find the end of the last complete record.
	cr := &countingReader{r: jf.f}
	jr, err := NewReader(cr)
	if err != nil {
		return err
	}
	if jr.Header().Meta.Start != StartGenesis {
		return errors.New("journal: OpenFile only supports genesis journals")
	}
	end := cr.n - int64(jr.r.Buffered())
	for {
		_, err := jr.read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		end = cr.n - int64(jr.r.Buffered())
	}
	if err := jf.f.Truncate(end); err != nil {
		return err
	}
	if _, err := jf.f.Seek(end, io.SeekStart); err != nil {
		return err
	}
	jf.next = jr.nextSeq
	jf.jw = &Writer{w: jf.w, start: StartGenesis, next: jr.nextSeq, started: jr.started}
	return nil
}

// Path returns the file path.
func (jf *File) Path() string { return jf.path }

// Append assigns r the next sequence number, writes it, and returns the seq.
func (jf *File) Append(r Record) (uint64, error) {
	jf.mu.Lock()
	defer jf.mu.Unlock()
	if jf.f == nil {
		return 0, errors.New("journal: file is closed")
	}
	r.Seq = jf.next
	if err := jf.jw.Write(r); err != nil {
		return 0, err
	}
	jf.next++
	if jf.sync {
		if err := jf.w.Flush(); err != nil {
			return 0, err
		}
		if err := jf.f.Sync(); err != nil {
			return 0, err
		}
	}
	return r.Seq, nil
}

// Flush writes buffered records to the file.
func (jf *File) Flush() error {
	jf.mu.Lock()
	defer jf.mu.Unlock()
	if jf.f == nil {
		return nil
	}
	return jf.w.Flush()
}

// Iterate calls fn for every record from the start of the journal. Records
// appended but not yet flushed are included.
func (jf *File) Iterate(fn func(Record) error) error {
	if err := jf.Flush(); err != nil {
		return err
	}
	f, err := os.Open(jf.path)
	if err != nil {
		return err
	}
	defer f.Close()
	jr, err := NewReader(f)
	if err != nil {
		return err
	}
	for {
		rec, err := jr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// Close flushes and closes the file.
func (jf *File) Close() error {
	jf.mu.Lock()
	defer jf.mu.Unlock()
	if jf.f == nil {
		return nil
	}
	ferr := jf.w.Flush()
	cerr := jf.f.Close()
	jf.f = nil
	if ferr != nil {
		return fmt.Errorf("journal: flush: %w", ferr)
	}
	return cerr
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ReadFile decodes the whole journal at path.
func ReadFile(path string) (*Journal, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Decode(f)
}

// WriteFile writes a complete journal to path atomically (write, then rename).
func WriteFile(path string, meta Meta, records []Record) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kavach-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	bw := bufio.NewWriter(tmp)
	if err := Encode(bw, meta, records); err != nil {
		tmp.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
