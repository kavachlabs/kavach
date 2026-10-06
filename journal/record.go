// Package journal implements the Kavach journal format described in SPEC.md.
package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"unicode/utf8"
)

// Format version written by this package.
const (
	Major = 0
	Minor = 1
)

// Magic is the 8-byte signature at the start of every journal file.
var Magic = [8]byte{0x89, 'K', 'V', 'J', '\r', '\n', 0x1a, '\n'}

// Limits from SPEC.md §3.
const (
	MaxMetaLen = 1 << 20
	MaxBodyLen = 16 << 20
	minBodyLen = 10 // type + flags + seq
)

// Type is a record type code.
type Type uint8

// Record types defined by SPEC.md §4.
const (
	TypeInput    Type = 0x01
	TypeClock    Type = 0x02
	TypeRand     Type = 0x03
	TypeOutput   Type = 0x04
	TypeMarker   Type = 0x05
	TypeSnapshot Type = 0x06
)

// FlagCritical marks a record that readers must understand (SPEC.md §7).
const FlagCritical uint8 = 1 << 0

var typeNames = map[Type]string{
	TypeInput:    "input",
	TypeClock:    "clock",
	TypeRand:     "rand",
	TypeOutput:   "output",
	TypeMarker:   "marker",
	TypeSnapshot: "snapshot",
}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type(0x%02x)", uint8(t))
}

// Known reports whether t is defined by the version of the spec this package implements.
func (t Type) Known() bool {
	_, ok := typeNames[t]
	return ok
}

// Standard marker kinds (SPEC.md §4.5).
const (
	MarkerPanic     = "panic"
	MarkerError     = "error"
	MarkerTrigger   = "trigger"
	MarkerInvariant = "invariant"
)

// Record is one decoded journal record. Which fields are meaningful depends on Type:
//
//	input:    Source, Position, Data
//	clock:    UnixNanos
//	rand:     Data
//	output:   Sink, Data
//	marker:   Kind, Message, Data
//	snapshot: Data
type Record struct {
	Type  Type   `json:"type"`
	Flags uint8  `json:"flags,omitempty"`
	Seq   uint64 `json:"seq"`

	Source    string `json:"source,omitempty"`
	Position  string `json:"position,omitempty"`
	Sink      string `json:"sink,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Message   string `json:"message,omitempty"`
	UnixNanos int64  `json:"unix_nanos,omitempty"`
	Data      []byte `json:"data,omitempty"`
}

// Critical reports whether the record's critical flag is set.
func (r Record) Critical() bool { return r.Flags&FlagCritical != 0 }

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func checksum(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// appendBody appends the encoded record body (type, flags, seq, payload) to dst.
func appendBody(dst []byte, r Record) ([]byte, error) {
	if r.Type == 0 {
		return nil, errors.New("journal: record type 0 is invalid")
	}
	if r.Flags&^FlagCritical != 0 {
		return nil, fmt.Errorf("journal: unsupported flag bits 0x%02x", r.Flags)
	}
	dst = append(dst, byte(r.Type), r.Flags)
	dst = binary.LittleEndian.AppendUint64(dst, r.Seq)
	switch r.Type {
	case TypeInput:
		dst = appendString(dst, r.Source)
		dst = appendString(dst, r.Position)
		dst = appendBytes(dst, r.Data)
	case TypeClock:
		dst = binary.LittleEndian.AppendUint64(dst, uint64(r.UnixNanos))
	case TypeRand, TypeSnapshot:
		dst = appendBytes(dst, r.Data)
	case TypeOutput:
		dst = appendString(dst, r.Sink)
		dst = appendBytes(dst, r.Data)
	case TypeMarker:
		dst = appendString(dst, r.Kind)
		dst = appendString(dst, r.Message)
		dst = appendBytes(dst, r.Data)
	default:
		// Extension types carry an opaque payload.
		dst = append(dst, r.Data...)
	}
	return dst, nil
}

// AppendFrame appends a complete framed record (length, body, crc) to dst.
func AppendFrame(dst []byte, r Record) ([]byte, error) {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	dst, err := appendBody(dst, r)
	if err != nil {
		return nil, err
	}
	body := dst[start+4:]
	if len(body) > MaxBodyLen {
		return nil, fmt.Errorf("journal: record body is %d bytes, limit is %d", len(body), MaxBodyLen)
	}
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(body)))
	return binary.LittleEndian.AppendUint32(dst, checksum(body)), nil
}

// parseBody decodes a record body. Unknown types keep their raw payload in Data.
func parseBody(body []byte) (Record, error) {
	var r Record
	r.Type = Type(body[0])
	r.Flags = body[1]
	r.Seq = binary.LittleEndian.Uint64(body[2:10])
	if r.Type == 0 {
		return r, errors.New("record type 0 is invalid")
	}
	if r.Flags&^FlagCritical != 0 {
		return r, fmt.Errorf("unsupported flag bits 0x%02x", r.Flags)
	}
	p := &payload{b: body[10:]}
	switch r.Type {
	case TypeInput:
		r.Source = p.string()
		r.Position = p.string()
		r.Data = p.bytes()
	case TypeClock:
		r.UnixNanos = int64(p.u64())
	case TypeRand, TypeSnapshot:
		r.Data = p.bytes()
	case TypeOutput:
		r.Sink = p.string()
		r.Data = p.bytes()
	case TypeMarker:
		r.Kind = p.string()
		r.Message = p.string()
		r.Data = p.bytes()
	default:
		r.Data = append([]byte(nil), body[10:]...)
		return r, nil
	}
	// Bytes after the known fields are ignored (SPEC.md §4).
	return r, p.err
}

func appendBytes(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func appendString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// payload is a cursor over a record payload that remembers the first error.
type payload struct {
	b   []byte
	err error
}

func (p *payload) fail(format string, args ...any) {
	if p.err == nil {
		p.err = fmt.Errorf(format, args...)
	}
}

func (p *payload) u64() uint64 {
	if p.err != nil {
		return 0
	}
	if len(p.b) < 8 {
		p.fail("payload too short for i64")
		return 0
	}
	v := binary.LittleEndian.Uint64(p.b)
	p.b = p.b[8:]
	return v
}

func (p *payload) bytes() []byte {
	if p.err != nil {
		return nil
	}
	n, k := binary.Uvarint(p.b)
	if k <= 0 {
		p.fail("invalid length varint")
		return nil
	}
	p.b = p.b[k:]
	if n > uint64(len(p.b)) {
		p.fail("field length %d exceeds payload", n)
		return nil
	}
	out := make([]byte, n)
	copy(out, p.b[:n])
	p.b = p.b[n:]
	return out
}

func (p *payload) string() string {
	b := p.bytes()
	if p.err == nil && !utf8.Valid(b) {
		p.fail("string field is not valid UTF-8")
	}
	return string(b)
}

// Clone returns a deep copy of r.
func (r Record) Clone() Record {
	if r.Data != nil {
		r.Data = append([]byte(nil), r.Data...)
	}
	return r
}

// MarshalText encodes known types by name and others as "0xNN".
func (t Type) MarshalText() ([]byte, error) {
	if n, ok := typeNames[t]; ok {
		return []byte(n), nil
	}
	return []byte(fmt.Sprintf("0x%02x", uint8(t))), nil
}

// UnmarshalText is the inverse of MarshalText.
func (t *Type) UnmarshalText(b []byte) error {
	for k, n := range typeNames {
		if n == string(b) {
			*t = k
			return nil
		}
	}
	var v uint8
	if _, err := fmt.Sscanf(string(b), "0x%02x", &v); err != nil {
		return fmt.Errorf("journal: unknown record type %q", b)
	}
	*t = Type(v)
	return nil
}
