// Package itch reads NASDAQ TotalView-ITCH 5.0 and rebuilds order books from it
// (docs/ITCH.md).
//
// ITCH reports what a venue's book did. Books applies those changes to
// pkg/orderbook books; it never matches them, because the trades already happened.
package itch

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Message is one ITCH message. Only the fields of the decoded types are set; any
// other type keeps its Type, Locate, Tracking and Timestamp. Reused by the caller:
// Reader.Next overwrites every field.
type Message struct {
	Type      byte
	Locate    uint16
	Tracking  uint16
	Timestamp uint64 // nanoseconds since midnight

	Ref       uint64 // order reference number; the original one for U
	NewRef    uint64 // U only
	Side      byte   // 'B' or 'S'; A and F only
	Shares    uint32 // A, F, U: the order's size; E, C: executed; X: cancelled
	Stock     [8]byte
	Price     uint32  // A, F, U: the order's price; C: the execution price. 4 implied decimals
	Match     uint64  // E, C
	Printable byte    // C
	MPID      [4]byte // F
}

// Message lengths in bytes, type byte included, from the ITCH 5.0 specification.
var lengths = [256]int{
	'A': 36, 'F': 40, 'E': 31, 'C': 36, 'X': 23, 'D': 19, 'U': 35,
}

// Decoded reports whether t is a type this package decodes beyond its header.
func Decoded(t byte) bool { return lengths[t] != 0 }

// ErrTruncated is wrapped by errors for input that ends inside a frame.
var ErrTruncated = errors.New("itch: input ends inside a message")

// Decode fills m from one message, without its length prefix. A decoded type at the
// wrong length is an error; other types need at least the 11-byte common header.
func Decode(b []byte, m *Message) error {
	if len(b) < 11 {
		return fmt.Errorf("itch: %d-byte message is shorter than the common header", len(b))
	}
	*m = Message{
		Type:      b[0],
		Locate:    binary.BigEndian.Uint16(b[1:]),
		Tracking:  binary.BigEndian.Uint16(b[3:]),
		Timestamp: uint48(b[5:]),
	}
	want := lengths[m.Type]
	if want == 0 {
		return nil
	}
	if len(b) != want {
		return fmt.Errorf("itch: type %q is %d bytes, not %d", m.Type, len(b), want)
	}
	switch m.Type {
	case 'A', 'F':
		m.Ref = binary.BigEndian.Uint64(b[11:])
		m.Side = b[19]
		m.Shares = binary.BigEndian.Uint32(b[20:])
		copy(m.Stock[:], b[24:32])
		m.Price = binary.BigEndian.Uint32(b[32:])
		if m.Type == 'F' {
			copy(m.MPID[:], b[36:40])
		}
		if m.Side != 'B' && m.Side != 'S' {
			return fmt.Errorf("itch: add order %d has side %q", m.Ref, m.Side)
		}
	case 'E', 'C':
		m.Ref = binary.BigEndian.Uint64(b[11:])
		m.Shares = binary.BigEndian.Uint32(b[19:])
		m.Match = binary.BigEndian.Uint64(b[23:])
		if m.Type == 'C' {
			m.Printable = b[31]
			m.Price = binary.BigEndian.Uint32(b[32:])
		}
	case 'X':
		m.Ref = binary.BigEndian.Uint64(b[11:])
		m.Shares = binary.BigEndian.Uint32(b[19:])
	case 'D':
		m.Ref = binary.BigEndian.Uint64(b[11:])
	case 'U':
		m.Ref = binary.BigEndian.Uint64(b[11:])
		m.NewRef = binary.BigEndian.Uint64(b[19:])
		m.Shares = binary.BigEndian.Uint32(b[27:])
		m.Price = binary.BigEndian.Uint32(b[31:])
	}
	return nil
}

func uint48(b []byte) uint64 {
	return uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 | uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5])
}

// Append encodes m with its 2-byte length prefix. Only decoded types are encoded;
// any other type is an error. It exists for fixtures and tests.
func Append(b []byte, m *Message) ([]byte, error) {
	n := lengths[m.Type]
	if n == 0 {
		return b, fmt.Errorf("itch: cannot encode type %q", m.Type)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(n))
	start := len(b)
	b = append(b, make([]byte, n)...)
	p := b[start:]
	p[0] = m.Type
	binary.BigEndian.PutUint16(p[1:], m.Locate)
	binary.BigEndian.PutUint16(p[3:], m.Tracking)
	ts := m.Timestamp
	for i := 5; i >= 0; i-- {
		p[5+i] = byte(ts)
		ts >>= 8
	}
	switch m.Type {
	case 'A', 'F':
		binary.BigEndian.PutUint64(p[11:], m.Ref)
		p[19] = m.Side
		binary.BigEndian.PutUint32(p[20:], m.Shares)
		copy(p[24:32], m.Stock[:])
		binary.BigEndian.PutUint32(p[32:], m.Price)
		if m.Type == 'F' {
			copy(p[36:40], m.MPID[:])
		}
	case 'E', 'C':
		binary.BigEndian.PutUint64(p[11:], m.Ref)
		binary.BigEndian.PutUint32(p[19:], m.Shares)
		binary.BigEndian.PutUint64(p[23:], m.Match)
		if m.Type == 'C' {
			p[31] = m.Printable
			binary.BigEndian.PutUint32(p[32:], m.Price)
		}
	case 'X':
		binary.BigEndian.PutUint64(p[11:], m.Ref)
		binary.BigEndian.PutUint32(p[19:], m.Shares)
	case 'D':
		binary.BigEndian.PutUint64(p[11:], m.Ref)
	case 'U':
		binary.BigEndian.PutUint64(p[11:], m.Ref)
		binary.BigEndian.PutUint64(p[19:], m.NewRef)
		binary.BigEndian.PutUint32(p[27:], m.Shares)
		binary.BigEndian.PutUint32(p[31:], m.Price)
	}
	return b, nil
}

// Reader reads length-prefixed messages, the framing of NASDAQ's sample files.
type Reader struct {
	r      *bufio.Reader
	buf    [1 << 16]byte
	offset int64 // bytes consumed so far, for error messages
}

// NewReader reads framed ITCH from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 1<<20)}
}

// Next reads the next message into m. It returns io.EOF at a clean end of input,
// and an error wrapping ErrTruncated if the input ends inside a frame.
func (rd *Reader) Next(m *Message) error {
	var hdr [2]byte
	if _, err := io.ReadFull(rd.r, hdr[:]); err != nil {
		if err == io.EOF {
			return io.EOF
		}
		return fmt.Errorf("%w: length prefix at offset %d", ErrTruncated, rd.offset)
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(rd.r, rd.buf[:n]); err != nil {
		return fmt.Errorf("%w: %d-byte message at offset %d", ErrTruncated, n, rd.offset)
	}
	at := rd.offset
	rd.offset += 2 + int64(n)
	if err := Decode(rd.buf[:n], m); err != nil {
		return fmt.Errorf("%v (offset %d)", err, at)
	}
	return nil
}

// Offset is the number of bytes consumed so far.
func (rd *Reader) Offset() int64 { return rd.offset }
