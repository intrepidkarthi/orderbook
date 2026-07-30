// Package wire is the binary order-entry format spoken by cmd/obgw.
//
// Framing and session semantics are SoupBinTCP 3.00 — a length-prefixed,
// typed-packet session layer with heartbeats and sequenced replay, published by
// Nasdaq and implemented by many venues. Taking it wholesale means the session
// rules are somebody else's well-tested design rather than an invention, and it
// costs no dependencies: the whole thing is a 2-byte length and a 1-byte type.
//
// The payloads inside those packets are this repository's own, because no
// standard order-entry payload matches this engine's order surface. They are
// fixed-width big-endian records so a decoder is bounds-checkable by inspection
// and a golden hex vector pins every field position.
//
// # This is deliberately not importable
//
// It lives under internal/ so "unsupported, may change" is a fact the compiler
// enforces rather than a sentence in a doc comment. pkg/orderentry is the
// supported, semver-covered surface; this package is how those types reach a
// socket, and the format is frozen by testdata vectors rather than by API policy.
//
// # What is deliberately absent from the wire
//
// A client never names an account and never sees an engine order id. Orders are
// referenced only by the client's own ClOrdID, scoped to the authenticated
// session. That is a security boundary, not a simplification: the engine cancels
// by (orderID, userID), and self-trade prevention lets one account observe
// another's resting orders, so a wire that carried either field would let a
// client name an order it does not own. Nor does the wire carry STPMode or the
// privileged flag — the first is venue policy, the second is a liquidation
// capability that must never be client-settable.
package wire

import (
	"errors"
	"fmt"
)

// Protocol version. Bumping it is a breaking change to every committed vector in
// testdata, which is the point: the vectors are the freeze.
//
// v2 added an explicit message-type byte. v1 distinguished messages by payload
// length, which meant any future message that happened to share a length with an
// existing one would have been silently misread as it.
const Version uint8 = 2

// SoupBinTCP packet types. Lowercase letters are client-to-server, uppercase are
// server-to-client, following the published spec.
const (
	PacketDebug         byte = '+'
	PacketLoginAccepted byte = 'A'
	PacketLoginRejected byte = 'J'
	PacketSequencedData byte = 'S'
	PacketServerHeartbt byte = 'H'
	PacketEndOfSession  byte = 'Z'
	PacketLoginRequest  byte = 'L'
	PacketUnsequenced   byte = 'U'
	PacketClientHeartbt byte = 'R'
	PacketLogoutRequest byte = 'O'
)

// Message types. Every payload begins with one of these, then Version. Encoding
// the type explicitly rather than inferring it from length is what lets the
// protocol grow: a new message is a new type byte, not a length nobody may reuse.
const (
	MsgEnter  uint8 = 'E' // inbound: new order
	MsgCancel uint8 = 'C' // inbound: cancel by ClOrdID
	MsgQuery  uint8 = 'Q' // inbound: report my open orders
	MsgReduce uint8 = 'M' // inbound: shrink a resting order, keeping queue position

	MsgAccepted  uint8 = 'A' // outbound: order is live
	MsgRejected  uint8 = 'R' // outbound: order refused
	MsgExecuted  uint8 = 'X' // outbound: a fill
	MsgCanceled  uint8 = 'D' // outbound: order removed
	MsgReplaced  uint8 = 'P' // outbound: size changed in place, queue position kept
	MsgCmdReject uint8 = 'K' // outbound: the command itself was refused
	MsgOpenOrder uint8 = 'O' // outbound: one live order, in reply to a Query
	MsgQueryEnd  uint8 = 'T' // outbound: the Query reply is complete
)

// Field widths. ClOrdIDLen bounds a client identifier; SymbolLen is 16 rather
// than a tighter fit because real venue symbols outgrow short fields and
// widening one later would invalidate every committed vector.
const (
	ClOrdIDLen = 20
	SymbolLen  = 16
)

// Side and order-type encodings.
const (
	SideBuy  uint8 = 'B'
	SideSell uint8 = 'S'

	TypeLimit  uint8 = 'L'
	TypeMarket uint8 = 'M'

	TIFGoodTillCancel  uint8 = 'G'
	TIFImmediateOrCanc uint8 = 'I'
	TIFFillOrKill      uint8 = 'F'
)

// Reason codes. A deliberately narrow vocabulary a client can branch on, not a
// mirror of the engine's error set: coupling the wire to an internal sentinel
// list would make every new sentinel a protocol change.
const (
	ReasonNone           uint16 = 0
	ReasonOther          uint16 = 1
	ReasonUnknownOrder   uint16 = 2
	ReasonDuplicateClOrd uint16 = 3
	ReasonTooSmall       uint16 = 4
	ReasonTooLarge       uint16 = 5
	ReasonPriceBand      uint16 = 6
	ReasonSelfTrade      uint16 = 7
	ReasonPostOnlyCross  uint16 = 8
	ReasonFOKCannotFill  uint16 = 9
	ReasonHalted         uint16 = 10
	ReasonThrottled      uint16 = 11
	ReasonOverloaded     uint16 = 12
	ReasonNotAuthorised  uint16 = 13
	ReasonMalformed      uint16 = 14
	ReasonShuttingDown   uint16 = 15
	// ReasonInvalidQuantity refuses a size the engine will not accept for an
	// order that does exist — a reduce that is not a reduction, or one below what
	// is already filled. Distinct from ReasonMalformed, which says the venue would
	// not look at the message: here it looked, and the client's own model of the
	// order is wrong.
	ReasonInvalidQuantity uint16 = 16
	// ReasonTooSoon refuses a withdrawal of displayed size before the venue's
	// minimum resting time has elapsed. It is the one refusal here that a client
	// should simply retry, which is why it is not folded into Other.
	ReasonTooSoon uint16 = 17
)

var (
	ErrShort     = errors.New("wire: buffer too short")
	ErrBadType   = errors.New("wire: unknown message type")
	ErrTooLong   = errors.New("wire: field exceeds its fixed width")
	ErrBadPacket = errors.New("wire: malformed packet")
)

// --- inbound payloads ---

// Enter is a new order. Version leads every payload so a decoder can refuse a
// message it does not understand before reading any field whose meaning may have
// changed.
type Enter struct {
	Version  uint8
	ClOrdID  string // client's own identifier, unique per session
	Symbol   string
	Side     uint8
	Type     uint8
	TIF      uint8
	PostOnly bool
	Price    int64 // ticks; 0 for market orders
	Quantity int64 // lots
}

// EnterLen is the encoded width of an Enter payload.
const EnterLen = 1 + 1 + ClOrdIDLen + SymbolLen + 1 + 1 + 1 + 1 + 8 + 8

// Cancel references an order by the client's own id. There is deliberately no
// way to name an engine order id or another account.
type Cancel struct {
	Version uint8
	ClOrdID string
}

// CancelLen is the encoded width of a Cancel payload.
const CancelLen = 1 + 1 + ClOrdIDLen

// Reduce shrinks a resting order in place, keeping its queue position. It is the
// one order-entry operation a client provably cannot build for itself:
// cancel-then-new is the obvious substitute and it sends the order to the back of
// its price level, which for a maker managing size is a material loss.
//
// It is a reduction only. A size increase or a price change forfeits priority —
// otherwise a participant could reserve a place in line and grow into it — so
// those remain cancel-then-new, and are refused here rather than silently
// reinterpreted as something the client did not ask for.
//
// Quantity is the new TOTAL, matching how the order was submitted, not a delta to
// subtract. A delta cannot be made safe against a concurrent fill: the venue and
// the client would be subtracting from different numbers, and the resulting size
// would depend on which of the two the venue believed.
type Reduce struct {
	Version  uint8
	ClOrdID  string
	Quantity int64
}

// ReduceLen is the encoded width of a Reduce payload. It is deliberately allowed
// to equal ReplacedLen: identical widths in opposite directions are safe now that
// the type byte, not the length, decides what a payload is.
const ReduceLen = 1 + 1 + ClOrdIDLen + 8

// --- outbound payloads ---

// Accepted reports that an order is live. OrderID is the venue's public handle
// for it, scoped to this session; it is not the engine's internal id.
type Accepted struct {
	Version  uint8
	ClOrdID  string
	Price    int64
	Quantity int64
	Side     uint8
}

// AcceptedLen is the encoded width of an Accepted payload.
const AcceptedLen = 1 + 1 + ClOrdIDLen + 8 + 8 + 1

// Rejected reports that an order was refused, with a code a client can branch on.
type Rejected struct {
	Version uint8
	ClOrdID string
	Reason  uint16
}

// RejectedLen is the encoded width of a Rejected payload.
const RejectedLen = 1 + 1 + ClOrdIDLen + 2

// Executed is a fill. LeavesQty is carried because the event stream is proven to
// reconstruct per-order remaining quantity (see TestEventStreamReconstructsBook);
// without that proof this field would be a guess and had to be omitted.
type Executed struct {
	Version   uint8
	ClOrdID   string
	Price     int64
	Quantity  int64
	LeavesQty int64
	Aggressor uint8 // SideBuy or SideSell: which side took liquidity
}

// ExecutedLen is the encoded width of an Executed payload.
const ExecutedLen = 1 + 1 + ClOrdIDLen + 8 + 8 + 8 + 1

// Canceled reports an order leaving the book, whether the client asked or the
// venue did (self-trade prevention, an OCO twin, an IOC remainder, a kill switch).
type Canceled struct {
	Version uint8
	ClOrdID string
	Reason  uint16
}

// CanceledLen is the encoded width of a Canceled payload.
const CanceledLen = 1 + 1 + ClOrdIDLen + 2

// Replaced reports an in-place size change that kept queue position: either a
// Reduce the client asked for, or self-trade-prevention DECREMENT shrinking a
// maker it did not. A client must handle both — the second arrives unsolicited,
// like Canceled.
type Replaced struct {
	Version   uint8
	ClOrdID   string
	LeavesQty int64
}

// ReplacedLen is the encoded width of a Replaced payload.
const ReplacedLen = 1 + 1 + ClOrdIDLen + 8

// CmdReject refuses the command itself rather than an order — malformed input, a
// rate limit, a saturated matcher. It is distinct from Rejected so a client can
// tell "the venue would not look at this" from "the venue looked and said no".
type CmdReject struct {
	Version uint8
	ClOrdID string
	Reason  uint16
}

// CmdRejectLen is the encoded width of a CmdReject payload.
const CmdRejectLen = 1 + 1 + ClOrdIDLen + 2

// Query asks the venue to report the account's live orders. It carries nothing:
// the account is the session's, and the gateway serves one instrument.
//
// It exists because resume can legitimately fail — an evicted cursor or a new
// venue incarnation — and without this a client refused at login has no
// in-protocol way back to a correct picture. Telling it to "reconcile out of
// band" is telling it to go and build a second integration.
type Query struct {
	Version uint8
}

// QueryLen is the encoded width of a Query payload.
const QueryLen = 1 + 1

// OpenOrder is one live order in reply to a Query. Quantity is what remains, not
// what was submitted: a client reconciling wants the current picture.
type OpenOrder struct {
	Version   uint8
	ClOrdID   string
	Price     int64
	LeavesQty int64
	Side      uint8
}

// OpenOrderLen is the encoded width of an OpenOrder payload.
const OpenOrderLen = 1 + 1 + ClOrdIDLen + 8 + 8 + 1

// QueryEnd terminates a Query reply. Count lets a client verify it received the
// whole report rather than a truncated one, and Seq names the point in its own
// stream the report is consistent with — everything after Seq is a change the
// client must apply on top.
//
// Without the terminator a client cannot distinguish "you have no open orders"
// from "the connection died mid-report", which are opposite conclusions.
type QueryEnd struct {
	Version uint8
	Count   uint32
	Seq     uint64
}

// QueryEndLen is the encoded width of a QueryEnd payload.
const QueryEndLen = 1 + 1 + 4 + 8

// --- encoding helpers ---

// putFixed writes s left-aligned into a fixed-width, NUL-padded field. An
// over-long value is an error rather than a silent truncation, because a
// truncated ClOrdID would collide with another order.
func putFixed(dst []byte, s string) error {
	if len(s) > len(dst) {
		return fmt.Errorf("%w: %q needs %d bytes, field is %d", ErrTooLong, s, len(s), len(dst))
	}
	n := copy(dst, s)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
	return nil
}

// MsgTypeOf reports the message type of a payload, so a dispatcher can switch on
// it without decoding. Length is never the discriminator.
func MsgTypeOf(payload []byte) (uint8, bool) {
	if len(payload) < 2 {
		return 0, false
	}
	return payload[0], true
}

// VersionOf reports the protocol version a payload declares.
func VersionOf(payload []byte) (uint8, bool) {
	if len(payload) < 2 {
		return 0, false
	}
	return payload[1], true
}

// getFixed reads a NUL-padded fixed-width field.
func getFixed(src []byte) string {
	for i, b := range src {
		if b == 0 {
			return string(src[:i])
		}
	}
	return string(src)
}

func putBool(b bool) byte {
	if b {
		return 1
	}
	return 0
}
