package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Frame magic numbers (SPEC.md §3).
const (
	magicHeaderFrame = 0x184D2A5A
	magicBlockFrame  = 0x184D2A5B
	magicZstdFrame   = 0xFD2FB528
	magicSkippable   = 0x184D2A50 // 0x184D2A50 to 0x184D2A5F

	blockFrameLen  = 8 + 4 + 4 + 4 + 4 // first_seq, count, raw_len, data_len, header_crc
	rawBlockMax    = 128 << 10         // RFC 8878 §3.1.1.2.3: Block_Maximum_Size
	DefaultLevel   = 3
	DefaultBlockSz = 4 << 20
)

// Compression values of the header's "compression" key (SPEC.md §3.2).
const (
	CompressionZstd = "zstd"
	CompressionNone = "none"
)

func isSkippable(magic uint32) bool { return magic&0xFFFFFFF0 == magicSkippable }

// blockHeader is the user data of a block frame (SPEC.md §3.3).
type blockHeader struct {
	firstSeq uint64
	count    uint32
	rawLen   uint32
	dataLen  uint32
}

func (h blockHeader) append(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, magicBlockFrame)
	dst = binary.LittleEndian.AppendUint32(dst, blockFrameLen)
	start := len(dst)
	dst = binary.LittleEndian.AppendUint64(dst, h.firstSeq)
	dst = binary.LittleEndian.AppendUint32(dst, h.count)
	dst = binary.LittleEndian.AppendUint32(dst, h.rawLen)
	dst = binary.LittleEndian.AppendUint32(dst, h.dataLen)
	return binary.LittleEndian.AppendUint32(dst, checksum(dst[start:]))
}

// parseBlockHeader decodes and validates the user data of a block frame.
func parseBlockHeader(b []byte) (blockHeader, error) {
	if len(b) != blockFrameLen {
		return blockHeader{}, corrupt("block frame is %d bytes, want %d", len(b), blockFrameLen)
	}
	if checksum(b[:blockFrameLen-4]) != binary.LittleEndian.Uint32(b[blockFrameLen-4:]) {
		return blockHeader{}, corrupt("block header checksum mismatch")
	}
	h := blockHeader{
		firstSeq: binary.LittleEndian.Uint64(b),
		count:    binary.LittleEndian.Uint32(b[8:]),
		rawLen:   binary.LittleEndian.Uint32(b[12:]),
		dataLen:  binary.LittleEndian.Uint32(b[16:]),
	}
	switch {
	case h.count == 0:
		return h, corrupt("block at seq %d holds no records", h.firstSeq)
	case h.rawLen == 0 || h.rawLen > MaxBlockLen:
		return h, corrupt("block at seq %d has raw_len %d, limit is %d", h.firstSeq, h.rawLen, MaxBlockLen)
	case h.dataLen == 0 || h.dataLen > MaxBlockLen:
		return h, corrupt("block at seq %d has data_len %d, limit is %d", h.firstSeq, h.dataLen, MaxBlockLen)
	}
	return h, nil
}

var (
	encoders sync.Map // level -> *zstd.Encoder
	decoder  = sync.OnceValues(func() (*zstd.Decoder, error) {
		return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxBlockLen))
	})
)

func encoderFor(level int) (*zstd.Encoder, error) {
	if e, ok := encoders.Load(level); ok {
		return e.(*zstd.Encoder), nil
	}
	e, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderCRC(true),
		zstd.WithSingleSegment(true),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	actual, _ := encoders.LoadOrStore(level, e)
	return actual.(*zstd.Encoder), nil
}

// compressFrame encodes raw as one zstd frame that declares its content size and
// carries a content checksum.
func compressFrame(raw []byte, level int) ([]byte, error) {
	e, err := encoderFor(level)
	if err != nil {
		return nil, err
	}
	frame := e.EncodeAll(raw, nil)
	// EncodeAll writes the content size; check, since a reader will.
	if size, err := frameContentSize(frame); err != nil || size != uint64(len(raw)) {
		return nil, fmt.Errorf("journal: zstd encoder wrote a frame without a usable content size (%v)", err)
	}
	return frame, nil
}

// rawFrame wraps raw in a zstd frame of raw blocks, with no compression
// (SPEC.md §3.3): a single-segment frame with a 4-byte content size and a
// content checksum, whose blocks hold at most 128 KiB each.
func rawFrame(raw []byte) []byte {
	out := binary.LittleEndian.AppendUint32(nil, magicZstdFrame)
	// Frame_Content_Size_flag 2 (4 bytes), Single_Segment, Content_Checksum.
	out = append(out, 2<<6|1<<5|1<<2)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(raw)))
	for rest := raw; len(rest) > 0; {
		n := min(len(rest), rawBlockMax)
		hdr := uint32(n) << 3 // Block_Type 0 (raw)
		if n == len(rest) {
			hdr |= 1 // Last_Block
		}
		out = append(out, byte(hdr), byte(hdr>>8), byte(hdr>>16))
		out = append(out, rest[:n]...)
		rest = rest[n:]
	}
	return binary.LittleEndian.AppendUint32(out, uint32(xxh64(raw)))
}

// frameContentSize reads the content size a zstd frame declares, and checks the
// frame is one SPEC.md §3.3 allows: it declares its size, carries a content
// checksum and uses no dictionary.
func frameContentSize(frame []byte) (uint64, error) {
	if len(frame) < 6 || binary.LittleEndian.Uint32(frame) != magicZstdFrame {
		return 0, errors.New("data frame is not a zstd frame")
	}
	fhd := frame[4]
	fcsFlag, single, hasCRC, dict := fhd>>6, fhd>>5&1 == 1, fhd>>2&1 == 1, fhd&3
	switch {
	case fhd&0x08 != 0:
		return 0, errors.New("data frame sets a reserved bit")
	case dict != 0:
		return 0, errors.New("data frame uses a dictionary")
	case !hasCRC:
		return 0, errors.New("data frame has no content checksum")
	case fcsFlag == 0 && !single:
		return 0, errors.New("data frame does not declare its content size")
	}
	pos := 5
	if !single {
		pos++ // Window_Descriptor
	}
	size := [4]int{1, 2, 4, 8}[fcsFlag]
	if len(frame) < pos+size {
		return 0, errors.New("data frame header is cut short")
	}
	var n uint64
	for i := size - 1; i >= 0; i-- {
		n = n<<8 | uint64(frame[pos+i])
	}
	if fcsFlag == 1 {
		n += 256
	}
	return n, nil
}

// decompressFrame decodes one data frame, checking its content size and
// checksum, and returns the block's raw records.
func decompressFrame(frame []byte, rawLen uint32) ([]byte, error) {
	size, err := frameContentSize(frame)
	if err != nil {
		return nil, corrupt("%v", err)
	}
	if size != uint64(rawLen) {
		return nil, corrupt("data frame declares %d bytes, block header says %d", size, rawLen)
	}
	d, err := decoder()
	if err != nil {
		return nil, err
	}
	raw, err := d.DecodeAll(frame, make([]byte, 0, rawLen))
	if err != nil {
		return nil, corrupt("data frame does not decode: %v", err)
	}
	if len(raw) != int(rawLen) {
		return nil, corrupt("data frame decodes to %d bytes, block header says %d", len(raw), rawLen)
	}
	return raw, nil
}

// xxh64 is XXH64 with seed 0, the hash of a zstd content checksum. Only its low
// 32 bits are stored, and only the uncompressed writer needs to compute it.
func xxh64(b []byte) uint64 {
	const (
		p1 uint64 = 11400714785074694791
		p2 uint64 = 14029467366897019727
		p3 uint64 = 1609587929392839161
		p4 uint64 = 9650029242287828579
		p5 uint64 = 2870177450012600261
	)
	round := func(acc, in uint64) uint64 { return bits.RotateLeft64(acc+in*p2, 31) * p1 }
	merge := func(h, v uint64) uint64 { return (h^round(0, v))*p1 + p4 }
	n := len(b)
	var h uint64
	if n >= 32 {
		zero := uint64(0) // constant arithmetic would overflow; wrap at run time
		v1, v2, v3, v4 := zero+p1, zero+p2, zero, zero-p1
		v1 += p2
		for ; len(b) >= 32; b = b[32:] {
			v1 = round(v1, binary.LittleEndian.Uint64(b))
			v2 = round(v2, binary.LittleEndian.Uint64(b[8:]))
			v3 = round(v3, binary.LittleEndian.Uint64(b[16:]))
			v4 = round(v4, binary.LittleEndian.Uint64(b[24:]))
		}
		h = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) + bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		h = merge(h, v1)
		h = merge(h, v2)
		h = merge(h, v3)
		h = merge(h, v4)
	} else {
		h = p5
	}
	h += uint64(n)
	for ; len(b) >= 8; b = b[8:] {
		h ^= round(0, binary.LittleEndian.Uint64(b))
		h = bits.RotateLeft64(h, 27)*p1 + p4
	}
	if len(b) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(b)) * p1
		h = bits.RotateLeft64(h, 23)*p2 + p3
		b = b[4:]
	}
	for _, c := range b {
		h ^= uint64(c) * p5
		h = bits.RotateLeft64(h, 11) * p1
	}
	h ^= h >> 33
	h *= p2
	h ^= h >> 29
	h *= p3
	h ^= h >> 32
	return h
}

// appendBlock appends a block frame and its data frame holding raw to dst.
func appendBlock(dst []byte, firstSeq uint64, count int, raw []byte, compression string, level int) ([]byte, error) {
	if len(raw) > MaxBlockLen {
		return nil, fmt.Errorf("journal: block holds %d bytes, limit is %d", len(raw), MaxBlockLen)
	}
	var frame []byte
	if compression == CompressionNone {
		frame = rawFrame(raw)
	} else {
		var err error
		if frame, err = compressFrame(raw, level); err != nil {
			return nil, err
		}
	}
	dst = blockHeader{firstSeq, uint32(count), uint32(len(raw)), uint32(len(frame))}.append(dst)
	return append(dst, frame...), nil
}
