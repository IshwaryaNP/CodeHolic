package src

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Record layout (little-endian):
// [CRC32: 4B][TotalLen: 4B][OpType: 1B][KeyLen: 2B][ValLen: 4B][Key][Value]
//
// Field offsets:
// 0..4   : CRC32 checksum (IEEE) covering hdr[4:15] + key + value
// 4..8   : TotalLen (uint32) = 15 + KeyLen + ValLen (total byte size of whole record)
// 8..9   : OpType (uint8) -> 1=Put, 2=Delete, 3=BatchBegin, 4=BatchCommit
// 9..11  : KeyLen (uint16)
// 11..15 : ValLen (uint32)
// 15..   : Key bytes
// 15+KeyLen..: Value bytes

const (
	// HeaderSize is the fixed size of a record header in bytes.
	HeaderSize = 15

	// Operation types.
	OpPut         byte = 1
	OpDelete      byte = 2
	OpBatchBegin  byte = 3
	OpBatchCommit byte = 4

	// MaxKeySize is the maximum permitted key length in bytes (uint16 max).
	MaxKeySize = 65535

	// MaxValueSize is the maximum permitted value size in bytes (64MB).
	MaxValueSize = 64 * 1024 * 1024
)

// Header represents the decoded fixed-size metadata of a log record.
type Header struct {
	CRC      uint32
	TotalLen uint32
	OpType   byte
	KeyLen   uint16
	ValLen   uint32
}

// Sentinels for validation errors.
var (
	ErrNotFound      = errors.New("record not found")
	ErrClosed        = errors.New("database is closed")
	ErrCorrupt       = errors.New("data is corrupted or invalid")
	ErrBroken        = errors.New("database is broken due to catastrophic write/truncate failure")
	ErrBatchTooLarge = errors.New("batch exceeds maximum allowed size")
	ErrKeyTooLarge   = errors.New("key exceeds maximum allowed size")
	ErrValueTooLarge = errors.New("value exceeds maximum allowed size")
	ErrEmptyKey      = errors.New("key cannot be empty")
	ErrDiskFull      = errors.New("disk full: write exceeds maximum permitted database file size")
)

// EncodeRecord appends an encoded record into dst and returns the modified slice.
// If cap(dst) is large enough, it performs zero heap allocations.
// dst = [existing...][CRC(4)][TotalLen(4)][OpType(1)][KeyLen(2)][ValLen(4)][key][val]
func EncodeRecord(dst []byte, op byte, key, val []byte) []byte {
	totalLen := HeaderSize + len(key) + len(val)
	start := len(dst)
	needed := start + totalLen

	if cap(dst) >= needed {
		dst = dst[:needed]
	} else {
		newDst := make([]byte, needed, needed+1024)
		copy(newDst, dst)
		dst = newDst
	}

	hdr := dst[start : start+HeaderSize]

	// Write header fields (except CRC at 0..4)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(totalLen))
	hdr[8] = op
	binary.LittleEndian.PutUint16(hdr[9:11], uint16(len(key)))
	binary.LittleEndian.PutUint32(hdr[11:15], uint32(len(val)))

	// Copy key and value
	copy(dst[start+HeaderSize:start+HeaderSize+len(key)], key)
	copy(dst[start+HeaderSize+len(key):start+totalLen], val)

	// Compute CRC32 incrementally: hdr[4:15], then key, then value
	c := crc32.ChecksumIEEE(hdr[4:15])
	if len(key) > 0 {
		c = crc32.Update(c, crc32.IEEETable, key)
	}
	if len(val) > 0 {
		c = crc32.Update(c, crc32.IEEETable, val)
	}

	binary.LittleEndian.PutUint32(hdr[0:4], c)
	return dst
}

// DecodeHeader decodes and validates a 15-byte record header.
// It performs validation using uint64 arithmetic to prevent integer overflows.
func DecodeHeader(hdr []byte) (Header, error) {
	if len(hdr) < HeaderSize {
		return Header{}, fmt.Errorf("%w: header too short (%d bytes)", ErrCorrupt, len(hdr))
	}

	h := Header{
		CRC:      binary.LittleEndian.Uint32(hdr[0:4]),
		TotalLen: binary.LittleEndian.Uint32(hdr[4:8]),
		OpType:   hdr[8],
		KeyLen:   binary.LittleEndian.Uint16(hdr[9:11]),
		ValLen:   binary.LittleEndian.Uint32(hdr[11:15]),
	}

	// Validate op range
	if h.OpType < OpPut || h.OpType > OpBatchCommit {
		return Header{}, fmt.Errorf("%w: invalid op type %d", ErrCorrupt, h.OpType)
	}

	// Validate TotalLen == 15 + KeyLen + ValLen using uint64 math
	expectedTotal := uint64(HeaderSize) + uint64(h.KeyLen) + uint64(h.ValLen)
	if uint64(h.TotalLen) != expectedTotal {
		return Header{}, fmt.Errorf("%w: TotalLen mismatch (got %d, expected %d)", ErrCorrupt, h.TotalLen, expectedTotal)
	}

	// ValLen bounds check
	if uint64(h.ValLen) > MaxValueSize {
		return Header{}, fmt.Errorf("%w: ValLen %d exceeds MaxValueSize", ErrCorrupt, h.ValLen)
	}

	// Delete must have ValLen == 0
	if h.OpType == OpDelete && h.ValLen != 0 {
		return Header{}, fmt.Errorf("%w: OpDelete must have ValLen == 0, got %d", ErrCorrupt, h.ValLen)
	}

	// Batch markers must have KeyLen == 0 && ValLen == 0
	if (h.OpType == OpBatchBegin || h.OpType == OpBatchCommit) && (h.KeyLen != 0 || h.ValLen != 0) {
		return Header{}, fmt.Errorf("%w: batch marker must have KeyLen=0 and ValLen=0", ErrCorrupt)
	}

	return h, nil
}

// VerifyCRC computes the CRC32 checksum over hdr[4:15] + key + val and checks
// if it matches the stored CRC in hdr[0:4]. Incremental computation avoids allocations.
func VerifyCRC(hdr, key, val []byte) bool {
	if len(hdr) < HeaderSize {
		return false
	}
	expected := binary.LittleEndian.Uint32(hdr[0:4])

	c := crc32.ChecksumIEEE(hdr[4:15])
	if len(key) > 0 {
		c = crc32.Update(c, crc32.IEEETable, key)
	}
	if len(val) > 0 {
		c = crc32.Update(c, crc32.IEEETable, val)
	}

	return c == expected
}
