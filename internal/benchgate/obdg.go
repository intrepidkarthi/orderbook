package benchgate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// OBDG v1, the portable output digest (docs/BENCH-GATE.md §3).
//
// Two chains over the same replay. core is what two correct matchers with different
// event conventions can agree on: per command whether it was refused, every trade
// from the returned trade list, and the terminal book. full adds each command's
// status and reason and the whole event stream in publish order, trades included.
// full exists because a reject and an accept-then-cancel leave the same book and the
// same trades; core alone could not tell them apart.
//
// Orders are named by tape POSITION, never by an engine id: ids are allocation
// policy (they carry shard bits, a failed fill-or-kill burns trade ids), and another
// implementation will not number anything the way this one does.
const (
	obdgMagic   = "OBDG"
	obdgVersion = 1
	// blockSize is the number of commands per chained block. Per-block hashes are
	// what let a divergence be located instead of only detected.
	blockSize = 4096
)

// Status and reason codes belong to this format. They equal refmatch's enum values
// today and are frozen by the test vector, not by Go's iota: a reordered enum in
// either package must not silently change the format.
const (
	stNA              = 0
	stNew             = 1
	stPartiallyFilled = 2
	stFilled          = 3
	stCancelled       = 4
	stRejected        = 5
	stReduced         = 6
)

// ref names an order: its tape position and a leg (0; 1 is reserved for a
// one-cancels-other second leg, which v1 tapes never carry).
type ref struct {
	pos uint64
	leg uint8
}

// dTrade is one print, as both chains record it.
type dTrade struct {
	price, qty   int64
	maker, taker ref
	aggressor    byte // 'B' or 'S': the taker's side
}

// Event tags in the full chain.
const (
	tagAccepted = 'A'
	tagRejected = 'R'
	tagCanceled = 'D'
	tagReplaced = 'P'
	tagTrade    = 'X'
)

// dEvent is one entry of a command's event stream, already named by position.
type dEvent struct {
	tag    byte
	ref    ref    // A, R, D, P
	reason uint8  // R
	trade  dTrade // X
}

// outcome is one command's result, in the format's vocabulary. Both drivers produce
// it; nothing in it names an engine id.
type outcome struct {
	pos    uint64
	status uint8
	reason uint8
	trades []dTrade // from the returned trade list: the core chain's source
	events []dEvent // the published event stream: the full chain's source
}

// restingOrder is one entry of the terminal book.
type restingOrder struct {
	ref                ref
	sell               bool
	price, qty, filled int64
}

// terminal is the venue's state after the last command.
type terminal struct {
	book      []restingOrder // bids best-first, then asks best-first, oldest first in a level
	lastTrade int64          // 0 if nothing traded
}

// chain hashes one of the two record streams in blocks.
type chain struct {
	prev   [32]byte
	block  bytes.Buffer
	blocks [][32]byte
	keep   *bytes.Buffer // every byte, when a caller asked to see them (the vector)
	trades uint64
}

func (c *chain) write(b []byte) {
	c.block.Write(b)
	if c.keep != nil {
		c.keep.Write(b)
	}
}

func (c *chain) seal() {
	h := sha256.New()
	h.Write(c.prev[:])
	h.Write(c.block.Bytes())
	copy(c.prev[:], h.Sum(nil))
	c.blocks = append(c.blocks, c.prev)
	c.block.Reset()
}

// Digest is a finished replay's fingerprint.
type Digest struct {
	Core, Full [32]byte
	CoreBlocks [][32]byte
	FullBlocks [][32]byte
	Resting    int
	Trades     uint64 // prints in the full chain (and, if the two agree, in core)
	CoreTrades uint64
	// EngineEvents is how many events the engine published over the replay. It is
	// not part of either chain; the sink=count benchmark's guard checks against it.
	EngineEvents     int
	coreRaw, fullRaw []byte
}

// digester accumulates a replay into the two chains.
type digester struct {
	core, full chain
	n          int
	done       bool
}

func newDigester(tapeFile [32]byte, keepBytes bool) *digester {
	d := &digester{}
	header := make([]byte, 0, 37)
	header = append(header, obdgMagic...)
	header = append(header, obdgVersion)
	header = append(header, tapeFile[:]...)
	h0 := sha256.Sum256(header)
	for _, c := range []*chain{&d.core, &d.full} {
		c.prev = h0
		if keepBytes {
			c.keep = &bytes.Buffer{}
			c.keep.Write(header)
		}
	}
	return d
}

func putRef(b []byte, r ref) []byte {
	b = binary.BigEndian.AppendUint64(b, r.pos)
	return append(b, r.leg)
}

func appendTrade(b []byte, ordinal uint64, t dTrade) []byte {
	b = append(b, tagTrade)
	b = binary.BigEndian.AppendUint64(b, ordinal)
	b = binary.BigEndian.AppendUint64(b, uint64(t.price))
	b = binary.BigEndian.AppendUint64(b, uint64(t.qty))
	b = putRef(b, t.maker)
	b = putRef(b, t.taker)
	return append(b, t.aggressor)
}

// command folds one command's outcome into both chains.
func (d *digester) command(o outcome) error {
	if d.done {
		return fmt.Errorf("obdg: command after the terminal record")
	}
	if o.status > stReduced {
		return fmt.Errorf("obdg: command %d has status %d, outside the format", o.pos, o.status)
	}

	// core: refused-or-not, then the returned trades.
	var b []byte
	b = append(b, 'c')
	b = binary.BigEndian.AppendUint64(b, o.pos)
	if o.status == stRejected {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	for _, t := range o.trades {
		d.core.trades++
		b = appendTrade(b, d.core.trades, t)
	}
	d.core.write(b)

	// full: status and reason, then the events in publish order.
	b = b[:0]
	b = append(b, 'C')
	b = binary.BigEndian.AppendUint64(b, o.pos)
	b = append(b, o.status, o.reason)
	for _, e := range o.events {
		switch e.tag {
		case tagAccepted, tagCanceled, tagReplaced:
			b = append(b, e.tag)
			b = putRef(b, e.ref)
		case tagRejected:
			b = append(b, e.tag)
			b = putRef(b, e.ref)
			b = append(b, e.reason)
		case tagTrade:
			d.full.trades++
			b = appendTrade(b, d.full.trades, e.trade)
		default:
			return fmt.Errorf("obdg: command %d: event tag %q is reserved or unknown in v1", o.pos, e.tag)
		}
	}
	d.full.write(b)

	d.n++
	if d.n%blockSize == 0 {
		d.core.seal()
		d.full.seal()
	}
	return nil
}

// finish writes the terminal record and returns the digest.
func (d *digester) finish(t terminal) Digest {
	// Seal the last, partial block. A full last block was sealed as it filled, and
	// an empty tape has no block at all: F then hashes H0 directly.
	if d.n%blockSize != 0 {
		d.core.seal()
		d.full.seal()
	}
	d.done = true
	out := Digest{Resting: len(t.book), Trades: d.full.trades, CoreTrades: d.core.trades}
	for _, c := range []*chain{&d.core, &d.full} {
		var b []byte
		b = append(b, 'E')
		b = binary.BigEndian.AppendUint64(b, uint64(len(t.book)))
		for _, o := range t.book {
			b = append(b, 'L')
			b = putRef(b, o.ref)
			if o.sell {
				b = append(b, 'S')
			} else {
				b = append(b, 'B')
			}
			b = binary.BigEndian.AppendUint64(b, uint64(o.price))
			b = binary.BigEndian.AppendUint64(b, uint64(o.qty))
			b = binary.BigEndian.AppendUint64(b, uint64(o.filled))
		}
		b = append(b, 'O') // v1 tapes never leave the open state
		b = binary.BigEndian.AppendUint64(b, uint64(t.lastTrade))
		b = binary.BigEndian.AppendUint64(b, c.trades)
		if c.keep != nil {
			c.keep.Write(b)
		}
		h := sha256.New()
		h.Write(c.prev[:])
		h.Write(b)
		var f [32]byte
		copy(f[:], h.Sum(nil))
		if c == &d.core {
			out.Core, out.CoreBlocks = f, c.blocks
			if c.keep != nil {
				out.coreRaw = c.keep.Bytes()
			}
		} else {
			out.Full, out.FullBlocks = f, c.blocks
			if c.keep != nil {
				out.fullRaw = c.keep.Bytes()
			}
		}
	}
	return out
}

// --- the .digest file -------------------------------------------------------------

// DigestFile is the committed expectation for one tape (docs/BENCH-GATE.md §3.3).
type DigestFile struct {
	Semantics  int
	TapeSHA256 [32]byte
	TapeName   string
	Resting    int
	Trades     uint64
	CoreBlocks [][32]byte
	FullBlocks [][32]byte
	Core, Full [32]byte
}

// FormatDigestFile renders a digest as an obdigest 1 file.
func FormatDigestFile(semantics int, tapeName string, tapeSHA [32]byte, d Digest) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "obdigest 1\n")
	fmt.Fprintf(&b, "semantics %d\n", semantics)
	fmt.Fprintf(&b, "tape sha256:%x file=%s\n", tapeSHA, tapeName)
	fmt.Fprintf(&b, "terminal resting=%d trades=%d\n", d.Resting, d.Trades)
	for i := range d.FullBlocks {
		fmt.Fprintf(&b, "block %d core=%x full=%x\n", i+1, d.CoreBlocks[i], d.FullBlocks[i])
	}
	fmt.Fprintf(&b, "core %x\n", d.Core)
	fmt.Fprintf(&b, "full %x\n", d.Full)
	return b.Bytes()
}

// ParseDigestFile reads an obdigest 1 file, refusing anything it does not expect.
func ParseDigestFile(src []byte) (*DigestFile, error) {
	lines := strings.Split(strings.TrimSuffix(string(src), "\n"), "\n")
	if len(lines) < 6 || lines[0] != "obdigest 1" {
		return nil, fmt.Errorf("obdigest: not an obdigest 1 file")
	}
	df := &DigestFile{}
	var err error
	if df.Semantics, err = intAfter(lines[1], "semantics "); err != nil {
		return nil, err
	}
	var tapeHex, name string
	if _, err := fmt.Sscanf(lines[2], "tape sha256:%64s file=%s", &tapeHex, &name); err != nil {
		return nil, fmt.Errorf("obdigest: tape line %q: %v", lines[2], err)
	}
	if err := decode32(tapeHex, &df.TapeSHA256); err != nil {
		return nil, err
	}
	df.TapeName = name
	if _, err := fmt.Sscanf(lines[3], "terminal resting=%d trades=%d", &df.Resting, &df.Trades); err != nil {
		return nil, fmt.Errorf("obdigest: terminal line %q: %v", lines[3], err)
	}
	blocks := lines[4 : len(lines)-2]
	for i, l := range blocks {
		var n int
		var ch, fh string
		if _, err := fmt.Sscanf(l, "block %d core=%64s full=%64s", &n, &ch, &fh); err != nil || n != i+1 {
			return nil, fmt.Errorf("obdigest: block line %q", l)
		}
		var c, f [32]byte
		if err := decode32(ch, &c); err != nil {
			return nil, err
		}
		if err := decode32(fh, &f); err != nil {
			return nil, err
		}
		df.CoreBlocks, df.FullBlocks = append(df.CoreBlocks, c), append(df.FullBlocks, f)
	}
	var ch, fh string
	if _, err := fmt.Sscanf(lines[len(lines)-2], "core %64s", &ch); err != nil {
		return nil, fmt.Errorf("obdigest: core line: %v", err)
	}
	if _, err := fmt.Sscanf(lines[len(lines)-1], "full %64s", &fh); err != nil {
		return nil, fmt.Errorf("obdigest: full line: %v", err)
	}
	if err := decode32(ch, &df.Core); err != nil {
		return nil, err
	}
	if err := decode32(fh, &df.Full); err != nil {
		return nil, err
	}
	if !bytes.Equal(FormatDigestFile(df.Semantics, df.TapeName, df.TapeSHA256, Digest{
		Core: df.Core, Full: df.Full, CoreBlocks: df.CoreBlocks, FullBlocks: df.FullBlocks,
		Resting: df.Resting, Trades: df.Trades,
	}), src) {
		return nil, fmt.Errorf("obdigest: file is not in canonical form")
	}
	return df, nil
}

func intAfter(line, prefix string) (int, error) {
	v, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return 0, fmt.Errorf("obdigest: line %q, want %q<n>", line, prefix)
	}
	return strconv.Atoi(v)
}

func decode32(s string, dst *[32]byte) error {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return fmt.Errorf("obdigest: %q is not a 32-byte hex hash", s)
	}
	copy(dst[:], b)
	return nil
}
