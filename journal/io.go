package journal

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Start values for Meta.Start (SPEC.md §3.2).
const (
	StartGenesis  = "genesis"
	StartSnapshot = "snapshot"
)

// Meta is the header metadata of a journal (SPEC.md §3.2).
type Meta struct {
	Service    string `json:"service"`
	Start      string `json:"start"`
	Handler    string `json:"handler,omitempty"`
	Producer   string `json:"producer,omitempty"`
	RecordedAt string `json:"recorded_at,omitempty"`
	// Env is the environment the journal was recorded in (SPEC.md §3.2).
	Env *Env `json:"env,omitempty"`
	// Variant is set on journals derived from a recorded one by perturbing it
	// (SPEC.md §3.2). They are replayed leniently and carry no outputs.
	Variant *Variant `json:"variant,omitempty"`
}

// Variant describes how a journal was derived from a recorded incident.
type Variant struct {
	ID       int    `json:"id"`
	Mutation string `json:"mutation"`
	// Incident is the seq of the input on which the old build is expected to
	// fail the way it failed in production.
	Incident uint64 `json:"incident"`
	// Failure is the recorded failure: "panic: <message>", "error: <message>"
	// or "invariant: <name>".
	Failure string `json:"failure"`
}

func (m Meta) validate() error {
	if m.Service == "" {
		return errors.New("header is missing \"service\"")
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

// Writer writes a journal to an io.Writer.
type Writer struct {
	w       io.Writer
	start   string
	next    uint64
	started bool
	buf     []byte
}

// NewWriter writes the header for meta to w and returns a Writer for its records.
func NewWriter(w io.Writer, meta Meta) (*Writer, error) {
	if err := meta.validate(); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	m, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	hdr := append([]byte(nil), Magic[:]...)
	hdr = binary.LittleEndian.AppendUint16(hdr, Major)
	hdr = binary.LittleEndian.AppendUint16(hdr, Minor)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(m)))
	hdr = append(hdr, m...)
	hdr = binary.LittleEndian.AppendUint32(hdr, checksum(hdr[len(Magic):]))
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &Writer{w: w, start: meta.Start}, nil
}

// Write appends one record. Sequence numbers must be contiguous, a genesis journal
// must start at 0, and a snapshot journal must start with a snapshot record.
func (w *Writer) Write(r Record) error {
	if err := checkOrder(w.started, w.next, w.start, r); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	var err error
	w.buf, err = AppendFrame(w.buf[:0], r)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(w.buf); err != nil {
		return err
	}
	w.started = true
	w.next = r.Seq + 1
	return nil
}

func checkOrder(started bool, next uint64, start string, r Record) error {
	if started {
		if r.Seq != next {
			return fmt.Errorf("sequence gap: expected seq %d, got %d", next, r.Seq)
		}
		if r.Type == TypeSnapshot {
			return fmt.Errorf("snapshot record at seq %d is not the first record", r.Seq)
		}
		return nil
	}
	switch start {
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
	return nil
}

// Reader reads a journal from an io.Reader.
type Reader struct {
	r         *bufio.Reader
	hdr       Header
	nextSeq   uint64
	started   bool
	truncated bool
	done      bool
	body      []byte
}

// NewReader reads and validates the journal header from r.
func NewReader(r io.Reader) (*Reader, error) {
	br := bufio.NewReader(r)
	fixed := make([]byte, len(Magic)+8)
	if _, err := io.ReadFull(br, fixed); err != nil {
		return nil, corrupt("short header: %v", err)
	}
	if !bytes.Equal(fixed[:len(Magic)], Magic[:]) {
		return nil, corrupt("not a Kavach journal (bad magic)")
	}
	major := binary.LittleEndian.Uint16(fixed[8:])
	minor := binary.LittleEndian.Uint16(fixed[10:])
	metaLen := binary.LittleEndian.Uint32(fixed[12:])
	if major != Major {
		return nil, fmt.Errorf("journal: unsupported format version %d.%d", major, minor)
	}
	// Before 1.0, a minor newer than ours may have changed anything (SPEC.md §7).
	if major == 0 && minor > Minor {
		return nil, fmt.Errorf("journal: format version 0.%d is newer than supported 0.%d", minor, Minor)
	}
	if metaLen > MaxMetaLen {
		return nil, corrupt("header metadata is %d bytes, limit is %d", metaLen, MaxMetaLen)
	}
	rest := make([]byte, int(metaLen)+4)
	if _, err := io.ReadFull(br, rest); err != nil {
		return nil, corrupt("short header: %v", err)
	}
	meta := rest[:metaLen]
	want := binary.LittleEndian.Uint32(rest[metaLen:])
	if got := checksum(append(fixed[len(Magic):], meta...)); got != want {
		return nil, corrupt("header checksum mismatch")
	}
	var m Meta
	if err := json.Unmarshal(meta, &m); err != nil {
		return nil, corrupt("header metadata is not a JSON object: %v", err)
	}
	if err := m.validate(); err != nil {
		return nil, corrupt("%v", err)
	}
	return &Reader{r: br, hdr: Header{Major: major, Minor: minor, Meta: m}}, nil
}

// Header returns the journal header.
func (r *Reader) Header() Header { return r.hdr }

// Truncated reports whether the journal ended inside a record. It is meaningful
// once Next has returned io.EOF.
func (r *Reader) Truncated() bool { return r.truncated }

// Next returns the next record. It returns io.EOF at the end of the journal,
// including when the file ends inside a record (see Truncated). Records of an
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
	if r.done {
		return Record{}, io.EOF
	}
	var lenBuf [4]byte
	n, err := io.ReadFull(r.r, lenBuf[:])
	if err != nil {
		r.done = true
		if n > 0 {
			r.truncated = true
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Record{}, io.EOF
		}
		return Record{}, err
	}
	bodyLen := binary.LittleEndian.Uint32(lenBuf[:])
	if bodyLen < minBodyLen || bodyLen > MaxBodyLen {
		r.done = true
		return Record{}, corrupt("record after seq %d has invalid length %d", r.nextSeq, bodyLen)
	}
	if cap(r.body) < int(bodyLen)+4 {
		r.body = make([]byte, int(bodyLen)+4)
	}
	frame := r.body[:int(bodyLen)+4]
	if _, err := io.ReadFull(r.r, frame); err != nil {
		r.done = true
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			r.truncated = true
			return Record{}, io.EOF
		}
		return Record{}, err
	}
	body := frame[:bodyLen]
	if checksum(body) != binary.LittleEndian.Uint32(frame[bodyLen:]) {
		r.done = true
		return Record{}, corrupt("record checksum mismatch after seq %d", r.nextSeq)
	}
	rec, err := parseBody(body)
	if err != nil {
		r.done = true
		return Record{}, corrupt("record at seq %d: %v", rec.Seq, err)
	}
	if err := checkOrder(r.started, r.nextSeq, r.hdr.Meta.Start, rec); err != nil {
		r.done = true
		return Record{}, corrupt("%v", err)
	}
	r.started = true
	r.nextSeq = rec.Seq + 1
	return rec, nil
}

// Journal is a fully decoded journal.
type Journal struct {
	Header    Header
	Records   []Record
	Truncated bool
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
	j.Truncated = jr.Truncated()
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
	return nil
}
