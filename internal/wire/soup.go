package wire

import (
	"encoding/binary"
	"errors"
	"io"
)

// SoupBinTCP framing: a 2-byte big-endian length covering the type byte and the
// payload, then the type byte, then the payload. The length excludes itself, so
// the smallest legal packet is 3 bytes on the wire and carries one type byte.
//
// MaxPayload bounds a single packet so a hostile or corrupt length cannot make
// the server allocate arbitrarily. It is far above any message this protocol
// defines; the point is to have a ceiling at all.
const MaxPayload = 4096

var ErrPacketTooBig = errors.New("wire: packet exceeds MaxPayload")

// Packet is one framed unit.
type Packet struct {
	Type    byte
	Payload []byte
}

// WritePacket frames and writes one packet.
func WritePacket(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrPacketTooBig
	}
	var hdr [3]byte
	binary.BigEndian.PutUint16(hdr[:2], uint16(len(payload)+1))
	hdr[2] = typ
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadPacket reads one packet into buf, returning a slice of buf holding the
// payload. buf is reused across calls; the returned payload is only valid until
// the next read, which is why callers decode immediately rather than retaining.
//
// A length of zero is malformed rather than an empty packet, since the type byte
// is always counted.
func ReadPacket(r io.Reader, buf []byte) (Packet, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Packet{}, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 {
		return Packet{}, ErrBadPacket
	}
	if n-1 > MaxPayload {
		return Packet{}, ErrPacketTooBig
	}
	var typ [1]byte
	if _, err := io.ReadFull(r, typ[:]); err != nil {
		return Packet{}, err
	}
	body := n - 1
	if body > len(buf) {
		return Packet{}, ErrShort
	}
	if body > 0 {
		if _, err := io.ReadFull(r, buf[:body]); err != nil {
			return Packet{}, err
		}
	}
	return Packet{Type: typ[0], Payload: buf[:body]}, nil
}

// --- login ---

// LoginRequest is the client's opening packet. Session and Sequence carry the
// resume cursor: an empty Session means "start me a new one", and a non-empty
// Session with a Sequence asks to resume that stream from that point.
//
// Password is a shared secret checked by the server. It is sent in the clear, so
// this protocol assumes a trusted network or a TLS wrapper below it — stated
// plainly here rather than left for a reader to assume either way.
type LoginRequest struct {
	Username string
	Password string
	Session  string
	Sequence uint64
}

const (
	usernameLen = 16
	passwordLen = 16
	sessionLen  = 10

	// LoginRequestLen is the encoded width of a LoginRequest.
	LoginRequestLen = usernameLen + passwordLen + sessionLen + 8
)

// EncodeLoginRequest appends a LoginRequest to dst.
func EncodeLoginRequest(dst []byte, m LoginRequest) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, LoginRequestLen)...)
	b := dst[base:]
	if err := putFixed(b[:usernameLen], m.Username); err != nil {
		return nil, err
	}
	off := usernameLen
	if err := putFixed(b[off:off+passwordLen], m.Password); err != nil {
		return nil, err
	}
	off += passwordLen
	if err := putFixed(b[off:off+sessionLen], m.Session); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(b[off+sessionLen:], m.Sequence)
	return dst, nil
}

// DecodeLoginRequest reads a LoginRequest from src.
func DecodeLoginRequest(src []byte) (LoginRequest, error) {
	if len(src) < LoginRequestLen {
		return LoginRequest{}, ErrShort
	}
	off := usernameLen
	m := LoginRequest{Username: getFixed(src[:usernameLen])}
	m.Password = getFixed(src[off : off+passwordLen])
	off += passwordLen
	m.Session = getFixed(src[off : off+sessionLen])
	m.Sequence = binary.BigEndian.Uint64(src[off+sessionLen:])
	return m, nil
}

// LoginAccepted tells the client which stream it is attached to and where that
// stream now starts. Session identifies the venue incarnation: a restarted venue
// mints a new one, so a client holding a stale cursor is told plainly rather than
// being served different content under sequence numbers it believes it has.
type LoginAccepted struct {
	Session  string
	Sequence uint64
}

// LoginAcceptedLen is the encoded width of a LoginAccepted.
const LoginAcceptedLen = sessionLen + 8

// EncodeLoginAccepted appends a LoginAccepted to dst.
func EncodeLoginAccepted(dst []byte, m LoginAccepted) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, LoginAcceptedLen)...)
	b := dst[base:]
	if err := putFixed(b[:sessionLen], m.Session); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(b[sessionLen:], m.Sequence)
	return dst, nil
}

// DecodeLoginAccepted reads a LoginAccepted from src.
func DecodeLoginAccepted(src []byte) (LoginAccepted, error) {
	if len(src) < LoginAcceptedLen {
		return LoginAccepted{}, ErrShort
	}
	return LoginAccepted{
		Session:  getFixed(src[:sessionLen]),
		Sequence: binary.BigEndian.Uint64(src[sessionLen:]),
	}, nil
}

// Login rejection codes.
const (
	RejectNotAuthorised byte = 'A' // bad username or password
	RejectNoSession     byte = 'S' // the requested session is not this incarnation
	RejectBadSequence   byte = 'Q' // the requested sequence is no longer retained
)
