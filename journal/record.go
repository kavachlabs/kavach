// Package journal implements the Kavach journal format described in SPEC.md.
package journal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"unicode/utf8"
)

// Format version written by this package.
const (
	Major = 0
	Minor = 2
)

// Magic is the 8-byte signature at the start of the header frame's data.
var Magic = [8]byte{0x89, 'K', 'V', 'J', '\r', '\n', 0x1a, '\n'}

// Limits from SPEC.md §3.
const (
	MaxMetaLen  = 1 << 20
	MaxBodyLen  = 16 << 20
	MaxBlockLen = 64 << 20 // raw_len and data_len of a block
	minBodyLen  = 10       // type + flags + seq
)

// Type is a record type code.
type Type uint8

// Record types defined by SPEC.md §4.
const (
	TypeInput       Type = 0x01
	TypeClock       Type = 0x02
	TypeRand        Type = 0x03
	TypeOutput      Type = 0x04
	TypeMarker      Type = 0x05
	TypeSnapshot    Type = 0x06
	TypeGateway     Type = 0x07
	TypeEnvironment Type = 0x08
	TypeConfig      Type = 0x09
)

// FlagCritical marks a record that readers must understand (SPEC.md §7).
const FlagCritical uint8 = 1 << 0

var typeNames = map[Type]string{
	TypeInput:       "input",
	TypeClock:       "clock",
	TypeRand:        "rand",
	TypeOutput:      "output",
	TypeMarker:      "marker",
	TypeSnapshot:    "snapshot",
	TypeGateway:     "gateway",
	TypeEnvironment: "environment",
	TypeConfig:      "config",
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
	MarkerCrash     = "crash"
	MarkerExit      = "exit"
	MarkerDropped   = "dropped"
)

// Scope values of an output or gateway (SPEC.md §4.4, §4.7).
const (
	ScopeRemote uint8 = 0
	ScopeLocal  uint8 = 1
)

// Forms of an environment fact (SPEC.md §4.8).
const (
	FactValue  uint8 = 0 // Value is the value
	FactSHA256 uint8 = 1 // Value is the SHA-256 of the value
	FactUnset  uint8 = 2 // the fact was removed or is unset
)

// Fact is one entry of an environment record.
type Fact struct {
	Key   string `json:"key"`
	Form  uint8  `json:"form"`
	Value []byte `json:"value,omitempty"`
}

// SortFacts orders facts by key, the order writers use for determinism.
func SortFacts(facts []Fact) {
	sort.Slice(facts, func(i, j int) bool { return facts[i].Key < facts[j].Key })
}

// Record is one decoded journal record. Which fields are meaningful depends on Type:
//
//	input:       Source, Position, Data
//	clock:       UnixNanos
//	rand:        Data
//	output:      Sink, Data, Scope
//	marker:      Kind, Message, Data
//	snapshot:    Data
//	gateway:     Gateway, Request, Response, Error, Scope
//	environment: Facts
//	config:      Key, Present, Value, Source
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

	Gateway  string `json:"gateway,omitempty"`
	Request  []byte `json:"request,omitempty"`
	Response []byte `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
	Scope    uint8  `json:"scope,omitempty"`

	Facts []Fact `json:"facts,omitempty"`

	Key     string `json:"key,omitempty"`
	Present bool   `json:"present,omitempty"`
	Value   []byte `json:"value,omitempty"`
}

// Critical reports whether the record's critical flag is set.
func (r Record) Critical() bool { return r.Flags&FlagCritical != 0 }

// MarshalJSON writes the fields of r's type. Unlike the struct tags alone, it
// keeps a zero scope and a false present flag, which carry meaning.
func (r Record) MarshalJSON() ([]byte, error) {
	type plain Record
	out := struct {
		plain
		Scope   *uint8 `json:"scope,omitempty"`
		Present *bool  `json:"present,omitempty"`
	}{plain: plain(r)}
	switch r.Type {
	case TypeOutput, TypeGateway:
		out.Scope = &r.Scope
	case TypeConfig:
		out.Present = &r.Present
	}
	return json.Marshal(out)
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func checksum(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// checkRecord validates the parts of r that are the same everywhere.
func checkRecord(r Record) error {
	if r.Type == 0 {
		return errors.New("record type 0 is invalid")
	}
	if r.Flags&^FlagCritical != 0 {
		return fmt.Errorf("unsupported flag bits 0x%02x", r.Flags)
	}
	return nil
}

// AppendPayload appends the type-specific payload of r to dst.
func AppendPayload(dst []byte, r Record) ([]byte, error) {
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
		dst = append(dst, r.Scope)
	case TypeMarker:
		dst = appendString(dst, r.Kind)
		dst = appendString(dst, r.Message)
		dst = appendBytes(dst, r.Data)
	case TypeGateway:
		dst = appendString(dst, r.Gateway)
		dst = appendBytes(dst, r.Request)
		dst = appendBytes(dst, r.Response)
		dst = appendString(dst, r.Error)
		dst = append(dst, r.Scope)
	case TypeEnvironment:
		dst = binary.AppendUvarint(dst, uint64(len(r.Facts)))
		for _, f := range r.Facts {
			dst = appendString(dst, f.Key)
			dst = append(dst, f.Form)
			dst = appendBytes(dst, f.Value)
		}
	case TypeConfig:
		dst = appendString(dst, r.Key)
		if r.Present {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
		dst = appendBytes(dst, r.Value)
		dst = appendString(dst, r.Source)
	default:
		// Extension types carry an opaque payload.
		dst = append(dst, r.Data...)
	}
	return dst, nil
}

// ParsePayload decodes the payload of a record of the given type and flags. It
// is the inverse of AppendPayload; the result has Seq 0. Unknown types keep
// their raw payload in Data.
func ParsePayload(ty Type, flags uint8, b []byte) (Record, error) {
	r := Record{Type: ty, Flags: flags}
	if err := checkRecord(r); err != nil {
		return r, err
	}
	p := &payload{b: b}
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
		r.Scope = p.u8()
	case TypeMarker:
		r.Kind = p.string()
		r.Message = p.string()
		r.Data = p.bytes()
	case TypeGateway:
		r.Gateway = p.string()
		r.Request = p.bytes()
		r.Response = p.bytes()
		r.Error = p.string()
		r.Scope = p.u8()
	case TypeEnvironment:
		r.Facts = p.facts()
	case TypeConfig:
		r.Key = p.string()
		switch v := p.u8(); v {
		case 0:
		case 1:
			r.Present = true
		default:
			p.fail("config present flag is %d", v)
		}
		r.Value = p.bytes()
		r.Source = p.string()
	default:
		r.Data = append([]byte(nil), b...)
		return r, nil
	}
	// Bytes after the known fields are ignored (SPEC.md §4).
	return r, p.err
}

// appendBody appends the encoded record body (type, flags, seq, payload) to dst.
func appendBody(dst []byte, r Record) ([]byte, error) {
	if err := checkRecord(r); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	dst = append(dst, byte(r.Type), r.Flags)
	dst = binary.LittleEndian.AppendUint64(dst, r.Seq)
	return AppendPayload(dst, r)
}

// AppendFrame appends a complete framed record (length, body) to dst.
func AppendFrame(dst []byte, r Record) ([]byte, error) {
	body, err := appendBody(nil, r)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyLen {
		return nil, fmt.Errorf("journal: record body is %d bytes, limit is %d", len(body), MaxBodyLen)
	}
	dst = binary.AppendUvarint(dst, uint64(len(body)))
	return append(dst, body...), nil
}

// parseBody decodes a record body.
func parseBody(body []byte) (Record, error) {
	seq := binary.LittleEndian.Uint64(body[2:10])
	r, err := ParsePayload(Type(body[0]), body[1], body[10:])
	r.Seq = seq
	return r, err
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

func (p *payload) u8() uint8 {
	if p.err != nil {
		return 0
	}
	if len(p.b) < 1 {
		p.fail("payload too short for u8")
		return 0
	}
	v := p.b[0]
	p.b = p.b[1:]
	return v
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

func (p *payload) uvarint() uint64 {
	if p.err != nil {
		return 0
	}
	n, k := binary.Uvarint(p.b)
	if k <= 0 {
		p.fail("invalid varint")
		return 0
	}
	p.b = p.b[k:]
	return n
}

func (p *payload) bytes() []byte {
	n := p.uvarint()
	if p.err != nil {
		return nil
	}
	if n > uint64(len(p.b)) {
		p.fail("field length %d exceeds payload", n)
		return nil
	}
	if n == 0 {
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

// facts reads the body of an environment record: a count and that many facts,
// whose keys must be distinct.
func (p *payload) facts() []Fact {
	n := p.uvarint()
	if p.err != nil {
		return nil
	}
	// A fact is at least 3 bytes, which bounds what a corrupt count can allocate.
	if n > uint64(len(p.b))/3 {
		p.fail("environment holds %d facts but only %d bytes", n, len(p.b))
		return nil
	}
	facts := make([]Fact, 0, n)
	seen := make(map[string]bool, n)
	for i := uint64(0); i < n; i++ {
		f := Fact{Key: p.string(), Form: p.u8(), Value: p.bytes()}
		if p.err != nil {
			return nil
		}
		if f.Form > FactUnset {
			p.fail("fact %q has unknown form %d", f.Key, f.Form)
			return nil
		}
		if seen[f.Key] {
			p.fail("environment repeats key %q", f.Key)
			return nil
		}
		seen[f.Key] = true
		facts = append(facts, f)
	}
	return facts
}

// Clone returns a deep copy of r.
func (r Record) Clone() Record {
	r.Data = cloneBytes(r.Data)
	r.Request = cloneBytes(r.Request)
	r.Response = cloneBytes(r.Response)
	r.Value = cloneBytes(r.Value)
	if r.Facts != nil {
		facts := make([]Fact, len(r.Facts))
		for i, f := range r.Facts {
			f.Value = cloneBytes(f.Value)
			facts[i] = f
		}
		r.Facts = facts
	}
	return r
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
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
