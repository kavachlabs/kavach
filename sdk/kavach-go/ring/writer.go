package ring

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// Writer writes KVRING02-framed records either into a lock-free shared-memory
// Ring or into a fallback file/io.Writer stream (e.g. kavach dev or non-mmap
// paths). All methods are safe for concurrent use and never block production
// traffic when the ring is full.
type Writer struct {
	mu      sync.Mutex
	shm     *Ring
	stream  io.Writer
	closer  io.Closer
	buf     []byte
	dropped atomic.Uint64
	opened  bool
	closed  bool
}

// NewRingWriter wraps an open *Ring in a thread-safe frame Writer and emits
// FrameOpen if the ring is empty (write cursor == 0).
func NewRingWriter(r *Ring, meta OpenMeta) (*Writer, error) {
	w := &Writer{
		shm: r,
		buf: make([]byte, 0, 1024),
	}
	if r != nil && r.WriteCursor() == 0 {
		if meta.Ring == 0 {
			meta.Ring = int(r.Capacity())
		}
		if meta.RingPath == "" {
			meta.RingPath = r.Path()
		}
		if err := w.WriteOpen(meta); err != nil {
			return nil, err
		}
	} else {
		w.opened = true
	}
	return w, nil
}

// NewStreamWriter creates a fallback frame Writer targeting any io.Writer
// (file, pipe, or buffer) using the exact same uvarint frame wire format.
func NewStreamWriter(out io.Writer, meta OpenMeta) (*Writer, error) {
	w := &Writer{
		stream: out,
		buf:    make([]byte, 0, 1024),
	}
	if c, ok := out.(io.Closer); ok {
		w.closer = c
	}
	if meta.Service != "" {
		if err := w.WriteOpen(meta); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// OpenPath opens a Writer for path: it attempts a KVRING02 mmap shared-memory
// ring first (working on Linux /dev/shm and macOS /tmp), and transparently
// falls back to a regular append stream file if mmap is unavailable (e.g. FIFO,
// pipe, or non-Unix host).
func OpenPath(path string, capacity uint64, meta OpenMeta) (*Writer, error) {
	if r, err := OpenOrCreate(path, capacity); err == nil {
		return NewRingWriter(r, meta)
	}
	// Fallback to a regular stream file.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	w, err := NewStreamWriter(f, meta)
	if err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

// Ring returns the underlying *Ring, or nil if using the stream fallback.
func (w *Writer) Ring() *Ring {
	if w == nil {
		return nil
	}
	return w.shm
}

// Dropped returns the total number of frames/batches dropped on full ring.
func (w *Writer) Dropped() uint64 {
	if w == nil {
		return 0
	}
	n := w.dropped.Load()
	if w.shm != nil {
		if d := w.shm.Dropped(); d > n {
			return d
		}
	}
	return n
}

// WriteOpen writes a FrameOpen header frame.
func (w *Writer) WriteOpen(meta OpenMeta) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	buf, err := AppendOpenFrame(w.buf[:0], meta)
	if err != nil {
		return err
	}
	w.buf = buf
	w.opened = true
	return w.publishLocked(buf)
}

// WriteRecord encodes and publishes a single FrameRecord non-blockingly.
func (w *Writer) WriteRecord(r Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	buf, err := AppendRecordFrame(w.buf[:0], r)
	if err != nil {
		return err
	}
	w.buf = buf
	return w.publishLocked(buf)
}

// WriteStep atomically encodes and publishes a complete step (Input + all
// interior records + optional Marker + StepEnd) in one contiguous publish so
// concurrent goroutines never interleave frames or trigger nested-input errors
// in ShmRingConsumer.
func (w *Writer) WriteStep(recs []Record, stepEnd bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	buf := w.buf[:0]
	for _, r := range recs {
		var err error
		buf, err = AppendRecordFrame(buf, r)
		if err != nil {
			return err
		}
	}
	if stepEnd {
		buf = AppendStepEndFrame(buf)
	}
	w.buf = buf
	return w.publishLocked(buf)
}

// WriteRaw publishes pre-encoded frame bytes atomically.
func (w *Writer) WriteRaw(p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || len(p) == 0 {
		return nil
	}
	return w.publishLocked(p)
}

// WriteFacts writes a FrameFacts frame holding environment facts.
func (w *Writer) WriteFacts(facts []Fact) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.buf = AppendFactsFrame(w.buf[:0], facts)
	return w.publishLocked(w.buf)
}

// WriteSnapshot writes a FrameSnapshot frame holding serialized handler state.
func (w *Writer) WriteSnapshot(state []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.buf = AppendSnapshotFrame(w.buf[:0], state)
	return w.publishLocked(w.buf)
}

// WriteStepEnd writes a standalone FrameStepEnd frame.
func (w *Writer) WriteStepEnd() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.buf = AppendStepEndFrame(w.buf[:0])
	return w.publishLocked(w.buf)
}

// WriteFlush writes a FrameFlush frame.
func (w *Writer) WriteFlush(durable bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.buf = AppendFlushFrame(w.buf[:0], durable)
	return w.publishLocked(w.buf)
}

// Close emits FrameClose and releases the underlying mmap ring or file handle.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.buf = AppendCloseFrame(w.buf[:0])
	_ = w.publishLocked(w.buf)
	w.closed = true
	var firstErr error
	if w.shm != nil {
		firstErr = w.shm.Close()
	}
	if w.closer != nil {
		if err := w.closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (w *Writer) publishLocked(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if w.shm != nil {
		if ok, _ := w.shm.TryPublish(p); !ok {
			w.dropped.Add(1)
		}
		return nil
	}
	if w.stream != nil {
		_, err := w.stream.Write(p)
		return err
	}
	return nil
}
