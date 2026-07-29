package wire

import "encoding/binary"

// Every payload is fixed-width big-endian. Encoders append into a caller buffer
// so a hot outbound path can reuse one; decoders take a slice and never retain
// it, so a reader can reuse its receive buffer too.

// --- Enter ---

// EncodeEnter appends an Enter payload to dst.
func EncodeEnter(dst []byte, m Enter) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, EnterLen)...)
	b := dst[base:]

	b[0] = m.Version
	if err := putFixed(b[1:1+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	off := 1 + ClOrdIDLen
	if err := putFixed(b[off:off+SymbolLen], m.Symbol); err != nil {
		return nil, err
	}
	off += SymbolLen
	b[off] = m.Side
	b[off+1] = m.Type
	b[off+2] = m.TIF
	b[off+3] = putBool(m.PostOnly)
	off += 4
	binary.BigEndian.PutUint64(b[off:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.Quantity))
	return dst, nil
}

// DecodeEnter reads an Enter payload from src.
func DecodeEnter(src []byte) (Enter, error) {
	if len(src) < EnterLen {
		return Enter{}, ErrShort
	}
	off := 1 + ClOrdIDLen
	m := Enter{
		Version: src[0],
		ClOrdID: getFixed(src[1 : 1+ClOrdIDLen]),
		Symbol:  getFixed(src[off : off+SymbolLen]),
	}
	off += SymbolLen
	m.Side = src[off]
	m.Type = src[off+1]
	m.TIF = src[off+2]
	m.PostOnly = src[off+3] != 0
	off += 4
	m.Price = int64(binary.BigEndian.Uint64(src[off:]))
	m.Quantity = int64(binary.BigEndian.Uint64(src[off+8:]))
	return m, nil
}

// --- Cancel ---

// EncodeCancel appends a Cancel payload to dst.
func EncodeCancel(dst []byte, m Cancel) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, CancelLen)...)
	b := dst[base:]
	b[0] = m.Version
	if err := putFixed(b[1:1+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	return dst, nil
}

// DecodeCancel reads a Cancel payload from src.
func DecodeCancel(src []byte) (Cancel, error) {
	if len(src) < CancelLen {
		return Cancel{}, ErrShort
	}
	return Cancel{Version: src[0], ClOrdID: getFixed(src[1 : 1+ClOrdIDLen])}, nil
}

// --- Accepted ---

// EncodeAccepted appends an Accepted payload to dst.
func EncodeAccepted(dst []byte, m Accepted) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, AcceptedLen)...)
	b := dst[base:]
	b[0] = m.Version
	if err := putFixed(b[1:1+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	off := 1 + ClOrdIDLen
	binary.BigEndian.PutUint64(b[off:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.Quantity))
	b[off+16] = m.Side
	return dst, nil
}

// DecodeAccepted reads an Accepted payload from src.
func DecodeAccepted(src []byte) (Accepted, error) {
	if len(src) < AcceptedLen {
		return Accepted{}, ErrShort
	}
	off := 1 + ClOrdIDLen
	return Accepted{
		Version:  src[0],
		ClOrdID:  getFixed(src[1:off]),
		Price:    int64(binary.BigEndian.Uint64(src[off:])),
		Quantity: int64(binary.BigEndian.Uint64(src[off+8:])),
		Side:     src[off+16],
	}, nil
}

// --- Executed ---

// EncodeExecuted appends an Executed payload to dst.
func EncodeExecuted(dst []byte, m Executed) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, ExecutedLen)...)
	b := dst[base:]
	b[0] = m.Version
	if err := putFixed(b[1:1+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	off := 1 + ClOrdIDLen
	binary.BigEndian.PutUint64(b[off:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.Quantity))
	binary.BigEndian.PutUint64(b[off+16:], uint64(m.LeavesQty))
	b[off+24] = m.Aggressor
	return dst, nil
}

// DecodeExecuted reads an Executed payload from src.
func DecodeExecuted(src []byte) (Executed, error) {
	if len(src) < ExecutedLen {
		return Executed{}, ErrShort
	}
	off := 1 + ClOrdIDLen
	return Executed{
		Version:   src[0],
		ClOrdID:   getFixed(src[1:off]),
		Price:     int64(binary.BigEndian.Uint64(src[off:])),
		Quantity:  int64(binary.BigEndian.Uint64(src[off+8:])),
		LeavesQty: int64(binary.BigEndian.Uint64(src[off+16:])),
		Aggressor: src[off+24],
	}, nil
}

// --- Rejected / Canceled / CmdReject (identical shape, distinct types) ---

func encodeIDReason(dst []byte, width int, version uint8, clOrdID string, reason uint16) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, width)...)
	b := dst[base:]
	b[0] = version
	if err := putFixed(b[1:1+ClOrdIDLen], clOrdID); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint16(b[1+ClOrdIDLen:], reason)
	return dst, nil
}

func decodeIDReason(src []byte, width int) (uint8, string, uint16, error) {
	if len(src) < width {
		return 0, "", 0, ErrShort
	}
	return src[0], getFixed(src[1 : 1+ClOrdIDLen]), binary.BigEndian.Uint16(src[1+ClOrdIDLen:]), nil
}

// EncodeRejected appends a Rejected payload to dst.
func EncodeRejected(dst []byte, m Rejected) ([]byte, error) {
	return encodeIDReason(dst, RejectedLen, m.Version, m.ClOrdID, m.Reason)
}

// DecodeRejected reads a Rejected payload from src.
func DecodeRejected(src []byte) (Rejected, error) {
	v, id, r, err := decodeIDReason(src, RejectedLen)
	return Rejected{Version: v, ClOrdID: id, Reason: r}, err
}

// EncodeCanceled appends a Canceled payload to dst.
func EncodeCanceled(dst []byte, m Canceled) ([]byte, error) {
	return encodeIDReason(dst, CanceledLen, m.Version, m.ClOrdID, m.Reason)
}

// DecodeCanceled reads a Canceled payload from src.
func DecodeCanceled(src []byte) (Canceled, error) {
	v, id, r, err := decodeIDReason(src, CanceledLen)
	return Canceled{Version: v, ClOrdID: id, Reason: r}, err
}

// EncodeCmdReject appends a CmdReject payload to dst.
func EncodeCmdReject(dst []byte, m CmdReject) ([]byte, error) {
	return encodeIDReason(dst, CmdRejectLen, m.Version, m.ClOrdID, m.Reason)
}

// DecodeCmdReject reads a CmdReject payload from src.
func DecodeCmdReject(src []byte) (CmdReject, error) {
	v, id, r, err := decodeIDReason(src, CmdRejectLen)
	return CmdReject{Version: v, ClOrdID: id, Reason: r}, err
}

// --- Replaced ---

// EncodeReplaced appends a Replaced payload to dst.
func EncodeReplaced(dst []byte, m Replaced) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, ReplacedLen)...)
	b := dst[base:]
	b[0] = m.Version
	if err := putFixed(b[1:1+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(b[1+ClOrdIDLen:], uint64(m.LeavesQty))
	return dst, nil
}

// DecodeReplaced reads a Replaced payload from src.
func DecodeReplaced(src []byte) (Replaced, error) {
	if len(src) < ReplacedLen {
		return Replaced{}, ErrShort
	}
	return Replaced{
		Version:   src[0],
		ClOrdID:   getFixed(src[1 : 1+ClOrdIDLen]),
		LeavesQty: int64(binary.BigEndian.Uint64(src[1+ClOrdIDLen:])),
	}, nil
}
