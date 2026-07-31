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

// --- conditional order entry ---
//
// All five conditional messages share the base-order block, so it is encoded and
// decoded once here rather than five times. A per-message copy is how the five
// layouts would silently drift apart.

// putBase writes a BaseOrder block at b[0:BaseOrderLen].
func putBase(b []byte, o BaseOrder) error {
	if err := putFixed(b[0:ClOrdIDLen], o.ClOrdID); err != nil {
		return err
	}
	off := ClOrdIDLen
	if err := putFixed(b[off:off+SymbolLen], o.Symbol); err != nil {
		return err
	}
	off += SymbolLen
	b[off] = o.Side
	b[off+1] = o.Type
	b[off+2] = o.TIF
	b[off+3] = putBool(o.PostOnly)
	off += 4
	binary.BigEndian.PutUint64(b[off:], uint64(o.Price))
	binary.BigEndian.PutUint64(b[off+8:], uint64(o.Quantity))
	return nil
}

// getBase reads a BaseOrder block from src[0:BaseOrderLen].
func getBase(src []byte) BaseOrder {
	off := ClOrdIDLen
	o := BaseOrder{
		ClOrdID: getFixed(src[0:ClOrdIDLen]),
		Symbol:  getFixed(src[off : off+SymbolLen]),
	}
	off += SymbolLen
	o.Side = src[off]
	o.Type = src[off+1]
	o.TIF = src[off+2]
	o.PostOnly = src[off+3] != 0
	off += 4
	o.Price = int64(binary.BigEndian.Uint64(src[off:]))
	o.Quantity = int64(binary.BigEndian.Uint64(src[off+8:]))
	return o
}

// encodeConditional writes the common shape: header, base order, then width-8
// trailing int64 fields in order.
func encodeConditional(dst []byte, width int, msgType, version uint8, o BaseOrder, tail ...int64) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, width)...)
	b := dst[base:]
	off := header(b, msgType, version)
	if err := putBase(b[off:], o); err != nil {
		return nil, err
	}
	off += BaseOrderLen
	for _, v := range tail {
		binary.BigEndian.PutUint64(b[off:], uint64(v))
		off += 8
	}
	return dst, nil
}

// EncodeEnterStop appends an EnterStop payload to dst.
func EncodeEnterStop(dst []byte, m EnterStop) ([]byte, error) {
	return encodeConditional(dst, EnterStopLen, MsgEnterStop, m.Version, m.Order, m.StopPrice)
}

// DecodeEnterStop reads an EnterStop payload from src.
func DecodeEnterStop(src []byte) (EnterStop, error) {
	if err := checkHeader(src, EnterStopLen, MsgEnterStop); err != nil {
		return EnterStop{}, err
	}
	off := 2 + BaseOrderLen
	return EnterStop{
		Version:   src[1],
		Order:     getBase(src[2:]),
		StopPrice: int64(binary.BigEndian.Uint64(src[off:])),
	}, nil
}

// EncodeEnterIceberg appends an EnterIceberg payload to dst.
func EncodeEnterIceberg(dst []byte, m EnterIceberg) ([]byte, error) {
	return encodeConditional(dst, EnterIcebergLen, MsgEnterIceberg, m.Version, m.Order, m.DisplayQty)
}

// DecodeEnterIceberg reads an EnterIceberg payload from src.
func DecodeEnterIceberg(src []byte) (EnterIceberg, error) {
	if err := checkHeader(src, EnterIcebergLen, MsgEnterIceberg); err != nil {
		return EnterIceberg{}, err
	}
	off := 2 + BaseOrderLen
	return EnterIceberg{
		Version:    src[1],
		Order:      getBase(src[2:]),
		DisplayQty: int64(binary.BigEndian.Uint64(src[off:])),
	}, nil
}

// EncodeEnterTrailing appends an EnterTrailing payload to dst.
func EncodeEnterTrailing(dst []byte, m EnterTrailing) ([]byte, error) {
	return encodeConditional(dst, EnterTrailingLen, MsgEnterTrailing, m.Version, m.Order, m.Trail)
}

// DecodeEnterTrailing reads an EnterTrailing payload from src.
func DecodeEnterTrailing(src []byte) (EnterTrailing, error) {
	if err := checkHeader(src, EnterTrailingLen, MsgEnterTrailing); err != nil {
		return EnterTrailing{}, err
	}
	off := 2 + BaseOrderLen
	return EnterTrailing{
		Version: src[1],
		Order:   getBase(src[2:]),
		Trail:   int64(binary.BigEndian.Uint64(src[off:])),
	}, nil
}

// EncodeEnterPegged appends an EnterPegged payload to dst.
func EncodeEnterPegged(dst []byte, m EnterPegged) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, EnterPeggedLen)...)
	b := dst[base:]
	off := header(b, MsgEnterPegged, m.Version)
	if err := putBase(b[off:], m.Order); err != nil {
		return nil, err
	}
	off += BaseOrderLen
	b[off] = m.Ref
	binary.BigEndian.PutUint64(b[off+1:], uint64(m.Offset))
	return dst, nil
}

// DecodeEnterPegged reads an EnterPegged payload from src.
func DecodeEnterPegged(src []byte) (EnterPegged, error) {
	if err := checkHeader(src, EnterPeggedLen, MsgEnterPegged); err != nil {
		return EnterPegged{}, err
	}
	off := 2 + BaseOrderLen
	return EnterPegged{
		Version: src[1],
		Order:   getBase(src[2:]),
		Ref:     src[off],
		Offset:  int64(binary.BigEndian.Uint64(src[off+1:])),
	}, nil
}

// EncodeEnterOCO appends an EnterOCO payload to dst.
func EncodeEnterOCO(dst []byte, m EnterOCO) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, EnterOCOLen)...)
	b := dst[base:]
	off := header(b, MsgEnterOCO, m.Version)
	if err := putBase(b[off:], m.Primary); err != nil {
		return nil, err
	}
	off += BaseOrderLen
	if err := putFixed(b[off:off+ClOrdIDLen], m.StopClOrdID); err != nil {
		return nil, err
	}
	off += ClOrdIDLen
	binary.BigEndian.PutUint64(b[off:], uint64(m.StopPrice))
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.StopLimitPrice))
	return dst, nil
}

// DecodeEnterOCO reads an EnterOCO payload from src.
func DecodeEnterOCO(src []byte) (EnterOCO, error) {
	if err := checkHeader(src, EnterOCOLen, MsgEnterOCO); err != nil {
		return EnterOCO{}, err
	}
	off := 2 + BaseOrderLen
	return EnterOCO{
		Version:        src[1],
		Primary:        getBase(src[2:]),
		StopClOrdID:    getFixed(src[off : off+ClOrdIDLen]),
		StopPrice:      int64(binary.BigEndian.Uint64(src[off+ClOrdIDLen:])),
		StopLimitPrice: int64(binary.BigEndian.Uint64(src[off+ClOrdIDLen+8:])),
	}, nil
}

// --- ReplaceOrder ---

// EncodeReplaceOrder appends a ReplaceOrder payload to dst.
func EncodeReplaceOrder(dst []byte, m ReplaceOrder) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, ReplaceOrderLen)...)
	b := dst[base:]
	off := header(b, MsgReplaceOrder, m.Version)
	if err := putFixed(b[off:off+ClOrdIDLen], m.OrigClOrdID); err != nil {
		return nil, err
	}
	off += ClOrdIDLen
	if err := putBase(b[off:], m.Order); err != nil {
		return nil, err
	}
	return dst, nil
}

// DecodeReplaceOrder reads a ReplaceOrder payload from src.
func DecodeReplaceOrder(src []byte) (ReplaceOrder, error) {
	if err := checkHeader(src, ReplaceOrderLen, MsgReplaceOrder); err != nil {
		return ReplaceOrder{}, err
	}
	off := 2 + ClOrdIDLen
	return ReplaceOrder{
		Version:     src[1],
		OrigClOrdID: getFixed(src[2:off]),
		Order:       getBase(src[off:]),
	}, nil
}

// --- market data ---
//
// A snapshot is a run of MDLevel followed by MDSnapshotEnd, rather than one
// variable-length message. That keeps every payload in this protocol fixed-width and
// bounds-checkable by inspection, and it is the same shape as the order-entry Query
// reply — including the terminator carrying a count, so a truncated snapshot cannot
// be mistaken for a complete one.

// EncodeMDSubscribe appends an MDSubscribe payload to dst.
func EncodeMDSubscribe(dst []byte, m MDSubscribe) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MDSubscribeLen)...)
	b := dst[base:]
	off := header(b, MsgMDSubscribe, m.Version)
	if err := putFixed(b[off:off+sessionLen], m.Incarnation); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(b[off+sessionLen:], m.Seq)
	return dst, nil
}

// DecodeMDSubscribe reads an MDSubscribe payload from src.
func DecodeMDSubscribe(src []byte) (MDSubscribe, error) {
	if err := checkHeader(src, MDSubscribeLen, MsgMDSubscribe); err != nil {
		return MDSubscribe{}, err
	}
	return MDSubscribe{
		Version:     src[1],
		Incarnation: getFixed(src[2 : 2+sessionLen]),
		Seq:         binary.BigEndian.Uint64(src[2+sessionLen:]),
	}, nil
}

// EncodeMDReject appends an MDReject payload to dst.
func EncodeMDReject(dst []byte, m MDReject) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MDRejectLen)...)
	b := dst[base:]
	off := header(b, MsgMDReject, m.Version)
	b[off] = m.Reason
	return dst, nil
}

// DecodeMDReject reads an MDReject payload from src.
func DecodeMDReject(src []byte) (MDReject, error) {
	if err := checkHeader(src, MDRejectLen, MsgMDReject); err != nil {
		return MDReject{}, err
	}
	return MDReject{Version: src[1], Reason: src[2]}, nil
}

// EncodeMDLevel appends an MDLevel payload to dst.
func EncodeMDLevel(dst []byte, m MDLevel) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MDLevelLen)...)
	b := dst[base:]
	off := header(b, MsgMDLevel, m.Version)
	b[off] = m.Side
	binary.BigEndian.PutUint64(b[off+1:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+9:], uint64(m.Qty))
	return dst, nil
}

// DecodeMDLevel reads an MDLevel payload from src.
func DecodeMDLevel(src []byte) (MDLevel, error) {
	if err := checkHeader(src, MDLevelLen, MsgMDLevel); err != nil {
		return MDLevel{}, err
	}
	return MDLevel{
		Version: src[1], Side: src[2],
		Price: int64(binary.BigEndian.Uint64(src[3:])),
		Qty:   int64(binary.BigEndian.Uint64(src[11:])),
	}, nil
}

// EncodeMDSnapshotEnd appends an MDSnapshotEnd payload to dst.
func EncodeMDSnapshotEnd(dst []byte, m MDSnapshotEnd) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MDSnapshotEndLen)...)
	b := dst[base:]
	off := header(b, MsgMDSnapshotEnd, m.Version)
	binary.BigEndian.PutUint32(b[off:], m.Count)
	binary.BigEndian.PutUint64(b[off+4:], m.Seq)
	binary.BigEndian.PutUint64(b[off+12:], uint64(m.LastTradePrice))
	return dst, nil
}

// DecodeMDSnapshotEnd reads an MDSnapshotEnd payload from src.
func DecodeMDSnapshotEnd(src []byte) (MDSnapshotEnd, error) {
	if err := checkHeader(src, MDSnapshotEndLen, MsgMDSnapshotEnd); err != nil {
		return MDSnapshotEnd{}, err
	}
	return MDSnapshotEnd{
		Version:        src[1],
		Count:          binary.BigEndian.Uint32(src[2:]),
		Seq:            binary.BigEndian.Uint64(src[6:]),
		LastTradePrice: int64(binary.BigEndian.Uint64(src[14:])),
	}, nil
}

// EncodeMDDelta appends an MDDelta payload to dst.
func EncodeMDDelta(dst []byte, m MDDelta) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MDDeltaLen)...)
	b := dst[base:]
	off := header(b, MsgMDDelta, m.Version)
	binary.BigEndian.PutUint64(b[off:], m.Seq)
	b[off+8] = m.Side
	binary.BigEndian.PutUint64(b[off+9:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+17:], uint64(m.Qty))
	return dst, nil
}

// DecodeMDDelta reads an MDDelta payload from src.
func DecodeMDDelta(src []byte) (MDDelta, error) {
	if err := checkHeader(src, MDDeltaLen, MsgMDDelta); err != nil {
		return MDDelta{}, err
	}
	return MDDelta{
		Version: src[1],
		Seq:     binary.BigEndian.Uint64(src[2:]),
		Side:    src[10],
		Price:   int64(binary.BigEndian.Uint64(src[11:])),
		Qty:     int64(binary.BigEndian.Uint64(src[19:])),
	}, nil
}

// EncodeMDTrade appends an MDTrade payload to dst.
func EncodeMDTrade(dst []byte, m MDTrade) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MDTradeLen)...)
	b := dst[base:]
	off := header(b, MsgMDTrade, m.Version)
	binary.BigEndian.PutUint64(b[off:], m.Seq)
	binary.BigEndian.PutUint64(b[off+8:], uint64(m.Price))
	binary.BigEndian.PutUint64(b[off+16:], uint64(m.Qty))
	b[off+24] = m.Aggressor
	return dst, nil
}

// DecodeMDTrade reads an MDTrade payload from src.
func DecodeMDTrade(src []byte) (MDTrade, error) {
	if err := checkHeader(src, MDTradeLen, MsgMDTrade); err != nil {
		return MDTrade{}, err
	}
	return MDTrade{
		Version:   src[1],
		Seq:       binary.BigEndian.Uint64(src[2:]),
		Price:     int64(binary.BigEndian.Uint64(src[10:])),
		Qty:       int64(binary.BigEndian.Uint64(src[18:])),
		Aggressor: src[26],
	}, nil
}

// EncodeMDStatus appends an MDStatus payload to dst.
func EncodeMDStatus(dst []byte, m MDStatus) ([]byte, error) {
	base := len(dst)
	dst = append(dst, make([]byte, MDStatusLen)...)
	b := dst[base:]
	off := header(b, MsgMDStatus, m.Version)
	binary.BigEndian.PutUint64(b[off:], m.Seq)
	b[off+8] = m.State
	return dst, nil
}

// DecodeMDStatus reads an MDStatus payload from src.
func DecodeMDStatus(src []byte) (MDStatus, error) {
	if err := checkHeader(src, MDStatusLen, MsgMDStatus); err != nil {
		return MDStatus{}, err
	}
	return MDStatus{
		Version: src[1],
		Seq:     binary.BigEndian.Uint64(src[2:]),
		State:   src[10],
	}, nil
}
