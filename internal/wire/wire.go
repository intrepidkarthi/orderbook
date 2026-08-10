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
//
// v4 let a subscriber say which instrument it wants. MDSubscribe named an
// incarnation and a sequence but no symbol, so a market-data connection could only
// ever mean "the one book this venue serves" — which is not a protocol a
// multi-symbol venue can speak (docs/MULTI-SYMBOL.md §4.5). A subscription now
// selects exactly one symbol, and every message on that connection belongs to it,
// so no other market-data payload changed.
//
// v3 gave a trade a name. Executed and MDTrade reported price, quantity and
// aggressor but no identifier, so no message could ever refer back to a specific
// print — which meant trade bust, once the engine had it
// (docs/TRADE-BUST.md), could not be delivered to a client at all: the venue
// would have been annulling a fill it had never named. Both payloads now carry
// TradeID, and Busted / MDBust are the messages that use it.
const Version uint8 = 4

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
//
// They must all be DISTINCT, across both directions. Inbound and outbound are
// separate conversations, so sharing a byte between them looks harmless — and it is
// not: every decoder checks only the type it wants, so two messages with the same
// byte are separated by nothing but their widths, which is the v1 dispatch this
// header replaced. TestMessageTypesAreDistinct enforces it, after 'O' and 'P' were
// each briefly assigned twice.
const (
	MsgEnter              uint8 = 'E' // inbound: new order
	MsgCancel             uint8 = 'C' // inbound: cancel by ClOrdID
	MsgQuery              uint8 = 'Q' // inbound: report my open orders
	MsgReduce             uint8 = 'M' // inbound: shrink a resting order, keeping queue position
	MsgReplaceOrder       uint8 = 'Z' // inbound: cancel one order and enter another, atomically
	MsgEnterDated         uint8 = 'J' // inbound: an order carrying its own expiry (GTD)
	MsgEnterStop          uint8 = 'S' // inbound: stop / stop-limit order
	MsgEnterOCO           uint8 = 'N' // inbound: one-cancels-other pair
	MsgEnterIceberg       uint8 = 'I' // inbound: iceberg (reserve) order
	MsgEnterPegged        uint8 = 'Y' // inbound: order pegged to bid/ask/mid
	MsgEnterTrailing      uint8 = 'W' // inbound: trailing stop
	MsgMassCancel         uint8 = 'F' // inbound: cancel everything I have resting
	MsgCancelOnDisconnect uint8 = 'B' // inbound: pull my book if this session drops

	MsgAccepted      uint8 = 'A' // outbound: order is live
	MsgRejected      uint8 = 'R' // outbound: order refused
	MsgExecuted      uint8 = 'X' // outbound: a fill
	MsgCanceled      uint8 = 'D' // outbound: order removed
	MsgReplaced      uint8 = 'P' // outbound: size changed in place, queue position kept
	MsgCmdReject     uint8 = 'K' // outbound: the command itself was refused
	MsgOpenOrder     uint8 = 'O' // outbound: one live order, in reply to a Query
	MsgQueryEnd      uint8 = 'T' // outbound: the Query reply is complete
	MsgMassCancelAck uint8 = 'G' // outbound: the mass cancel is complete
	MsgCODAck        uint8 = 'V' // outbound: cancel-on-disconnect setting accepted
	MsgBusted        uint8 = 'U' // outbound: one of your fills has been annulled
)

// Market-data message types.
//
// A separate numbering space from order entry, because the two are separate
// conversations on separate connections and nothing decodes both. Sharing one space
// would mean every future order-entry message had to avoid every market-data one for
// no benefit — but a decoder that received the wrong family would still refuse it,
// since every decoder checks for exactly the type it wants.
const (
	MsgMDSubscribe uint8 = 'b' // inbound: start, or resume from a sequence
	MsgMDReject    uint8 = 'r' // outbound: the subscription was refused

	MsgMDLevel       uint8 = 'l' // outbound: one level of a snapshot
	MsgMDSnapshotEnd uint8 = 'e' // outbound: the snapshot is complete
	MsgMDDelta       uint8 = 'd' // outbound: one aggregated level change
	MsgMDTrade       uint8 = 't' // outbound: a print
	MsgMDStatus      uint8 = 's' // outbound: a venue state change
	MsgMDIndicative  uint8 = 'i' // outbound: what an auction would clear at right now
	MsgMDBust        uint8 = 'u' // outbound: an earlier print is annulled
)

// Market-data reject reasons. Narrower than the order-entry vocabulary because a
// subscriber has fewer ways to be wrong.
const (
	MDRejectWrongIncarnation uint8 = 'I' // the cursor belongs to another run of the venue
	MDRejectEvicted          uint8 = 'E' // too far behind; take a fresh snapshot
	MDRejectMalformed        uint8 = 'M'
	MDRejectUnknownSymbol    uint8 = 'S' // this venue does not serve that instrument
)

// Venue states carried by MDStatus.
const (
	MDStateOpen       uint8 = 'O'
	MDStateHalted     uint8 = 'H'
	MDStateCancelOnly uint8 = 'C'
)

// MDSubscribe starts a market-data subscription.
//
// Seq of 0 means "I have nothing, send me a snapshot". A non-zero Seq with a matching
// incarnation is a resume: send me everything after this. Incarnation is checked
// because sequence numbers mean nothing across a restart, and serving different
// content under numbers a subscriber believes it holds is the failure neither side
// can see.
type MDSubscribe struct {
	Version     uint8
	Incarnation string
	Seq         uint64
	// Symbol selects the instrument. One subscription is one symbol, and that is
	// what keeps every other market-data payload unchanged: a connection carries
	// one book, so a delta or a print needs no symbol of its own. A subscriber
	// watching two instruments opens two connections, which is also how it wants to
	// be shaped — it can drop one without disturbing the other.
	//
	// Sequences are per symbol and are not comparable across them
	// (docs/MULTI-SYMBOL.md §4.2). Two connections are two timelines.
	Symbol string
}

// MDSubscribeLen is the encoded width of an MDSubscribe payload.
const MDSubscribeLen = 1 + 1 + sessionLen + 8 + SymbolLen

// MDReject refuses a subscription with one reason byte.
type MDReject struct {
	Version uint8
	Reason  uint8
}

// MDRejectLen is the encoded width of an MDReject payload.
const MDRejectLen = 1 + 1 + 1

// MDLevel is one price level of a snapshot. A snapshot is a run of these followed by
// an MDSnapshotEnd, which is the same shape as the order-entry Query reply — a
// variable-length payload would be the only thing in this protocol that is not
// fixed-width, and the terminator carries the count so a truncated snapshot cannot
// look like a complete one.
type MDLevel struct {
	Version uint8
	Side    uint8
	Price   int64
	Qty     int64
}

// MDLevelLen is the encoded width of an MDLevel payload.
const MDLevelLen = 1 + 1 + 1 + 8 + 8

// MDSnapshotEnd terminates a snapshot. Seq is the point the snapshot is consistent
// with: everything the subscriber receives after this with a greater sequence applies
// on top, and nothing at or below it does.
type MDSnapshotEnd struct {
	Version        uint8
	Count          uint32
	Seq            uint64
	LastTradePrice int64
}

// MDSnapshotEndLen is the encoded width of an MDSnapshotEnd payload.
const MDSnapshotEndLen = 1 + 1 + 4 + 8 + 8

// MDDelta is one aggregated level change. Qty is the level's NEW total; zero means
// the level is gone. Absolute rather than incremental, so a subscriber that drops one
// recovers on the next update for that level instead of being permanently wrong.
type MDDelta struct {
	Version uint8
	Seq     uint64
	Side    uint8
	Price   int64
	Qty     int64
}

// MDDeltaLen is the encoded width of an MDDelta payload.
const MDDeltaLen = 1 + 1 + 8 + 1 + 8 + 8

// MDTrade is a print.
type MDTrade struct {
	Version   uint8
	Seq       uint64
	Price     int64
	Qty       int64
	Aggressor uint8
	// TradeID names this print so an MDBust can refer back to it. Seq would not
	// do: it is the feed's position, which a subscriber that bootstrapped from a
	// later snapshot has never seen, and it is not what the other edge calls the
	// same trade — a drop copy and a market-data feed disagreeing about the name
	// of one print is a reconciliation bug waiting to be written.
	TradeID int64
}

// MDTradeLen is the encoded width of an MDTrade payload.
const MDTradeLen = 1 + 1 + 8 + 8 + 8 + 1 + 8

// MDBust annuls an earlier print, in the same ordered stream as the print it
// annuls. It carries no book change and implies none: a subscriber that rewinds
// its own depth on one of these diverges from the venue. Adjust the tape, not the
// book — docs/TRADE-BUST.md §2.
type MDBust struct {
	Version uint8
	Seq     uint64 // this message's own feed sequence, like every other MD message
	TradeID int64  // the annulled print, as reported in MDTrade.TradeID
}

// MDBustLen is the encoded width of an MDBust payload.
const MDBustLen = 1 + 1 + 8 + 8

// MDStatus is a venue state change, carried in the same ordered stream as the data it
// qualifies rather than delivered on the side.
type MDStatus struct {
	Version uint8
	Seq     uint64
	State   uint8
}

// MDStatusLen is the encoded width of an MDStatus payload.
const MDStatusLen = 1 + 1 + 8 + 1

// MDIndicative is what an auction would clear at if it uncrossed now, with the
// imbalance left over.
//
// Published during pre-open and the closing auction so participants can react before
// the price is fixed. Imbalance is buy minus sell interest at that price, in lots:
// positive means more to buy than to sell. The price alone says where the auction is;
// the imbalance says which way it moves if nobody responds.
type MDIndicative struct {
	Version   uint8
	Seq       uint64
	Price     int64
	Volume    int64
	Imbalance int64
}

// MDIndicativeLen is the encoded width of an MDIndicative payload.
const MDIndicativeLen = 1 + 1 + 8 + 8 + 8 + 8

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
	// TIFDay rests until the venue's session close. It needs no extra field, so it
	// rides the existing Enter — a new legal value for a byte that already exists
	// moves nothing and invalidates no vector.
	TIFDay uint8 = 'D'
	// TIFGoodTillDate needs a deadline, which Enter has nowhere to put. Use
	// EnterDated; sending 'T' on a plain Enter is refused rather than silently
	// treated as GTC.
	TIFGoodTillDate uint8 = 'T'
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

// ReplaceOrder cancels a resting order and enters another in a single command, so
// the client is never briefly holding neither.
//
// Without it a reprice is two messages, Cancel then Enter, and between them the
// client is naked: if the connection dies in the gap it does not know whether it has
// zero orders or one, and another participant can take the price in between. Every
// real venue offers an atomic replace for exactly that reason.
//
// **Priority is forfeited.** The replacement goes to the back of its price level,
// which is correct — an order that could reprice or grow in place would let a
// participant reserve a place in the queue. A same-price size REDUCTION should use
// Reduce, which keeps priority. Replace is for everything else.
//
// **The atomicity is precise, and narrower than it sounds.** No other command can
// interleave: the cancel and the entry happen back to back on the matching
// goroutine. If the original cannot be cancelled — already filled, already gone, not
// yours, or inside the venue's minimum resting time — nothing happens at all and the
// replacement is NOT entered, because a client replacing an order it no longer holds
// did not mean to open a new position. But if the original is cancelled and the
// replacement is then refused by the engine (a price band, a post-only cross), the
// client ends up holding neither, and is told so by a Canceled followed by a
// Rejected. That outcome is reported in the same command rather than left to be
// discovered, which is the part the two-message sequence could not offer.
//
// There is no new outbound message: a successful replace is a Canceled for the old
// ClOrdID followed by an Accepted for the new one, which already describes it
// exactly.
type ReplaceOrder struct {
	Version     uint8
	OrigClOrdID string
	Order       BaseOrder
}

// ReplaceOrderLen is the encoded width of a ReplaceOrder payload.
const ReplaceOrderLen = 1 + 1 + ClOrdIDLen + BaseOrderLen

// EnterDated is an order that carries its own deadline: the base order plus the
// instant it expires.
//
// It is a separate message because Enter has nowhere to put a timestamp, and adding
// one would move every byte after it and invalidate a vector that deployed clients
// already parse. DAY needs no such field — the venue's session close is the deadline —
// so DAY rides the existing Enter as a new TIF value.
//
// ExpiresAt is Unix nanoseconds UTC. A deadline already in the past is refused rather
// than accepted and immediately expired, which would be a confusing accept-then-cancel
// for an order that was never viable.
type EnterDated struct {
	Version   uint8
	Order     BaseOrder
	ExpiresAt int64 // Unix nanoseconds, UTC
}

// EnterDatedLen is the encoded width of an EnterDated payload.
const EnterDatedLen = 1 + 1 + BaseOrderLen + 8

// --- conditional orders ---
//
// The engine has supported stop, stop-limit, OCO, iceberg, pegged and
// trailing-stop orders since v0.5.0, and until now the wire could express two of
// the six: a client could place a limit or a market order and nothing else. Four
// order types were reachable only by an embedder calling the engine in-process,
// which is the same shape of gap as Reduce before v0.12.0 — a real, tested,
// durable capability with no way for a client to ask for it.
//
// Each type gets its OWN message rather than one message with a union of fields.
// A single EnterConditional carrying StopPrice, DisplayQty, PegOffset and a trail
// distance would mean four fields of which three are meaningless on any given
// message, and "a field that exists but is never checked" is exactly what the
// v0.11.0 audit spent its time removing. Five messages cost more code and no
// ambiguity.
//
// All five share the same 56-byte base-order block (BaseOrderLen) as Enter's body,
// so the fields a client already knows how to fill mean the same thing here.
//
// Three of them encode to the same width and are separated by nothing but the type
// byte, which is the fourth time that byte has earned its version bump.

// BaseOrder is the order description shared by Enter and every conditional entry:
// the fields that describe an order regardless of what triggers it.
type BaseOrder struct {
	ClOrdID  string
	Symbol   string
	Side     uint8
	Type     uint8
	TIF      uint8
	PostOnly bool
	Price    int64 // ticks; 0 for market
	Quantity int64 // lots
}

// BaseOrderLen is the encoded width of a BaseOrder block.
const BaseOrderLen = ClOrdIDLen + SymbolLen + 1 + 1 + 1 + 1 + 8 + 8

// EnterStop is a stop or stop-limit order. It rests off the book until the last
// trade price reaches StopPrice, then enters as Type: 'M' for a stop-market, or
// 'L' with Price as the limit for a stop-limit.
//
// StopPrice must be positive; the engine refuses zero rather than treating it as
// "trigger immediately", because an order that fires the instant it arrives is a
// market order and the client should say so.
type EnterStop struct {
	Version   uint8
	Order     BaseOrder
	StopPrice int64
}

// EnterStopLen is the encoded width of an EnterStop payload.
const EnterStopLen = 1 + 1 + BaseOrderLen + 8

// EnterOCO pairs a primary order with a stop leg: fill one and the other is pulled.
// The classic use is a take-profit limit alongside a stop-loss on the same position.
//
// The stop leg carries only its own ClOrdID and prices; it **inherits symbol, side,
// quantity and time-in-force from the primary**. That is deliberate rather than
// lazy: legs with different quantities would leave a residual position behind
// whichever one fired, which is never what an OCO is for, and a protocol that
// cannot express the mistake is better than one that documents it.
//
// StopLimitPrice is the limit the stop leg enters at, or 0 for a stop-market leg.
type EnterOCO struct {
	Version        uint8
	Primary        BaseOrder
	StopClOrdID    string
	StopPrice      int64
	StopLimitPrice int64
}

// EnterOCOLen is the encoded width of an EnterOCO payload.
const EnterOCOLen = 1 + 1 + BaseOrderLen + ClOrdIDLen + 8 + 8

// EnterIceberg shows DisplayQty at a time and refills from the reserve as slices
// fill. Order.Quantity is the TOTAL size, matching the engine.
//
// There is deliberately no jitter field. The engine sets reload-size jitter from
// its own configuration (anti-fingerprinting is venue policy), so a client-supplied
// value would be decoded and overwritten — the precise bug the v0.11.0 audit found
// in Symbol.
type EnterIceberg struct {
	Version    uint8
	Order      BaseOrder
	DisplayQty int64
}

// EnterIcebergLen is the encoded width of an EnterIceberg payload.
const EnterIcebergLen = 1 + 1 + BaseOrderLen + 8

// EnterPegged tracks a reference price at a fixed offset in ticks. Ref is PegBid,
// PegAsk or PegMid. Offset is signed: negative sits inside the reference, positive
// outside.
//
// Order.Price is ignored — the peg computes it — and is refused if non-zero rather
// than silently overwritten, so a client cannot believe it set a price that the
// venue then replaced.
type EnterPegged struct {
	Version uint8
	Order   BaseOrder
	Ref     uint8
	Offset  int64
}

// EnterPeggedLen is the encoded width of an EnterPegged payload.
const EnterPeggedLen = 1 + 1 + BaseOrderLen + 1 + 8

// EnterTrailing is a stop whose trigger follows the market by Trail ticks, ratcheting
// in the favourable direction only. Trail must be positive.
type EnterTrailing struct {
	Version uint8
	Order   BaseOrder
	Trail   int64
}

// EnterTrailingLen is the encoded width of an EnterTrailing payload.
const EnterTrailingLen = 1 + 1 + BaseOrderLen + 8

// Peg references, as single bytes. types.PegReference is a string ("BID"/"ASK"/
// "MID"); the wire uses a byte because a fixed-width record cannot carry a string
// without padding it, and the gateway maps between them.
const (
	PegBid uint8 = 'B'
	PegAsk uint8 = 'A'
	PegMid uint8 = 'M'
)

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
	// TradeID is the engine's identifier for this print — the one thing a later
	// Busted message can use to say which fill it means. Two partial fills of the
	// same order at the same price are otherwise indistinguishable on this wire,
	// so without it a bust would be undeliverable rather than merely awkward.
	//
	// It is an engine TRADE id, not an order id, and carrying it does not breach
	// the "a client never sees an engine order id" rule at the top of this
	// package: a trade id names an event both counterparties took part in, and
	// discloses nothing about anybody's resting orders.
	TradeID int64
}

// ExecutedLen is the encoded width of an Executed payload.
const ExecutedLen = 1 + 1 + ClOrdIDLen + 8 + 8 + 8 + 1 + 8

// Busted tells a client that one of its fills has been annulled: the trade will
// not settle. It does NOT mean the order came back — the book moved on long
// before the bust, and nothing is re-rested. See docs/TRADE-BUST.md §2.
//
// No reason field, deliberately. Bust reasons are operator free text and this is a
// fixed-width wire, so carrying one would mean inventing a code vocabulary nobody
// has asked for yet; Nasdaq's ITCH "Broken Trade" carries the match number and
// nothing else for the same reason. The why reaches the client out of band.
type Busted struct {
	Version uint8
	ClOrdID string // the client's name for the order that filled
	TradeID int64  // the annulled print, as reported in Executed.TradeID
}

// BustedLen is the encoded width of a Busted payload.
const BustedLen = 1 + 1 + ClOrdIDLen + 8

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

// MassCancel pulls every order the account has resting. It carries nothing: the
// account is the session's and the gateway serves one instrument.
//
// This is the control a market maker reaches for when its own state is wrong or it
// needs out of the market immediately, and it is the reason a venue is quotable
// rather than merely testable. Engine.CancelAllForUser has existed since v0.9.0 with
// no way for a client to invoke it.
//
// Each removed order still produces its own Canceled on the account's stream — the
// sweep is not a substitute for those. MassCancelAck follows them and says how many
// there were.
type MassCancel struct {
	Version uint8
}

// MassCancelLen is the encoded width of a MassCancel payload.
const MassCancelLen = 1 + 1

// MassCancelAck terminates a mass cancel. Count is how many orders were removed and
// Seq is the point in the account's stream the sweep is consistent with, so a client
// can tell a completed sweep of zero orders from a connection that died mid-sweep —
// the same reason QueryEnd exists.
type MassCancelAck struct {
	Version uint8
	Count   uint32
	Seq     uint64
}

// MassCancelAckLen is the encoded width of a MassCancelAck payload.
const MassCancelAckLen = 1 + 1 + 4 + 8

// CancelOnDisconnect asks the venue to pull the account's book if this session
// drops. Enabled is idempotent, so a client may set it at any point after login and
// re-assert it freely.
//
// It is deliberately a message rather than a LoginRequest field: adding a field to
// LoginRequest would move every byte after it and invalidate a committed golden
// vector, which is exactly what the type byte exists to avoid.
//
// Scope caveat, stated because it can surprise: orders are not tagged with the
// session that entered them, so the sweep is account-wide. An account holding two
// connections, one with this enabled, loses its whole book when that one drops —
// including orders entered on the other.
type CancelOnDisconnect struct {
	Version uint8
	Enabled bool
}

// CancelOnDisconnectLen is the encoded width of a CancelOnDisconnect payload.
const CancelOnDisconnectLen = 1 + 1 + 1

// CODAck confirms the cancel-on-disconnect setting now in force, so a client is
// never guessing about a control that decides whether its book survives.
type CODAck struct {
	Version uint8
	Enabled bool
}

// CODAckLen is the encoded width of a CODAck payload.
const CODAckLen = 1 + 1 + 1

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
