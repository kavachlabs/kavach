package recorder

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/journal"
)

// File names, under the open object's dir:
//
//	<dir>/<service>-<run>-<segment>.kavach            a segment, segment as six digits
//	<dir>/fixtures/<service>-<run>-<seq>.kavach       a fixture; seq is the failing input's
//
// Service names are made safe for a file name first (see safeName).

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func safeName(s string) string { return unsafeName.ReplaceAllString(s, "_") }

// countWriter counts the bytes written through it.
type countWriter struct {
	f *os.File
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.f.Write(p)
	c.n += int64(n)
	return n, err
}

// segment is a journal file being written.
type segment struct {
	path     string
	f        *os.File
	cw       *countWriter
	w        *journal.Writer
	meta     journal.Meta
	index    int
	firstSeq uint64
	started  time.Time
}

func (r *recorder) segmentPath(index int) string {
	return filepath.Join(r.open.Dir, fmt.Sprintf("%s-%s-%06d.kavach", safeName(r.open.Service), r.run, index))
}

// newSegment creates the file of segment index and writes its header.
func (r *recorder) newSegment(index int, start string) (*segment, error) {
	meta := journal.Meta{
		Service:     r.open.Service,
		Start:       start,
		Compression: r.open.Compression,
		Handler:     r.open.Handler,
		Producer:    r.open.Producer,
		Recorder:    Name,
		RecordedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Run:         r.run,
		Segment:     &index,
	}
	if r.test != nil && r.test.RecordedAt != "" {
		meta.RecordedAt = r.test.RecordedAt
	}
	path := r.segmentPath(index)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	s := &segment{path: path, f: f, cw: &countWriter{f: f}, meta: meta, index: index, started: time.Now()}
	s.w, err = journal.NewWriterOptions(s.cw, meta, journal.WriterOptions{Level: r.open.Level, BlockBytes: r.open.BlockBytes})
	if err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// sync writes the open block and makes the file durable.
func (s *segment) sync() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.f.Sync()
}

func (s *segment) close() error {
	err := s.sync()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeFixture cuts the current segment, from its first record through the
// record with seq last, into a file of its own (SPEC.md §3.6). The segment has
// been synced, so everything is on disk to read.
func (r *recorder) writeFixture(inputSeq, last uint64) (string, error) {
	s := r.seg
	dir := filepath.Join(r.open.Dir, "fixtures")
	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%06d.kavach", safeName(r.open.Service), r.run, inputSeq))

	src, err := os.Open(s.path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	jr, err := journal.NewReader(src)
	if err != nil {
		return "", err
	}

	meta := s.meta
	meta.CutFrom = &journal.CutFrom{Run: r.run, Segment: s.index, FirstSeq: s.firstSeq, LastSeq: last}
	tmp, err := os.CreateTemp(dir, ".kavach-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	jw, err := journal.NewWriterOptions(tmp, meta, journal.WriterOptions{Level: r.open.Level, BlockBytes: r.open.BlockBytes})
	if err != nil {
		tmp.Close()
		return "", err
	}
	for {
		rec, err := jr.Next()
		if err == io.EOF {
			tmp.Close()
			return "", fmt.Errorf("segment %s ends before seq %d", s.path, last)
		}
		if err != nil {
			tmp.Close()
			return "", err
		}
		if err := jw.Write(rec); err != nil {
			tmp.Close()
			return "", err
		}
		if rec.Seq == last {
			break
		}
	}
	if err := jw.Flush(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

// prune deletes the oldest segments of the service beyond retain_segments. It
// never touches fixtures, nor the segment being written.
func (r *recorder) prune() error {
	entries, err := os.ReadDir(r.open.Dir)
	if err != nil {
		return err
	}
	type file struct {
		path string
		mod  time.Time
	}
	var segs []file
	prefix := safeName(r.open.Service) + "-"
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".kavach") {
			continue
		}
		path := filepath.Join(r.open.Dir, name)
		if r.seg != nil && path == r.seg.path {
			continue
		}
		// A service name can be a prefix of another's; the header knows.
		if !r.isSegmentOfService(path) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		segs = append(segs, file{path, info.ModTime()})
	}
	// The segment being written counts towards the limit.
	keep := r.open.RetainSegments
	if r.seg != nil {
		keep--
	}
	if keep < 0 {
		keep = 0
	}
	if len(segs) <= keep {
		return nil
	}
	sort.Slice(segs, func(i, j int) bool {
		if !segs[i].mod.Equal(segs[j].mod) {
			return segs[i].mod.Before(segs[j].mod)
		}
		return segs[i].path < segs[j].path
	})
	var errs []error
	for _, s := range segs[:len(segs)-keep] {
		if err := os.Remove(s.path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *recorder) isSegmentOfService(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	jr, err := journal.NewReader(f)
	return err == nil && jr.Header().Meta.Service == r.open.Service && jr.Header().Meta.Segment != nil
}
