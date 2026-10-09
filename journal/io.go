package journal

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Start values for Meta.Start (SPEC.md §3.2).
const (
	StartGenesis  = "genesis"
	StartSnapshot = "snapshot"
)

// Meta is the header metadata of a journal (SPEC.md §3.2).
type Meta struct {
	Service string `json:"service"`
	Start   string `json:"start"`
	// Compression is "zstd" or "none" (SPEC.md §3.3). Empty means "zstd" when
	// writing.
	Compression string `json:"compression"`
	Handler     string `json:"handler,omitempty"`
	Producer    string `json:"producer,omitempty"`
	Recorder    string `json:"recorder,omitempty"`
	RecordedAt  string `json:"recorded_at,omitempty"`
	// Run identifies the process run that wrote the journal; the segments of a
	// run share it and number themselves with Segment from 0.
	Run     string `json:"run,omitempty"`
	Segment *int   `json:"segment,omitempty"`
	// CutFrom is set on a fixture cut from a longer journal (SPEC.md §3.6).
	CutFrom *CutFrom `json:"cut_from,omitempty"`
	// Variant is set on journals derived from a recorded one by perturbing it
	// (SPEC.md §3.2). They are replayed leniently and carry no outputs.
	Variant *Variant `json:"variant,omitempty"`
}

// CutFrom names the journal a fixture was cut from.
type CutFrom struct {
	Run      string `json:"run"`
	Segment  int    `json:"segment"`
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}

// Variant describes how a journal was derived from a recorded incident.
type Variant struct {
	ID       int    `json:"id"`
	Mutation string `json:"mutation"`
	// Incident is the seq of the input on which the old build is expected to
	// fail the way it failed in production.
	Incident uint64 `json:"incident"`
	// Failure is the recorded failure: "panic: <message>", "error: <message>",
	// "invariant: <name>" or "crash".
	Failure string `json:"failure"`
}

func (m Meta) validate() error {
	if m.Service == "" {
		return errors.New("header is missing \"service\"")
	}
	switch m.Compression {
	case CompressionZstd, CompressionNone:
	case "":
		return errors.New("header is missing \"compression\"")
	default:
		return fmt.Errorf("unsupported compression %q", m.Compression)
	}
	switch m.Start {
	case StartGenesis, StartSnapshot:
		return nil
	case "":
		return errors.New("header is missing \"start\"")
	default:
		return fmt.Errorf("unsupported start %q", m.Start)
	}
}

// Header is a decoded journal header.
type Header struct {
	Major, Minor uint16
	Meta         Meta
}

// ErrCorrupt wraps every error caused by invalid journal bytes.
var ErrCorrupt = errors.New("journal: corrupt")

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

// appendHeader appends the header frame for meta to dst.
func appendHeader(dst []byte, meta Meta) ([]byte, error) {
	m, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if len(m) > MaxMetaLen {
		return nil, fmt.Errorf("journal: header metadata is %d bytes, limit is %d", len(m), MaxMetaLen)
	}
	data := append([]byte(nil), Magic[:]...)
	data = binary.LittleEndian.AppendUint16(data, Major)
	data = binary.LittleEndian.AppendUint16(data, Minor)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(m)))
	data = append(data, m...)
	data = binary.LittleEndian.AppendUint32(data, checksum(data[len(Magic):]))
	dst = binary.LittleEndian.AppendUint32(dst, magicHeaderFrame)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(data)))
	return append(dst, data...), nil
}

// order checks the order of records (SPEC.md §3.4, §5) and carries what it
// needs between records.
type order struct {
	start   string
	next    uint64
	started bool
	sawEnv  bool
}

func (o *order) check(r *Record) error {
	if o.started {
		if r.Seq != o.next {
			return fmt.Errorf("sequence gap: expected seq %d, got %d", o.next, r.Seq)
		}
		if r.Type == TypeSnapshot {
			return fmt.Errorf("snapshot record at seq %d is not the first record", r.Seq)
		}
	} else {
		switch o.start {
		case StartGenesis:
			if r.Seq != 0 {
				return fmt.Errorf("genesis journal must start at seq 0, got %d", r.Seq)
			}
			if r.Type == TypeSnapshot {
				return errors.New("genesis journal must not contain a snapshot record")
			}
		case StartSnapshot:
			if r.Type != TypeSnapshot {
				return fmt.Errorf("snapshot journal must start with a snapshot record, got %s", r.Type)
			}
		}
	}
	switch r.Type {
	case TypeGateway, TypeEnvironment, TypeConfig:
		if !r.Critical() {
			return fmt.Errorf("%s record at seq %d lacks the critical flag", r.Type, r.Seq)
		}
	case TypeInput:
		if !o.sawEnv {
			return fmt.Errorf("input at seq %d comes before the genesis environment", r.Seq)
		}
	}
	if r.Type == TypeEnvironment {
		seen := make(map[string]bool, len(r.Facts))
		for _, f := range r.Facts {
			if seen[f.Key] || f.Form > FactUnset {
				return fmt.Errorf("environment at seq %d has a repeated key or unknown form at %q", r.Seq, f.Key)
			}
			seen[f.Key] = true
		}
		o.sawEnv = true
	}
	o.started = true
	o.next = r.Seq + 1
	return nil
}

// WriterOptions tune how a Writer encodes blocks.
type WriterOptions struct {
	// Level is the zstd compression level. Defaults to DefaultLevel.
	Level int
	// BlockBytes is the raw size at which a block is closed. Defaults to
	// DefaultBlockSz.
	BlockBytes int
	// Async compresses and writes the blocks that fill up on a goroutine of
	// the Writer's own, so that Write does not wait for them. Flush and Close
	// wait for everything to be written. Close must be called to stop the
	// goroutine.
	Async bool
}

// Writer writes a journal to an io.Writer. Records are gathered into a block,
// which is written when it reaches the target size, on Flush and on Close.
type Writer struct {
	w     io.Writer
	meta  Meta
	opts  WriterOptions
	ord   order
	raw   []byte // records of the open block
	count int
	first uint64
	out   []byte
	body  []byte // scratch for one record

	// Async state: filled blocks go to the writer goroutine, which hands the
	// buffers back.
	jobs    chan blockJob
	free    chan []byte
	wg      sync.WaitGroup // blocks handed over and not yet written
	errMu   sync.Mutex
	asyncEr error
}

type blockJob struct {
	first uint64
	count int
	raw   []byte
}

// NewWriter writes the header for meta to w and returns a Writer for its
// records. An empty meta.Compression means zstd.
func NewWriter(w io.Writer, meta Meta) (*Writer, error) {
	return NewWriterOptions(w, meta, WriterOptions{})
}

// NewWriterOptions is NewWriter with options.
func NewWriterOptions(w io.Writer, meta Meta, opts WriterOptions) (*Writer, error) {
	if meta.Compression == "" {
		meta.Compression = CompressionZstd
	}
	if err := meta.validate(); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	if opts.Level == 0 {
		opts.Level = DefaultLevel
	}
	if opts.BlockBytes <= 0 {
		opts.BlockBytes = DefaultBlockSz
	}
	hdr, err := appendHeader(nil, meta)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return newWriter(w, meta, opts, order{start: meta.Start}), nil
}

func newWriter(w io.Writer, meta Meta, opts WriterOptions, ord order) *Writer {
	jw := &Writer{w: w, meta: meta, opts: opts, ord: ord}
	if opts.Async {
		jw.jobs, jw.free = make(chan blockJob, 1), make(chan []byte, 2)
		go jw.run(jw.jobs)
	}
	return jw
}

// run compresses and writes the blocks handed over, in order.
func (w *Writer) run(jobs <-chan blockJob) {
	var out []byte
	for j := range jobs {
		var err error
		if w.asyncErr() == nil {
			out, err = appendBlock(out[:0], j.first, j.count, j.raw, w.meta.Compression, w.opts.Level)
			if err == nil {
				_, err = w.w.Write(out)
			}
			w.setAsyncErr(err)
		}
		select {
		case w.free <- j.raw[:0]:
		default:
		}
		w.wg.Done()
	}
}

func (w *Writer) asyncErr() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.asyncEr
}

func (w *Writer) setAsyncErr(err error) {
	if err == nil {
		return
	}
	w.errMu.Lock()
	w.asyncEr = err
	w.errMu.Unlock()
}

// Write adds one record to the open block. Sequence numbers must be contiguous,
// a genesis journal must start at 0, a snapshot journal must start with a
// snapshot record and the first input must follow the genesis environment.
func (w *Writer) Write(r Record) error {
	if err := w.ord.check(&r); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	// check has advanced the order; a failed encode below leaves the writer
	// unusable, which is fine for a writer that has seen an invalid record.
	body, err := appendBody(w.body[:0], r)
	if err != nil {
		return err
	}
	w.body = body
	if len(body) > MaxBodyLen {
		return fmt.Errorf("journal: record body is %d bytes, limit is %d", len(body), MaxBodyLen)
	}
	if err := w.begin(r.Seq, len(body)); err != nil {
		return err
	}
	w.raw = append(w.raw, body...)
	return w.end()
}

// begin starts a record body of n bytes in the open block: it closes a block
// the record would overfill and writes the length.
func (w *Writer) begin(seq uint64, n int) error {
	if w.count > 0 && len(w.raw)+n+binary.MaxVarintLen64 > MaxBlockLen {
		if err := w.flushBlock(); err != nil {
			return err
		}
	}
	if w.count == 0 {
		w.first = seq
	}
	w.raw = binary.AppendUvarint(w.raw, uint64(n))
	return nil
}

// end counts the record just appended and closes the block once it is full.
func (w *Writer) end() error {
	w.count++
	if len(w.raw) >= w.opts.BlockBytes {
		return w.flushBlock()
	}
	return nil
}

// WriteEncoded adds a record whose payload is already encoded, as
// AppendPayload would write it. It checks the order like Write, but not the
// payload, which the caller has validated with ValidatePayload.
func (w *Writer) WriteEncoded(seq uint64, ty Type, flags uint8, payload []byte) error {
	r := Record{Seq: seq, Type: ty, Flags: flags}
	if err := w.ord.check(&r); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	if err := checkRecord(r); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	n := 10 + len(payload) // type, flags, seq and the payload
	if n > MaxBodyLen {
		return fmt.Errorf("journal: record body is %d bytes, limit is %d", n, MaxBodyLen)
	}
	if err := w.begin(seq, n); err != nil {
		return err
	}
	w.raw = append(w.raw, byte(ty), flags)
	w.raw = binary.LittleEndian.AppendUint64(w.raw, seq)
	w.raw = append(w.raw, payload...)
	return w.end()
}

// Pending returns the number of records in the open block.
func (w *Writer) Pending() int { return w.count }

// Flush writes the open block, if any, and every block handed over before it.
func (w *Writer) Flush() error {
	err := w.flushBlock()
	if w.jobs != nil {
		w.wg.Wait()
		if err == nil {
			err = w.asyncErr()
		}
	}
	return err
}

// flushBlock closes the open block: it writes it, or hands it to the writer
// goroutine.
func (w *Writer) flushBlock() error {
	if w.count == 0 {
		return nil
	}
	if w.jobs != nil {
		if err := w.asyncErr(); err != nil {
			return err
		}
		w.wg.Add(1)
		w.jobs <- blockJob{w.first, w.count, w.raw}
		select {
		case w.raw = <-w.free:
		default:
			w.raw = make([]byte, 0, cap(w.raw))
		}
		w.count = 0
		return nil
	}
	var err error
	w.out, err = appendBlock(w.out[:0], w.first, w.count, w.raw, w.meta.Compression, w.opts.Level)
	if err != nil {
		return err
	}
	w.raw, w.count = w.raw[:0], 0
	_, err = w.w.Write(w.out)
	return err
}

// Close writes the open block. It does not close the underlying writer.
func (w *Writer) Close() error {
	err := w.Flush()
	if w.jobs != nil {
		close(w.jobs)
		w.jobs = nil
	}
	return err
}

// Reader reads a journal from an io.Reader.
type Reader struct {
	r       *bufio.Reader
	hdr     Header
	ord     order
	pos     int64 // bytes consumed from the stream
	good    int64 // end of the last complete frame
	queue   []Record
	err     error
	done    bool
	ignored int64
	body    []byte
	dataBuf []byte
}

// NewReader reads and validates the journal header from r.
func NewReader(r io.Reader) (*Reader, error) {
	jr := &Reader{r: bufio.NewReader(r)}
	var fh [8]byte
	if _, err := jr.readFull(fh[:]); err != nil {
		return nil, corrupt("short header: %v", err)
	}
	if binary.LittleEndian.Uint32(fh[:]) != magicHeaderFrame {
		return nil, corrupt("not a Kavach journal (bad magic): file does not begin with a header frame")
	}
	dataLen := binary.LittleEndian.Uint32(fh[4:])
	if dataLen < uint32(len(Magic)+8+4) || dataLen > MaxMetaLen+uint32(len(Magic)+8+4) {
		return nil, corrupt("header frame is %d bytes", dataLen)
	}
	data := make([]byte, dataLen)
	if _, err := jr.readFull(data); err != nil {
		return nil, corrupt("short header: %v", err)
	}
	if !bytes.Equal(data[:len(Magic)], Magic[:]) {
		return nil, corrupt("not a Kavach journal (bad magic)")
	}
	major := binary.LittleEndian.Uint16(data[8:])
	minor := binary.LittleEndian.Uint16(data[10:])
	metaLen := binary.LittleEndian.Uint32(data[12:])
	if metaLen > MaxMetaLen || uint64(metaLen)+20 != uint64(dataLen) {
		return nil, corrupt("header metadata is %d bytes in a frame of %d", metaLen, dataLen)
	}
	if got := checksum(data[len(Magic) : 16+metaLen]); got != binary.LittleEndian.Uint32(data[16+metaLen:]) {
		return nil, corrupt("header checksum mismatch")
	}
	// Before 1.0, every minor version is its own format (SPEC.md §7).
	if major != Major || minor != Minor {
		return nil, fmt.Errorf("journal: unsupported format version %d.%d, this reader implements %d.%d", major, minor, Major, Minor)
	}
	var m Meta
	if err := json.Unmarshal(data[16:16+metaLen], &m); err != nil {
		return nil, corrupt("header metadata is not a JSON object: %v", err)
	}
	if err := m.validate(); err != nil {
		return nil, corrupt("%v", err)
	}
	jr.hdr = Header{Major: major, Minor: minor, Meta: m}
	jr.ord = order{start: m.Start}
	jr.good = jr.pos
	return jr, nil
}

func (r *Reader) readFull(p []byte) (int, error) {
	n, err := io.ReadFull(r.r, p)
	r.pos += int64(n)
	return n, err
}

// Header returns the journal header.
func (r *Reader) Header() Header { return r.hdr }

// Truncated reports whether the journal ended inside a block. It is meaningful
// once Next has returned io.EOF.
func (r *Reader) Truncated() bool { return r.ignored > 0 }

// IgnoredBytes is the number of bytes after the last complete block that were
// ignored because the file ended inside one (SPEC.md §3.5).
func (r *Reader) IgnoredBytes() int64 { return r.ignored }

// Offset is the length of the file up to the end of the last complete frame
// read so far.
func (r *Reader) Offset() int64 { return r.good }

// Next returns the next record. It returns io.EOF at the end of the journal,
// including when the file ends inside a block (see Truncated). Records of an
// unknown, non-critical type are skipped.
func (r *Reader) Next() (Record, error) {
	for {
		rec, err := r.read()
		if err != nil {
			return Record{}, err
		}
		if !rec.Type.Known() {
			if rec.Critical() {
				return Record{}, fmt.Errorf("journal: critical record of unknown %s at seq %d", rec.Type, rec.Seq)
			}
			continue
		}
		return rec, nil
	}
}

func (r *Reader) read() (Record, error) {
	for len(r.queue) == 0 {
		if r.err != nil {
			return Record{}, r.err
		}
		if r.done {
			return Record{}, io.EOF
		}
		if err := r.nextBlock(); err != nil {
			r.done, r.err = true, err
		}
	}
	rec := r.queue[0]
	r.queue = r.queue[1:]
	return rec, nil
}

// cut records that the file ended inside a frame, and how many bytes after the
// last complete one it did.
func (r *Reader) cut() {
	r.done = true
	r.ignored = r.pos - r.good
}

// nextBlock reads frames up to and including the next block and queues its
// records. At the end of the file it sets done.
func (r *Reader) nextBlock() error {
	for {
		var fh [8]byte
		n, err := r.readFull(fh[:])
		if err == io.EOF {
			r.done = true
			return nil
		}
		if err != nil {
			if n > 0 {
				r.cut()
				return nil
			}
			return err
		}
		magic, length := binary.LittleEndian.Uint32(fh[:]), binary.LittleEndian.Uint32(fh[4:])
		switch {
		case magic == magicBlockFrame:
			return r.block(length)
		case isSkippable(magic):
			skipped, err := io.CopyN(io.Discard, r.r, int64(length))
			r.pos += skipped
			if err != nil {
				r.cut()
				return nil
			}
			r.good = r.pos
		case magic == magicZstdFrame:
			return corrupt("data frame without a block frame")
		default:
			return corrupt("unexpected frame with magic 0x%08x", magic)
		}
	}
}

func (r *Reader) block(length uint32) error {
	if length != blockFrameLen {
		return corrupt("block frame is %d bytes, want %d", length, blockFrameLen)
	}
	var bh [blockFrameLen]byte
	if _, err := r.readFull(bh[:]); err != nil {
		r.cut()
		return nil
	}
	h, err := parseBlockHeader(bh[:])
	if err != nil {
		return err
	}
	if cap(r.dataBuf) < int(h.dataLen) {
		r.dataBuf = make([]byte, h.dataLen)
	}
	frame := r.dataBuf[:h.dataLen]
	if _, err := r.readFull(frame); err != nil {
		r.cut()
		return nil
	}
	raw, err := decompressFrame(frame, h.rawLen)
	if err != nil {
		return fmt.Errorf("%w (block at seq %d)", err, h.firstSeq)
	}
	recs, err := r.records(h, raw)
	if err != nil {
		return err
	}
	r.queue = recs
	r.good = r.pos
	return nil
}

// records decodes and checks the records of a block.
func (r *Reader) records(h blockHeader, raw []byte) ([]Record, error) {
	recs := make([]Record, 0, h.count)
	for len(raw) > 0 {
		n, k := binary.Uvarint(raw)
		if k <= 0 {
			return nil, corrupt("block at seq %d: invalid record length", h.firstSeq)
		}
		if n < minBodyLen || n > MaxBodyLen {
			return nil, corrupt("block at seq %d: record has invalid length %d", h.firstSeq, n)
		}
		if n > uint64(len(raw)-k) {
			return nil, corrupt("block at seq %d: record of %d bytes runs past the end of the block", h.firstSeq, n)
		}
		rec, err := parseBody(raw[k : k+int(n)])
		if err != nil {
			return nil, corrupt("record at seq %d: %v", rec.Seq, err)
		}
		raw = raw[k+int(n):]
		if len(recs) == 0 && rec.Seq != h.firstSeq {
			return nil, corrupt("block header says first_seq %d, its first record has seq %d", h.firstSeq, rec.Seq)
		}
		if err := r.ord.check(&rec); err != nil {
			return nil, corrupt("%v", err)
		}
		recs = append(recs, rec)
	}
	if len(recs) != int(h.count) {
		return nil, corrupt("block at seq %d holds %d records, block header says %d", h.firstSeq, len(recs), h.count)
	}
	return recs, nil
}

// Journal is a fully decoded journal.
type Journal struct {
	Header  Header
	Records []Record
	// Truncated is set when the file ended inside a block; IgnoredBytes is how
	// much of it was ignored (SPEC.md §3.5).
	Truncated    bool
	IgnoredBytes int64
}

// Decode reads an entire journal from r.
func Decode(r io.Reader) (*Journal, error) {
	jr, err := NewReader(r)
	if err != nil {
		return nil, err
	}
	j := &Journal{Header: jr.Header()}
	for {
		rec, err := jr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		j.Records = append(j.Records, rec)
	}
	j.Truncated, j.IgnoredBytes = jr.Truncated(), jr.IgnoredBytes()
	return j, nil
}

// Encode writes meta and records as a complete journal to w.
func Encode(w io.Writer, meta Meta, records []Record) error {
	jw, err := NewWriter(w, meta)
	if err != nil {
		return err
	}
	for _, r := range records {
		if err := jw.Write(r); err != nil {
			return err
		}
	}
	return jw.Close()
}
