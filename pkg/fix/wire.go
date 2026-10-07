// Package fix is FIX 4.4 order entry for the engine (docs/FIX.md): the tag=value
// wire with BodyLength and CheckSum verified, NewOrderSingle and OrderCancelRequest
// in, ExecutionReport and OrderCancelReject out of the event stream.
//
// The session layer (logon, heartbeats, sequence recovery, resend) is not here. A
// session engine hands this package one application message at a time.
package fix

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// SOH ends every field.
const SOH = 0x01

const beginString = "FIX.4.4"

var (
	// ErrIncomplete means the input holds only part of a message: read more.
	ErrIncomplete = errors.New("fix: incomplete message")
	// ErrBodyLength means BodyLength (9) does not match the body.
	ErrBodyLength = errors.New("fix: BodyLength does not match the body")
	// ErrCheckSum means CheckSum (10) does not match the message.
	ErrCheckSum = errors.New("fix: CheckSum does not match the message")
	// ErrBeginString means the message is not FIX.4.4.
	ErrBeginString = errors.New("fix: BeginString is not FIX.4.4")
	// ErrGarbled means a field or the header order is malformed.
	ErrGarbled = errors.New("fix: garbled message")
)

// Field is one tag=value pair.
type Field struct {
	Tag   int
	Value string
}

// Message is a parsed message's fields in wire order, header and trailer included.
// Repeated tags are kept; order and repetition are part of FIX.
type Message struct {
	Fields []Field
}

// Get returns the first value of tag.
func (m Message) Get(tag int) (string, bool) {
	for _, f := range m.Fields {
		if f.Tag == tag {
			return f.Value, true
		}
	}
	return "", false
}

// Type is MsgType (35).
func (m Message) Type() string {
	v, _ := m.Get(35)
	return v
}

var prefix = []byte("8=" + beginString + "\x01")

// Parse reads one message from the front of b and returns it with the number of
// bytes it used. It verifies BeginString, BodyLength and CheckSum, and that 8, 9 and
// 35 open the message in that order and 10 closes it.
func Parse(b []byte) (Message, int, error) {
	if len(b) < len(prefix) {
		if bytes.HasPrefix(prefix, b) {
			return Message{}, 0, ErrIncomplete
		}
		return Message{}, 0, ErrBeginString
	}
	if !bytes.HasPrefix(b, prefix) {
		if bytes.HasPrefix(b, []byte("8=")) {
			return Message{}, 0, ErrBeginString
		}
		return Message{}, 0, fmt.Errorf("%w: does not start with 8=", ErrGarbled)
	}
	rest := b[len(prefix):]
	if len(rest) < 2 {
		return Message{}, 0, ErrIncomplete
	}
	if !bytes.HasPrefix(rest, []byte("9=")) {
		return Message{}, 0, fmt.Errorf("%w: BodyLength (9) is not the second field", ErrGarbled)
	}
	end := bytes.IndexByte(rest, SOH)
	if end < 0 {
		return Message{}, 0, ErrIncomplete
	}
	n, err := strconv.Atoi(string(rest[2:end]))
	if err != nil || n <= 0 {
		return Message{}, 0, fmt.Errorf("%w: BodyLength %q", ErrGarbled, rest[2:end])
	}
	bodyStart := len(prefix) + end + 1
	bodyEnd := bodyStart + n
	const trailer = len("10=000\x01")
	if len(b) < bodyEnd+trailer {
		// Not enough bytes for the length it claims. If what is there already
		// shows the trailer elsewhere, the length is wrong rather than the input
		// short.
		if i := bytes.Index(b[bodyStart:], []byte("\x0110=")); i >= 0 && bodyStart+i+1 < bodyEnd && len(b) >= bodyStart+i+1+trailer {
			return Message{}, 0, ErrBodyLength
		}
		return Message{}, 0, ErrIncomplete
	}
	if b[bodyEnd-1] != SOH || !bytes.HasPrefix(b[bodyEnd:], []byte("10=")) {
		return Message{}, 0, ErrBodyLength
	}
	if b[bodyEnd+trailer-1] != SOH {
		return Message{}, 0, fmt.Errorf("%w: CheckSum is not three digits", ErrGarbled)
	}
	want, err := strconv.Atoi(string(b[bodyEnd+3 : bodyEnd+6]))
	if err != nil {
		return Message{}, 0, fmt.Errorf("%w: CheckSum %q", ErrGarbled, b[bodyEnd+3:bodyEnd+6])
	}
	if checksum(b[:bodyEnd]) != want {
		return Message{}, 0, ErrCheckSum
	}

	var m Message
	for p := 0; p < bodyEnd+trailer; {
		e := bytes.IndexByte(b[p:], SOH)
		eq := bytes.IndexByte(b[p:p+e], '=')
		if eq <= 0 {
			return Message{}, 0, fmt.Errorf("%w: field %q has no tag", ErrGarbled, b[p:p+e])
		}
		tag, err := strconv.Atoi(string(b[p : p+eq]))
		if err != nil || tag <= 0 {
			return Message{}, 0, fmt.Errorf("%w: tag %q", ErrGarbled, b[p:p+eq])
		}
		m.Fields = append(m.Fields, Field{tag, string(b[p+eq+1 : p+e])})
		p += e + 1
	}
	if len(m.Fields) < 4 || m.Fields[2].Tag != 35 {
		return Message{}, 0, fmt.Errorf("%w: MsgType (35) is not the third field", ErrGarbled)
	}
	return m, bodyEnd + trailer, nil
}

func checksum(b []byte) int {
	var s int
	for _, c := range b {
		s += int(c)
	}
	return s % 256
}

// Builder writes one message: MsgType, then the fields in the order added, framed
// with BodyLength and CheckSum.
type Builder struct {
	body []byte
}

// NewBuilder starts a message of the given MsgType.
func NewBuilder(msgType string) *Builder {
	b := &Builder{}
	return b.Add(35, msgType)
}

// Add appends a field.
func (b *Builder) Add(tag int, value string) *Builder {
	b.body = strconv.AppendInt(b.body, int64(tag), 10)
	b.body = append(b.body, '=')
	b.body = append(b.body, value...)
	b.body = append(b.body, SOH)
	return b
}

// AddInt appends an integer field.
func (b *Builder) AddInt(tag int, v int64) *Builder {
	return b.Add(tag, strconv.FormatInt(v, 10))
}

// Bytes returns the framed message.
func (b *Builder) Bytes() []byte {
	out := append([]byte{}, prefix...)
	out = append(out, "9="...)
	out = strconv.AppendInt(out, int64(len(b.body)), 10)
	out = append(out, SOH)
	out = append(out, b.body...)
	return append(out, fmt.Sprintf("10=%03d\x01", checksum(out))...)
}
