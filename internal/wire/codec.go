package wire

import "encoding/binary"

// Every payload is fixed-width big-endian and begins with two bytes: the message
// type, then the protocol version. Encoders append into a caller buffer so a hot
// outbound path can reuse one; decoders take a slice and never retain it, so a
// reader can reuse its receive buffer too.
//
// Decoders verify the type byte. A payload that decodes cleanly as the wrong
// message is the failure mode this header exists to prevent.

// header writes the type and version and returns the offset of the body.
func header(b []byte, msgType, version uint8) int {
	b[0] = msgType
	b[1] = version
	return 2
}

// checkHeader verifies a payload is the message the caller asked for.
func checkHeader(src []byte, width int, want uint8) error {
	if len(src) < width {
		return ErrShort
	}
	if src[0] != want {
		return ErrBadType
	}
	return nil
}

// --- Enter ---

// EncodeEnter appends an Enter payload to dst.
func EncodeEnter(dst []byte, m Enter) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, EnterLen)...)
	b := dst[base:]

	off := header(b, MsgEnter, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	off += ClOrdIDLen
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
	if err := checkHeader(src, EnterLen, MsgEnter); err != nil {
		return Enter{}, err
	}
	off := 2 + ClOrdIDLen
	m := Enter{
		Version: src[1],
		ClOrdID: getFixed(src[2 : 2+ClOrdIDLen]),
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
	off := header(b, MsgCancel, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	return dst, nil
}

// DecodeCancel reads a Cancel payload from src.
func DecodeCancel(src []byte) (Cancel, error) {
	if err := checkHeader(src, CancelLen, MsgCancel); err != nil {
		return Cancel{}, err
	}
	return Cancel{Version: src[1], ClOrdID: getFixed(src[2 : 2+ClOrdIDLen])}, nil
}

// --- Reduce ---

// EncodeReduce appends a Reduce payload to dst.
func EncodeReduce(dst []byte, m Reduce) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, ReduceLen)...)
	b := dst[base:]
	off := header(b, MsgReduce, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(b[off+ClOrdIDLen:], uint64(m.Quantity))
	return dst, nil
}

// DecodeReduce reads a Reduce payload from src.
func DecodeReduce(src []byte) (Reduce, error) {
	if err := checkHeader(src, ReduceLen, MsgReduce); err != nil {
		return Reduce{}, err
	}
	return Reduce{
		Version:  src[1],
		ClOrdID:  getFixed(src[2 : 2+ClOrdIDLen]),
		Quantity: int64(binary.BigEndian.Uint64(src[2+ClOrdIDLen:])),
	}, nil
}

// --- Accepted ---

// EncodeAccepted appends an Accepted payload to dst.
func EncodeAccepted(dst []byte, m Accepted) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, AcceptedLen)...)
	b := dst[base:]
	off := header(b, MsgAccepted, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	off += ClOrdIDLen
	binary.BigEndian.PutUint64(b[off:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.Quantity))
	b[off+16] = m.Side
	return dst, nil
}

// DecodeAccepted reads an Accepted payload from src.
func DecodeAccepted(src []byte) (Accepted, error) {
	if err := checkHeader(src, AcceptedLen, MsgAccepted); err != nil {
		return Accepted{}, err
	}
	off := 2 + ClOrdIDLen
	return Accepted{
		Version:  src[1],
		ClOrdID:  getFixed(src[2:off]),
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
	off := header(b, MsgExecuted, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	off += ClOrdIDLen
	binary.BigEndian.PutUint64(b[off:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.Quantity))
	binary.BigEndian.PutUint64(b[off+16:], uint64(m.LeavesQty))
	b[off+24] = m.Aggressor
	return dst, nil
}

// DecodeExecuted reads an Executed payload from src.
func DecodeExecuted(src []byte) (Executed, error) {
	if err := checkHeader(src, ExecutedLen, MsgExecuted); err != nil {
		return Executed{}, err
	}
	off := 2 + ClOrdIDLen
	return Executed{
		Version:   src[1],
		ClOrdID:   getFixed(src[2:off]),
		Price:     int64(binary.BigEndian.Uint64(src[off:])),
		Quantity:  int64(binary.BigEndian.Uint64(src[off+8:])),
		LeavesQty: int64(binary.BigEndian.Uint64(src[off+16:])),
		Aggressor: src[off+24],
	}, nil
}

// --- Rejected / Canceled / CmdReject (identical shape, distinct types) ---

func encodeIDReason(dst []byte, width int, msgType, version uint8, clOrdID string, reason uint16) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, width)...)
	b := dst[base:]
	off := header(b, msgType, version)
	if err := putFixed(b[off:off+ClOrdIDLen], clOrdID); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint16(b[off+ClOrdIDLen:], reason)
	return dst, nil
}

func decodeIDReason(src []byte, width int, want uint8) (uint8, string, uint16, error) {
	if err := checkHeader(src, width, want); err != nil {
		return 0, "", 0, err
	}
	return src[1], getFixed(src[2 : 2+ClOrdIDLen]), binary.BigEndian.Uint16(src[2+ClOrdIDLen:]), nil
}

// EncodeRejected appends a Rejected payload to dst.
func EncodeRejected(dst []byte, m Rejected) ([]byte, error) {
	return encodeIDReason(dst, RejectedLen, MsgRejected, m.Version, m.ClOrdID, m.Reason)
}

// DecodeRejected reads a Rejected payload from src.
func DecodeRejected(src []byte) (Rejected, error) {
	v, id, r, err := decodeIDReason(src, RejectedLen, MsgRejected)
	return Rejected{Version: v, ClOrdID: id, Reason: r}, err
}

// EncodeCanceled appends a Canceled payload to dst.
func EncodeCanceled(dst []byte, m Canceled) ([]byte, error) {
	return encodeIDReason(dst, CanceledLen, MsgCanceled, m.Version, m.ClOrdID, m.Reason)
}

// DecodeCanceled reads a Canceled payload from src.
func DecodeCanceled(src []byte) (Canceled, error) {
	v, id, r, err := decodeIDReason(src, CanceledLen, MsgCanceled)
	return Canceled{Version: v, ClOrdID: id, Reason: r}, err
}

// EncodeCmdReject appends a CmdReject payload to dst.
func EncodeCmdReject(dst []byte, m CmdReject) ([]byte, error) {
	return encodeIDReason(dst, CmdRejectLen, MsgCmdReject, m.Version, m.ClOrdID, m.Reason)
}

// DecodeCmdReject reads a CmdReject payload from src.
func DecodeCmdReject(src []byte) (CmdReject, error) {
	v, id, r, err := decodeIDReason(src, CmdRejectLen, MsgCmdReject)
	return CmdReject{Version: v, ClOrdID: id, Reason: r}, err
}

// --- Replaced ---

// EncodeReplaced appends a Replaced payload to dst.
func EncodeReplaced(dst []byte, m Replaced) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, ReplacedLen)...)
	b := dst[base:]
	off := header(b, MsgReplaced, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(b[off+ClOrdIDLen:], uint64(m.LeavesQty))
	return dst, nil
}

// DecodeReplaced reads a Replaced payload from src.
func DecodeReplaced(src []byte) (Replaced, error) {
	if err := checkHeader(src, ReplacedLen, MsgReplaced); err != nil {
		return Replaced{}, err
	}
	return Replaced{
		Version:   src[1],
		ClOrdID:   getFixed(src[2 : 2+ClOrdIDLen]),
		LeavesQty: int64(binary.BigEndian.Uint64(src[2+ClOrdIDLen:])),
	}, nil
}

// --- Query / OpenOrder / QueryEnd ---

// EncodeQuery appends a Query payload to dst.
func EncodeQuery(dst []byte, m Query) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, QueryLen)...)
	header(dst[base:], MsgQuery, m.Version)
	return dst, nil
}

// DecodeQuery reads a Query payload from src.
func DecodeQuery(src []byte) (Query, error) {
	if err := checkHeader(src, QueryLen, MsgQuery); err != nil {
		return Query{}, err
	}
	return Query{Version: src[1]}, nil
}

// EncodeOpenOrder appends an OpenOrder payload to dst.
func EncodeOpenOrder(dst []byte, m OpenOrder) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, OpenOrderLen)...)
	b := dst[base:]
	off := header(b, MsgOpenOrder, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.ClOrdID); err != nil {
		return nil, err
	}
	off += ClOrdIDLen
	binary.BigEndian.PutUint64(b[off:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.LeavesQty))
	b[off+16] = m.Side
	return dst, nil
}

// DecodeOpenOrder reads an OpenOrder payload from src.
func DecodeOpenOrder(src []byte) (OpenOrder, error) {
	if err := checkHeader(src, OpenOrderLen, MsgOpenOrder); err != nil {
		return OpenOrder{}, err
	}
	off := 2 + ClOrdIDLen
	return OpenOrder{
		Version:   src[1],
		ClOrdID:   getFixed(src[2:off]),
		Price:     int64(binary.BigEndian.Uint64(src[off:])),
		LeavesQty: int64(binary.BigEndian.Uint64(src[off+8:])),
		Side:      src[off+16],
	}, nil
}

// EncodeQueryEnd appends a QueryEnd payload to dst.
func EncodeQueryEnd(dst []byte, m QueryEnd) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, QueryEndLen)...)
	b := dst[base:]
	off := header(b, MsgQueryEnd, m.Version)
	binary.BigEndian.PutUint32(b[off:], m.Count)
	binary.BigEndian.PutUint64(b[off+4:], m.Seq)
	return dst, nil
}

// DecodeQueryEnd reads a QueryEnd payload from src.
func DecodeQueryEnd(src []byte) (QueryEnd, error) {
	if err := checkHeader(src, QueryEndLen, MsgQueryEnd); err != nil {
		return QueryEnd{}, err
	}
	return QueryEnd{
		Version: src[1],
		Count:   binary.BigEndian.Uint32(src[2:]),
		Seq:     binary.BigEndian.Uint64(src[6:]),
	}, nil
}

// --- MassCancel / MassCancelAck / CancelOnDisconnect / CODAck ---

// EncodeMassCancel appends a MassCancel payload to dst.
func EncodeMassCancel(dst []byte, m MassCancel) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MassCancelLen)...)
	header(dst[base:], MsgMassCancel, m.Version)
	return dst, nil
}

// DecodeMassCancel reads a MassCancel payload from src.
func DecodeMassCancel(src []byte) (MassCancel, error) {
	if err := checkHeader(src, MassCancelLen, MsgMassCancel); err != nil {
		return MassCancel{}, err
	}
	return MassCancel{Version: src[1]}, nil
}

// EncodeMassCancelAck appends a MassCancelAck payload to dst.
func EncodeMassCancelAck(dst []byte, m MassCancelAck) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MassCancelAckLen)...)
	b := dst[base:]
	off := header(b, MsgMassCancelAck, m.Version)
	binary.BigEndian.PutUint32(b[off:], m.Count)
	binary.BigEndian.PutUint64(b[off+4:], m.Seq)
	return dst, nil
}

// DecodeMassCancelAck reads a MassCancelAck payload from src.
func DecodeMassCancelAck(src []byte) (MassCancelAck, error) {
	if err := checkHeader(src, MassCancelAckLen, MsgMassCancelAck); err != nil {
		return MassCancelAck{}, err
	}
	return MassCancelAck{
		Version: src[1],
		Count:   binary.BigEndian.Uint32(src[2:]),
		Seq:     binary.BigEndian.Uint64(src[6:]),
	}, nil
}

// EncodeCancelOnDisconnect appends a CancelOnDisconnect payload to dst.
func EncodeCancelOnDisconnect(dst []byte, m CancelOnDisconnect) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, CancelOnDisconnectLen)...)
	b := dst[base:]
	off := header(b, MsgCancelOnDisconnect, m.Version)
	b[off] = putBool(m.Enabled)
	return dst, nil
}

// DecodeCancelOnDisconnect reads a CancelOnDisconnect payload from src.
func DecodeCancelOnDisconnect(src []byte) (CancelOnDisconnect, error) {
	if err := checkHeader(src, CancelOnDisconnectLen, MsgCancelOnDisconnect); err != nil {
		return CancelOnDisconnect{}, err
	}
	return CancelOnDisconnect{Version: src[1], Enabled: src[2] != 0}, nil
}

// EncodeCODAck appends a CODAck payload to dst.
func EncodeCODAck(dst []byte, m CODAck) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, CODAckLen)...)
	b := dst[base:]
	off := header(b, MsgCODAck, m.Version)
	b[off] = putBool(m.Enabled)
	return dst, nil
}

// DecodeCODAck reads a CODAck payload from src.
func DecodeCODAck(src []byte) (CODAck, error) {
	if err := checkHeader(src, CODAckLen, MsgCODAck); err != nil {
		return CODAck{}, err
	}
	return CODAck{Version: src[1], Enabled: src[2] != 0}, nil
}
