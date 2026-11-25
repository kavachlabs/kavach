// Package ring implements the lock-free KVRING02 shared-memory ring writer and
// stream fallback matching include/kavach/daemon/shm_consumer.hpp and SPEC §10.2/§10.7.
package ring

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// KVRING02 binary layout constants matching shm_consumer.hpp byte-for-byte.
const (
	Magic           = "KVRING02"
	HeaderBytes     = 65536
	MinCapacity     = 65536
	DefaultCapacity = 8 << 20
	OffsetMagic     = 0
	OffsetCapacity  = 8
	OffsetWrite     = 64
	OffsetRead      = 128
	MaxFrameLen     = (16 << 20) + 65536
	ProtocolVersion = 1
)

// FrameKind identifies the outer uvarint-framed record stream envelope.
// Values match kavach::daemon::FrameKind in shm_consumer.hpp.
type FrameKind uint8

const (
	FrameOpen     FrameKind = 0x01
	FrameRecord   FrameKind = 0x02
	FrameStepEnd  FrameKind = 0x03
	FrameFacts    FrameKind = 0x04
	FrameSnapshot FrameKind = 0x05
	FrameFlush    FrameKind = 0x06
	FrameClose    FrameKind = 0x07
)

// String returns the human-readable frame kind name.
func (k FrameKind) String() string {
	switch k {
	case FrameOpen:
		return "open"
	case FrameRecord:
		return "record"
	case FrameStepEnd:
		return "step_end"
	case FrameFacts:
		return "facts"
	case FrameSnapshot:
		return "snapshot"
	case FrameFlush:
		return "flush"
	case FrameClose:
		return "close"
	default:
		return fmt.Sprintf("frame(0x%02x)", uint8(k))
	}
}

// RecordType identifies the inner record type of a FrameRecord payload.
// Values match kavach::daemon::RecordType in shm_consumer.hpp and SPEC §4.
type RecordType uint8

const (
	RecordInput       RecordType = 0x01
	RecordClock       RecordType = 0x02
	RecordRand        RecordType = 0x03
	RecordOutput      RecordType = 0x04
	RecordMarker      RecordType = 0x05
	RecordSnapshot    RecordType = 0x06
	RecordGateway     RecordType = 0x07
	RecordEnvironment RecordType = 0x08
	RecordConfig      RecordType = 0x09
)

// String returns the human-readable record type name.
func (t RecordType) String() string {
	switch t {
	case RecordInput:
		return "input"
	case RecordClock:
		return "clock"
	case RecordRand:
		return "rand"
	case RecordOutput:
		return "output"
	case RecordMarker:
		return "marker"
	case RecordSnapshot:
		return "snapshot"
	case RecordGateway:
		return "gateway"
	case RecordEnvironment:
		return "environment"
	case RecordConfig:
		return "config"
	default:
		return fmt.Sprintf("record(0x%02x)", uint8(t))
	}
}

// Record flag and scope constants matching SPEC §4 and §7.
const (
	FlagCritical uint8 = 1 << 0

	ScopeRemote uint8 = 0
	ScopeLocal  uint8 = 1

	FactValue  uint8 = 0
	FactSHA256 uint8 = 1
	FactUnset  uint8 = 2
)

// Standard failure/lifecycle marker kinds matching SPEC §4.5.
const (
	MarkerPanic     = "panic"
	MarkerError     = "error"
	MarkerTrigger   = "trigger"
	MarkerInvariant = "invariant"
	MarkerCrash     = "crash"
	MarkerExit      = "exit"
	MarkerDropped   = "dropped"
)

// OpenMeta is the JSON object encoded inside FrameOpen (SPEC §10.2).
type OpenMeta struct {
	Protocol  int    `json:"protocol"`
	Service   string `json:"service"`
	Start     string `json:"start"`
	Producer  string `json:"producer,omitempty"`
	Handler   string `json:"handler,omitempty"`
	Snapshots bool   `json:"snapshots"`
	Ring      int    `json:"ring,omitempty"`
	RingPath  string `json:"ring_path,omitempty"`
}

// Fact is one entry of a FrameFacts / RecordEnvironment payload.
type Fact struct {
	Key   string
	Form  uint8
	Value []byte
}

// Record is one decoded or encodable record payload.
type Record struct {
	Type  RecordType
	Flags uint8

	Source    string
	Position  string
	Sink      string
	Kind      string
	Message   string
	UnixNanos int64
	Data      []byte

	Gateway  string
	Request  []byte
	Response []byte
	Error    string
	Scope    uint8

	Facts []Fact

	Key     string
	Present bool
	Value   []byte
}

// Frame is one decoded frame from a KVRING02 ring or record stream.
type Frame struct {
	Kind    FrameKind
	Payload []byte
}

// RecordType returns the RecordType of a FrameRecord frame.
func (f Frame) RecordType() (RecordType, bool) {
	if f.Kind != FrameRecord || len(f.Payload) < 2 {
		return 0, false
	}
	return RecordType(f.Payload[0]), true
}

// RecordFlags returns the flags byte of a FrameRecord frame.
func (f Frame) RecordFlags() (uint8, bool) {
	if f.Kind != FrameRecord || len(f.Payload) < 2 {
		return 0, false
	}
	return f.Payload[1], true
}

// RecordPayload returns the payload bytes after type and flags of a FrameRecord frame.
func (f Frame) RecordPayload() []byte {
	if f.Kind != FrameRecord || len(f.Payload) < 2 {
		return nil
	}
	return f.Payload[2:]
}

// AppendFrame appends a uvarint-length-prefixed frame (kind + payload) to dst.
func AppendFrame(dst []byte, kind FrameKind, payload []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(1+len(payload)))
	dst = append(dst, byte(kind))
	return append(dst, payload...)
}

// AppendOpenFrame appends a FrameOpen frame with JSON-encoded OpenMeta.
func AppendOpenFrame(dst []byte, meta OpenMeta) ([]byte, error) {
	if meta.Protocol == 0 {
		meta.Protocol = ProtocolVersion
	}
	if meta.Start == "" {
		meta.Start = "genesis"
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return dst, err
	}
	return AppendFrame(dst, FrameOpen, b), nil
}

// AppendStepEndFrame appends a FrameStepEnd frame ([0x01, 0x03]).
func AppendStepEndFrame(dst []byte) []byte {
	return AppendFrame(dst, FrameStepEnd, nil)
}

// AppendFlushFrame appends a FrameFlush frame (1 byte: 0 or 1).
func AppendFlushFrame(dst []byte, durable bool) []byte {
	b := byte(0)
	if durable {
		b = 1
	}
	return AppendFrame(dst, FrameFlush, []byte{b})
}

// AppendCloseFrame appends a FrameClose frame ([0x01, 0x07]).
func AppendCloseFrame(dst []byte) []byte {
	return AppendFrame(dst, FrameClose, nil)
}

// AppendFactsFrame appends a FrameFacts frame holding environment facts.
func AppendFactsFrame(dst []byte, facts []Fact) []byte {
	var body []byte
	body = binary.AppendUvarint(body, uint64(len(facts)))
	for _, f := range facts {
		body = appendString(body, f.Key)
		body = append(body, f.Form)
		body = appendBytes(body, f.Value)
	}
	return AppendFrame(dst, FrameFacts, body)
}

// AppendSnapshotFrame appends a FrameSnapshot frame holding state bytes.
func AppendSnapshotFrame(dst []byte, state []byte) []byte {
	body := appendBytes(nil, state)
	return AppendFrame(dst, FrameSnapshot, body)
}

// AppendRecordPayload appends the wire payload for Record r (without type/flags).
func AppendRecordPayload(dst []byte, r Record) ([]byte, error) {
	switch r.Type {
	case RecordInput:
		dst = appendString(dst, r.Source)
		dst = appendString(dst, r.Position)
		dst = appendBytes(dst, r.Data)
	case RecordClock:
		dst = binary.LittleEndian.AppendUint64(dst, uint64(r.UnixNanos))
	case RecordRand, RecordSnapshot:
		dst = appendBytes(dst, r.Data)
	case RecordOutput:
		dst = appendString(dst, r.Sink)
		dst = appendBytes(dst, r.Data)
		dst = append(dst, r.Scope)
	case RecordMarker:
		dst = appendString(dst, r.Kind)
		dst = appendString(dst, r.Message)
		dst = appendBytes(dst, r.Data)
	case RecordGateway:
		dst = appendString(dst, r.Gateway)
		dst = appendBytes(dst, r.Request)
		dst = appendBytes(dst, r.Response)
		dst = appendString(dst, r.Error)
		dst = append(dst, r.Scope)
	case RecordEnvironment:
		dst = binary.AppendUvarint(dst, uint64(len(r.Facts)))
		for _, f := range r.Facts {
			dst = appendString(dst, f.Key)
			dst = append(dst, f.Form)
			dst = appendBytes(dst, f.Value)
		}
	case RecordConfig:
		dst = appendString(dst, r.Key)
		if r.Present {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
		dst = appendBytes(dst, r.Value)
		dst = appendString(dst, r.Source)
	default:
		return dst, fmt.Errorf("ring: invalid record type 0x%02x", uint8(r.Type))
	}
	return dst, nil
}

// AppendRecordFrame encodes a FrameRecord frame in place with zero extra allocations
// for records under 128 bytes. Matches recstream.AppendRecordFrame and shm_consumer.hpp.
func AppendRecordFrame(dst []byte, r Record) ([]byte, error) {
	if r.Type == 0 || r.Type == RecordEnvironment {
		return dst, fmt.Errorf("ring: record type 0x%02x cannot be sent in FrameRecord", uint8(r.Type))
	}
	if r.Flags&^FlagCritical != 0 {
		return dst, fmt.Errorf("ring: invalid record flags 0x%02x", r.Flags)
	}
	start := len(dst)
	dst = append(dst, 0, byte(FrameRecord), byte(r.Type), r.Flags)
	var err error
	dst, err = AppendRecordPayload(dst, r)
	if err != nil {
		return dst[:start], err
	}
	flen := len(dst) - start - 1 // FrameKind + RecordType + Flags + payload
	if flen > MaxFrameLen {
		return dst[:start], fmt.Errorf("ring: frame length %d exceeds max %d", flen, MaxFrameLen)
	}
	if flen < 0x80 {
		dst[start] = byte(flen)
		return dst, nil
	}
	var hdr [binary.MaxVarintLen64]byte
	k := binary.PutUvarint(hdr[:], uint64(flen))
	dst = append(dst, hdr[:k-1]...)
	copy(dst[start+k:], dst[start+1:len(dst)-(k-1)])
	copy(dst[start:], hdr[:k])
	return dst, nil
}

// DecodeRecord decodes a FrameRecord payload ([type, flags, body...]).
func DecodeRecord(framePayload []byte) (Record, error) {
	if len(framePayload) < 2 {
		return Record{}, errors.New("ring: record frame shorter than type+flags")
	}
	ty := RecordType(framePayload[0])
	flags := framePayload[1]
	if ty == 0 || (flags&^FlagCritical) != 0 {
		return Record{}, fmt.Errorf("ring: invalid record header type=0x%02x flags=0x%02x", uint8(ty), flags)
	}
	return DecodeRecordPayload(ty, flags, framePayload[2:])
}

// DecodeRecordPayload decodes the payload of a record given its type and flags.
func DecodeRecordPayload(ty RecordType, flags uint8, b []byte) (Record, error) {
	r := Record{Type: ty, Flags: flags}
	c := cursor{b: b}
	switch ty {
	case RecordInput:
		r.Source = c.str()
		r.Position = c.str()
		r.Data = c.bytes()
	case RecordClock:
		r.UnixNanos = int64(c.u64())
	case RecordRand, RecordSnapshot:
		r.Data = c.bytes()
	case RecordOutput:
		r.Sink = c.str()
		r.Data = c.bytes()
		r.Scope = c.u8()
	case RecordMarker:
		r.Kind = c.str()
		r.Message = c.str()
		r.Data = c.bytes()
	case RecordGateway:
		r.Gateway = c.str()
		r.Request = c.bytes()
		r.Response = c.bytes()
		r.Error = c.str()
		r.Scope = c.u8()
	case RecordEnvironment:
		n := c.uvarint()
		if c.err == nil && n > uint64(len(c.b))/3 {
			c.fail("ring: environment fact count %d exceeds payload", n)
		}
		if c.err == nil {
			r.Facts = make([]Fact, 0, n)
			for i := uint64(0); i < n; i++ {
				f := Fact{Key: c.str(), Form: c.u8(), Value: c.bytes()}
				if c.err != nil {
					break
				}
				r.Facts = append(r.Facts, f)
			}
		}
	case RecordConfig:
		r.Key = c.str()
		p := c.u8()
		if p > 1 {
			c.fail("ring: invalid config present byte %d", p)
		}
		r.Present = p == 1
		r.Value = c.bytes()
		r.Source = c.str()
	default:
		r.Data = append([]byte(nil), b...)
		return r, nil
	}
	return r, c.err
}

func appendBytes(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func appendString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

type cursor struct {
	b   []byte
	err error
}

func (c *cursor) fail(format string, args ...any) {
	if c.err == nil {
		c.err = fmt.Errorf(format, args...)
	}
}

func (c *cursor) u8() uint8 {
	if c.err != nil {
		return 0
	}
	if len(c.b) < 1 {
		c.fail("ring: truncated u8")
		return 0
	}
	v := c.b[0]
	c.b = c.b[1:]
	return v
}

func (c *cursor) u64() uint64 {
	if c.err != nil {
		return 0
	}
	if len(c.b) < 8 {
		c.fail("ring: truncated u64")
		return 0
	}
	v := binary.LittleEndian.Uint64(c.b[:8])
	c.b = c.b[8:]
	return v
}

func (c *cursor) uvarint() uint64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Uvarint(c.b)
	if n <= 0 {
		c.fail("ring: invalid uvarint")
		return 0
	}
	c.b = c.b[n:]
	return v
}

func (c *cursor) view() []byte {
	n := c.uvarint()
	if c.err != nil {
		return nil
	}
	if n > uint64(len(c.b)) {
		c.fail("ring: field length %d exceeds remaining %d bytes", n, len(c.b))
		return nil
	}
	v := c.b[:n]
	c.b = c.b[n:]
	return v
}

func (c *cursor) bytes() []byte {
	v := c.view()
	if len(v) == 0 {
		return nil
	}
	return append([]byte(nil), v...)
}

func (c *cursor) str() string {
	v := c.view()
	if c.err == nil && !utf8.Valid(v) {
		c.fail("ring: string field is not valid UTF-8")
	}
	return string(v)
}

// FrameDecoder mirrors kavach::daemon::FrameStreamDecoder in shm_consumer.hpp.
type FrameDecoder struct {
	pending    []byte
	frames     []Frame
	activeStep bool
	closed     bool
}

// DrainResult mirrors kavach::daemon::ConsumerDrainResult.
type DrainResult struct {
	Frames             []Frame
	SynthesizedMarker  *Record
	DiscardedTornBytes int
	CleanClose         bool
}

// Feed appends bytes and decodes any complete frames, validating state transitions
// identically to shm_consumer.hpp::FrameStreamDecoder::feed.
func (d *FrameDecoder) Feed(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	d.pending = append(d.pending, p...)
	count := 0
	off := 0
	for off < len(d.pending) {
		rem := d.pending[off:]
		flen, hdr := binary.Uvarint(rem)
		if hdr == 0 {
			break // incomplete varint
		}
		if hdr < 0 || flen == 0 || flen > MaxFrameLen {
			return count, errors.New("ring: invalid frame length")
		}
		if uint64(len(rem)-hdr) < flen {
			break // incomplete frame body
		}
		if d.closed {
			return count, errors.New("ring: unexpected frame after close")
		}
		rk := FrameKind(rem[hdr])
		if rk < FrameOpen || rk > FrameClose {
			return count, fmt.Errorf("ring: unknown frame kind 0x%02x", uint8(rk))
		}
		body := rem[hdr+1 : hdr+int(flen)]
		switch rk {
		case FrameOpen:
			if !utf8.Valid(body) {
				return count, errors.New("ring: open frame not valid UTF-8")
			}
		case FrameRecord:
			if len(body) < 2 || body[0] == 0 || RecordType(body[0]) == RecordEnvironment || (body[1]&^FlagCritical) != 0 {
				return count, errors.New("ring: invalid record frame")
			}
			if RecordType(body[0]) == RecordInput {
				if d.activeStep {
					return count, errors.New("ring: nested input without step_end")
				}
				d.activeStep = true
			}
		case FrameStepEnd:
			if len(body) != 0 || !d.activeStep {
				return count, errors.New("ring: invalid step_end")
			}
			d.activeStep = false
		case FrameFlush:
			if len(body) != 1 || body[0] > 1 {
				return count, errors.New("ring: invalid flush payload")
			}
		case FrameClose:
			if len(body) != 0 || d.activeStep {
				return count, errors.New("ring: invalid close")
			}
			d.closed = true
		}
		cp := make([]byte, len(body))
		copy(cp, body)
		d.frames = append(d.frames, Frame{Kind: rk, Payload: cp})
		off += hdr + int(flen)
		count++
	}
	if off > 0 {
		d.pending = append(d.pending[:0], d.pending[off:]...)
	}
	return count, nil
}

// FinishEOF drains accumulated frames and synthesizes crash/exit markers matching
// shm_consumer.hpp::FrameStreamDecoder::finish_eof.
func (d *FrameDecoder) FinishEOF() DrainResult {
	res := DrainResult{
		Frames:             d.frames,
		DiscardedTornBytes: len(d.pending),
	}
	d.frames = nil
	d.pending = nil
	if d.activeStep {
		crash := Record{Type: RecordMarker, Flags: FlagCritical, Kind: MarkerCrash, Message: "process ended during step"}
		payload, _ := AppendRecordPayload([]byte{byte(RecordMarker), FlagCritical}, crash)
		res.Frames = append(res.Frames, Frame{Kind: FrameRecord, Payload: payload})
		res.SynthesizedMarker = &crash
		d.activeStep = false
	} else if !d.closed {
		exit := Record{Type: RecordMarker, Kind: MarkerExit, Message: "process ended without closing the recorder"}
		res.SynthesizedMarker = &exit
	} else {
		res.CleanClose = true
	}
	return res
}

// ActiveStep reports whether an Input frame has not yet been matched by StepEnd.
func (d *FrameDecoder) ActiveStep() bool { return d.activeStep }

// Closed reports whether a Close frame has been decoded.
func (d *FrameDecoder) Closed() bool { return d.closed }

// Frames returns the decoded frames buffered so far.
func (d *FrameDecoder) Frames() []Frame { return d.frames }
