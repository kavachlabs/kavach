// Package recstream encodes and decodes the record stream between an SDK and
// kavach-recorder (SPEC.md §10.2). The recorder reads it with a Reader; an SDK
// writes it with an Encoder.
package recstream

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kavachlabs/kavach/journal"
)

// Kinds of frame (SPEC.md §10.2).
const (
	KindOpen     byte = 0x01
	KindRecord   byte = 0x02
	KindStepEnd  byte = 0x03
	KindFacts    byte = 0x04
	KindSnapshot byte = 0x05
	KindFlush    byte = 0x06
	KindClose    byte = 0x07
)

var kindNames = map[byte]string{
	KindOpen: "open", KindRecord: "record", KindStepEnd: "step_end", KindFacts: "facts",
	KindSnapshot: "snapshot", KindFlush: "flush", KindClose: "close",
}

// KindName returns the name of a frame kind, or "kind(0xNN)".
func KindName(k byte) string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind(0x%02x)", k)
}

// MaxFrame bounds the size of one frame, kind and payload together: a record
// (at most journal.MaxBodyLen) or a snapshot.
const MaxFrame = 64 << 20

// Protocol is the version of the record stream this package speaks.
const Protocol = 1

// Open is the JSON object of the open frame (SPEC.md §10.2). Zero values of
// the optional fields mean the defaults the spec lists; Defaults fills them.
type Open struct {
	Protocol  int    `json:"protocol"`
	Service   string `json:"service"`
	Start     string `json:"start"`
	Producer  string `json:"producer,omitempty"`
	Handler   string `json:"handler,omitempty"`
	Snapshots bool   `json:"snapshots"`

	Dir            string   `json:"dir,omitempty"`
	Compression    string   `json:"compression,omitempty"`
	Level          int      `json:"level,omitempty"`
	BlockBytes     int      `json:"block_bytes,omitempty"`
	FlushMS        int      `json:"flush_ms,omitempty"`
	SegmentBytes   int64    `json:"segment_bytes,omitempty"`
	SegmentSeconds int      `json:"segment_seconds,omitempty"`
	RetainSegments int      `json:"retain_segments,omitempty"`
	SecretKeys     []string `json:"secret_keys,omitempty"`

	// Ring, if set, is the capacity of the shared-memory ring that carries
	// every later frame (SPEC.md §10.7).
	Ring int `json:"ring,omitempty"`
	// RingPath names the ring file when the SDK could not pass it as fd 3.
	RingPath string `json:"ring_path,omitempty"`
}

// Defaults of the open object (SPEC.md §10.2).
const (
	DefaultDir            = "kavach"
	DefaultBlockBytes     = 4 << 20
	DefaultFlushMS        = 1000
	DefaultSegmentBytes   = 256 << 20
	DefaultSegmentSeconds = 3600
	DefaultRetainSegments = 24
)

// Defaults returns o with every unset optional field set to its default. It
// does not touch the required fields.
func (o Open) Defaults() Open {
	if o.Dir == "" {
		o.Dir = DefaultDir
	}
	if o.Compression == "" {
		o.Compression = journal.CompressionZstd
	}
	if o.Level == 0 {
		o.Level = journal.DefaultLevel
	}
	if o.BlockBytes == 0 {
		o.BlockBytes = DefaultBlockBytes
	}
	if o.FlushMS == 0 {
		o.FlushMS = DefaultFlushMS
	}
	if o.SegmentBytes == 0 {
		o.SegmentBytes = DefaultSegmentBytes
	}
	if o.SegmentSeconds == 0 {
		o.SegmentSeconds = DefaultSegmentSeconds
	}
	if o.RetainSegments == 0 {
		o.RetainSegments = DefaultRetainSegments
	}
	return o
}

// Validate checks the open object.
func (o Open) Validate() error {
	switch {
	case o.Protocol != Protocol:
		return fmt.Errorf("open: unsupported protocol %d, want %d", o.Protocol, Protocol)
	case o.Service == "":
		return errors.New("open: missing \"service\"")
	case o.Start != journal.StartGenesis && o.Start != journal.StartSnapshot:
		return fmt.Errorf("open: \"start\" must be \"genesis\" or \"snapshot\", got %q", o.Start)
	case o.Compression != "" && o.Compression != journal.CompressionZstd && o.Compression != journal.CompressionNone:
		return fmt.Errorf("open: unsupported compression %q", o.Compression)
	case o.Level < 0 || o.BlockBytes < 0 || o.FlushMS < 0 || o.SegmentBytes < 0 || o.SegmentSeconds < 0 || o.RetainSegments < 0:
		return errors.New("open: numeric options must not be negative")
	case o.BlockBytes > journal.MaxBlockLen-journal.MaxBodyLen:
		return fmt.Errorf("open: block_bytes %d is too large", o.BlockBytes)
	case o.Ring != 0 && (o.Ring < MinRing || o.Ring&(o.Ring-1) != 0):
		return fmt.Errorf("open: ring %d is not a power of two of at least %d", o.Ring, MinRing)
	}
	return nil
}

// Frame is one frame of the record stream.
type Frame struct {
	Kind    byte
	Payload []byte
}

// AppendFrame appends the encoding of a frame to dst.
func AppendFrame(dst []byte, kind byte, payload []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(1+len(payload)))
	dst = append(dst, kind)
	return append(dst, payload...)
}

// AppendRecordFrame appends a record frame holding r to dst. Seq is ignored.
// It encodes in place, so the hot path of an SDK needs no scratch payload.
func AppendRecordFrame(dst []byte, r journal.Record) ([]byte, error) {
	start := len(dst)
	// One byte for the length is enough for all but large records; those
	// are shifted up once the length is known.
	dst = append(dst, 0, KindRecord, byte(r.Type), r.Flags)
	dst, err := journal.AppendPayload(dst, r)
	if err != nil {
		return dst[:start], err
	}
	n := len(dst) - start - 1
	if n-2 > journal.MaxBodyLen-10 {
		return dst[:start], fmt.Errorf("recstream: record payload is %d bytes, too large", n-2)
	}
	if n < 0x80 {
		dst[start] = byte(n)
		return dst, nil
	}
	var hdr [binary.MaxVarintLen64]byte
	k := binary.PutUvarint(hdr[:], uint64(n))
	dst = append(dst, hdr[:k-1]...)
	copy(dst[start+k:], dst[start+1:len(dst)-(k-1)])
	copy(dst[start:], hdr[:k])
	return dst, nil
}

// Buffered is the number of bytes read from the source but not yet decoded.
func (r *Reader) Buffered() int { return r.r.Buffered() }

// Raw returns the reader's source, with whatever it has buffered. Over the
// ring, standard input carries only doorbell bytes after the open frame.
func (r *Reader) Raw() io.Reader { return r.r }

// Reader decodes frames from a stream.
type Reader struct {
	r    *bufio.Reader
	slab []byte // frames are carved from it, one allocation for many
}

const slabSize = 64 << 10

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReaderSize(r, 1<<20)} }

// Next returns the next frame. It returns io.EOF when the stream ends between
// frames and io.ErrUnexpectedEOF when it ends inside one. A frame that cannot
// be valid (length zero or over MaxFrame) is an error that is neither.
func (r *Reader) Next() (Frame, error) {
	n, err := binary.ReadUvarint(r.r)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Frame{}, err
		}
		return Frame{}, fmt.Errorf("invalid frame length: %w", err)
	}
	if n == 0 || n > MaxFrame {
		return Frame{}, fmt.Errorf("invalid frame length %d", n)
	}
	var buf []byte
	if n > slabSize/4 {
		buf = make([]byte, n)
	} else {
		if int(n) > len(r.slab) {
			r.slab = make([]byte, slabSize)
		}
		buf, r.slab = r.slab[:n:n], r.slab[n:]
	}
	if _, err := io.ReadFull(r.r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Kind: buf[0], Payload: buf[1:]}, nil
}

// ParseOpen decodes the payload of an open frame. Unknown keys are ignored.
func ParseOpen(payload []byte) (Open, error) {
	var o Open
	if err := json.Unmarshal(payload, &o); err != nil {
		return o, fmt.Errorf("open: %w", err)
	}
	return o, o.Validate()
}

// ParseRecord decodes the payload of a record frame: type, flags and the
// record's payload, without a seq. The result has Seq 0.
func ParseRecord(payload []byte) (journal.Record, error) {
	if len(payload) < 2 {
		return journal.Record{}, errors.New("record frame is shorter than its type and flags")
	}
	return journal.ParsePayload(journal.Type(payload[0]), payload[1], payload[2:])
}

// ParseFacts decodes the payload of a facts frame.
func ParseFacts(payload []byte) ([]journal.Fact, error) {
	r, err := journal.ParsePayload(journal.TypeEnvironment, 0, payload)
	return r.Facts, err
}

// ParseSnapshot decodes the payload of a snapshot frame: bytes.
func ParseSnapshot(payload []byte) ([]byte, error) {
	r, err := journal.ParsePayload(journal.TypeSnapshot, 0, payload)
	return r.Data, err
}

// ParseFlush decodes the payload of a flush frame and reports whether it asks
// for durability.
func ParseFlush(payload []byte) (durable bool, err error) {
	if len(payload) != 1 || payload[0] > 1 {
		return false, errors.New("flush frame must hold one byte, 0 or 1")
	}
	return payload[0] == 1, nil
}

// Encoder writes frames to an io.Writer. It is what an SDK written in Go calls;
// it does not buffer, so each call is one write into the pipe.
type Encoder struct {
	w   io.Writer
	buf []byte
}

// NewEncoder returns an Encoder that writes to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

func (e *Encoder) write(kind byte, payload []byte) error {
	e.buf = AppendFrame(e.buf[:0], kind, payload)
	_, err := e.w.Write(e.buf)
	return err
}

// Open writes the open frame. Protocol is set to Protocol.
func (e *Encoder) Open(o Open) error {
	o.Protocol = Protocol
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return e.write(KindOpen, b)
}

// Record writes a record frame. Seq is ignored: the recorder assigns it.
func (e *Encoder) Record(r journal.Record) error {
	buf, err := AppendRecordFrame(e.buf[:0], r)
	if err != nil {
		return err
	}
	e.buf = buf
	_, err = e.w.Write(buf)
	return err
}

// Records writes several record frames, then a step_end frame if stepEnd, in
// one write into the pipe.
func (e *Encoder) Records(recs []journal.Record, stepEnd bool) error {
	var out []byte
	for _, r := range recs {
		payload, err := journal.AppendPayload([]byte{byte(r.Type), r.Flags}, r)
		if err != nil {
			return err
		}
		out = AppendFrame(out, KindRecord, payload)
	}
	if stepEnd {
		out = AppendFrame(out, KindStepEnd, nil)
	}
	_, err := e.w.Write(out)
	return err
}

// StepEnd writes a step_end frame.
func (e *Encoder) StepEnd() error { return e.write(KindStepEnd, nil) }

// Facts writes a facts frame.
func (e *Encoder) Facts(facts []journal.Fact) error {
	payload, err := journal.AppendPayload(nil, journal.Record{Type: journal.TypeEnvironment, Facts: facts})
	if err != nil {
		return err
	}
	return e.write(KindFacts, payload)
}

// Snapshot writes a snapshot frame holding the handler's state.
func (e *Encoder) Snapshot(state []byte) error {
	payload, err := journal.AppendPayload(nil, journal.Record{Type: journal.TypeSnapshot, Data: state})
	if err != nil {
		return err
	}
	return e.write(KindSnapshot, payload)
}

// Flush writes a flush frame.
func (e *Encoder) Flush(durable bool) error {
	b := byte(0)
	if durable {
		b = 1
	}
	return e.write(KindFlush, []byte{b})
}

// Close writes a close frame.
func (e *Encoder) Close() error { return e.write(KindClose, nil) }
