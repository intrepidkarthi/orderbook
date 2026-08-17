// Package tape generates deterministic pseudo-random command streams, and shrinks
// the ones that find a bug down to something a person will read.
//
// There is one generator in this repository and this is it. Two generators means
// two alphabets, means one of them is extended and the other is not, means the
// sweep that claims to be the stronger test is exercising less than the one it is
// supposed to dominate. docs/JOURNAL-COMPLETENESS.md §1 is what that costs.
//
// Like package refmatch, this imports nothing outside the standard library. A tape
// that spoke the production vocabulary would drag pkg/types into the model through
// the back door.
package tape

import (
	"fmt"
	"strings"
)

// Kind is a command kind the tape can draw.
type Kind uint8

const (
	Submit Kind = iota
	Cancel
	Reduce
	Replace
	CancelAll
	Halt
	Resume
	CancelOnly
	SetPhase
	// KindCount is a sentinel: keep it last. It is what lets a guard ENUMERATE the
	// kinds rather than restate them.
	KindCount
)

func (k Kind) String() string {
	switch k {
	case Submit:
		return "Submit"
	case Cancel:
		return "Cancel"
	case Reduce:
		return "Reduce"
	case Replace:
		return "Replace"
	case CancelAll:
		return "CancelAll"
	case Halt:
		return "Halt"
	case Resume:
		return "Resume"
	case CancelOnly:
		return "CancelOnly"
	case SetPhase:
		return "SetPhase"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// Phase names a trading phase a SetPhase command can move the venue to. It is the
// tape's own small enum rather than matching.EngineState for the same reason every
// other value here is: a tape that imported the engine's vocabulary would not be
// usable by a model that must not.
type Phase uint8

const (
	PhaseOpen Phase = iota
	PhasePreOpen
	PhaseClosingAuction
	PhaseHalted
	PhaseCancelOnly
	PhaseClosed
)

// Cmd is one command. Every field is a scalar or a string, so a Cmd is comparable
// and a tape can be compared with == .
//
// Pos is the command's ORIGINAL index in the generated tape, and Target names an
// earlier command by its Pos. That is what makes a tape deletion-closed: delete the
// submit at position 12 and every later command that named 12 now names a position
// that produced no order, which both sides model as a plain "not found". If a
// target were an index into the ids accumulated so far — which is how the tape this
// supersedes did it — deleting an earlier submit would silently RETARGET every
// later cancel, so a shrinker would lose the failure and report that the bug went
// away.
type Cmd struct {
	Pos  int
	Kind Kind

	// Order payload, used by Submit and by Replace's replacement.
	User       string
	Sell       bool
	MarketOrd  bool
	Price      int64
	Qty        int64
	TIF        uint8 // 0 GTC, 1 IOC, 2 FOK
	PostOnly   bool
	STP        uint8 // 0 unset, then the five modes in declaration order
	TradeGroup int64
	Privileged bool

	// Target names an earlier command by its Pos, for Cancel, Reduce and Replace.
	Target int
	// NewQty is Reduce's new total quantity.
	NewQty int64
	// Phase is SetPhase's destination.
	Phase Phase
}

// lcg is a tiny deterministic generator. math/rand's stream is not guaranteed
// stable across Go releases, and a harness whose input silently changes under a
// toolchain upgrade is a harness whose green run means nothing.
type lcg uint64

func (r *lcg) next() uint64 {
	*r = lcg(uint64(*r)*6364136223846793005 + 1442695040888963407)
	return uint64(*r) >> 11
}

func (r *lcg) intn(n int64) int64 { return int64(r.next() % uint64(n)) }

// Vector returns the first n values of the stream seeded with seed. It exists so a
// golden test can pin the PRIMITIVE rather than a generated tape: pinning tapes
// would make every intentional alphabet extension a golden-file churn, which trains
// people to regenerate goldens without reading them.
func Vector(seed uint64, n int) []uint64 {
	r := lcg(seed)
	out := make([]uint64, n)
	for i := range out {
		out[i] = r.next()
	}
	return out
}

// Profile decides which kinds a tape may draw and with what weights.
type Profile struct {
	Name string
	// Weights is the relative draw weight of each kind. A zero weight for a kind the
	// model claims to implement is the config-free form of an incomplete alphabet,
	// and it reports as coverage — so there is a guard on it.
	Weights [KindCount]int
	// Users is the size of the account pool. Small, so self-matches actually happen.
	Users int
	// TradeGroups is the size of the trade-group pool (0 means none), so
	// self-trade prevention is reached across accounts too.
	TradeGroups int64
	// PriceLo and PriceSpan bound the price range. Narrow, so orders cross.
	PriceLo, PriceSpan int64
	// QtyMax bounds order size.
	QtyMax int64
	// Exotic draws market orders, IOC/FOK, post-only, per-order STP modes, trade
	// groups and privileged orders.
	Exotic bool
	// ExoticDamp divides the exotic draw RATE (0 and 1 both mean undamped). It is a
	// density knob, never an on/off one: at every value each exotic path is still
	// reachable, and the outcome guards assert each is actually reached.
	//
	// It exists because breadth and depth pull against each other in a REPLAY sweep
	// and not in a differential one. Market orders and IOC/FOK never rest, and the
	// three cancelling STP modes remove liquidity that is already resting, so drawn
	// at the differential rate they cost the 400-command boundary sweep more than
	// half its continuous prints (117 -> 52 measured) and nearly half its peak
	// resting depth (45 -> 25). A boundary sweep over a near-empty book compares a
	// near-empty book. The differential harness has the opposite need — it compares
	// semantics per command and does not care what is left resting — so it runs
	// undamped.
	//
	// Damping is a rate rather than a switch precisely because a switch is what
	// produced the incomplete alphabet this field was introduced to fix. Measured on
	// the 400-command sweep at damp 4: 110 prints, 17 auction prints, peak depth 32,
	// and market, post-only, IOC, FOK, per-order STP, trade groups and privileged
	// orders all still drawn.
	//
	// The draw COUNT is identical at every value, so a profile at damp 0 or 1 draws
	// exactly the stream it drew before this field existed — which is what keeps the
	// differential sweep's recorded mutation evidence valid. TestDifferentialTapeIsPinned
	// enforces that.
	ExoticDamp int64
	// Phases pins phase transitions to fixed command indices, so every consumer of
	// a profile runs the same session however long its tape is.
	Phases map[int]Phase
}

// Differential is the tier-1 alphabet: everything that changes what the matching
// walk does, and nothing that sits on top of a walk it does not change.
var Differential = Profile{
	Name:        "differential",
	Weights:     weights(map[Kind]int{Submit: 60, Cancel: 12, Reduce: 8, Replace: 6, CancelAll: 3, Halt: 2, Resume: 3, CancelOnly: 2}),
	Users:       4,
	TradeGroups: 2,
	PriceLo:     98,
	PriceSpan:   7,
	QtyMax:      9,
	Exotic:      true,
}

// Recovery is the alphabet the crash-boundary sweeps drive. It is deliberately a
// SUPERSET of Differential: the replay oracle can check kinds the model does not
// model, and refusing to sweep them because the model is behind would be exactly
// the trade docs/REFERENCE-MATCHER.md exists to argue against.
//
// "Superset" is a claim on TWO axes and it was true on only one of them for exactly
// one round. This profile shipped with Exotic:false, which made it a superset on the
// Kind axis (it adds SetPhase) and a strict SUBSET on the order-payload axis: 287
// submits on the 400-command sweep, every one of them a plain GTC limit, zero market
// orders, zero IOC, zero FOK, zero post-only, zero per-order STP, zero trade groups
// and zero privileged orders. The replay oracle therefore never crossed a crash
// boundary carrying a rejected FOK's reversed prints or an STP-cancelled maker —
// the two paths docs/REFERENCE-MATCHER.md §9 named in advance as defect-bearing and
// the two the differential harness then confirmed as live defects. A document whose
// thesis is "an exhaustive check over an incomplete alphabet reports completeness"
// was reporting completeness over an incomplete alphabet. Exotic is on now, and
// TestRecoveryTapeSpeaksTheTierOneAlphabet asserts the payload axis by OUTCOME so
// the claim cannot quietly become false again.
//
// The phase transitions are a session in the order a venue runs one: the tape opens
// in pre-open so the first orders accumulate into a legitimately crossed book, the
// transition at 21 uncrosses it, the closing auction accumulates a second crossed
// book on top of a live one, and the transition out of it prints the closing
// uncross before continuous trading resumes. Fixed indices, all below 200, so the
// 2000-command recovery tape and the 400- and 200-command sweeps run the same
// session.
var Recovery = Profile{
	Name:        "recovery",
	Weights:     weights(map[Kind]int{Submit: 70, Cancel: 10, Reduce: 7, Replace: 6, CancelAll: 2, Halt: 1, Resume: 3, CancelOnly: 1}),
	Users:       8,
	TradeGroups: 2,
	PriceLo:     95,
	PriceSpan:   11,
	QtyMax:      9,
	Exotic:      true,
	// See ExoticDamp. Only market/IOC/FOK are damped, and only so the boundary
	// sweep keeps a book worth comparing; every exotic path is still drawn.
	ExoticDamp: 4,
	Phases: map[int]Phase{
		0:   PhasePreOpen,
		21:  PhaseOpen,
		120: PhaseClosingAuction,
		141: PhaseOpen,
	},
}

func weights(w map[Kind]int) [KindCount]int {
	var out [KindCount]int
	for k, v := range w {
		out[k] = v
	}
	return out
}

// ProRata is the same alphabet as Differential; the difference is the engine
// configuration the harness pairs it with, not the tape.
var ProRata = Profile{
	Name:        "prorata",
	Weights:     Differential.Weights,
	Users:       Differential.Users,
	TradeGroups: Differential.TradeGroups,
	PriceLo:     Differential.PriceLo,
	PriceSpan:   Differential.PriceSpan,
	QtyMax:      Differential.QtyMax,
	Exotic:      Differential.Exotic,
}

// Gen is a pure function of (profile, seed, n).
func Gen(p Profile, seed uint64, n int) []Cmd {
	r := lcg(seed)
	total := 0
	for _, w := range p.Weights {
		total += w
	}
	accumulate := accumulating(p, n)
	out := make([]Cmd, 0, n)
	for i := 0; i < n; i++ {
		if ph, ok := p.Phases[i]; ok {
			out = append(out, Cmd{Pos: i, Kind: SetPhase, Phase: ph})
			continue
		}
		c := Cmd{Pos: i, Kind: pick(&r, p.Weights, total)}
		// The first commands are always submits: a tape whose opening move is a
		// cancel of nothing wastes the only part of the sweep a reader ever reads.
		// So is every command inside an accumulating auction phase, and that one is
		// load-bearing rather than cosmetic — see accumulating().
		if i < 4 || accumulate[i] {
			c.Kind = Submit
		}
		switch c.Kind {
		case Submit:
			fillOrder(&r, p, &c)
		case Replace:
			fillOrder(&r, p, &c)
			c.Target = aimRecent(&r, i)
			c.User = aimAtOwner(out, c.Target, &r, c.User)
		case Cancel:
			c.Target = aimRecent(&r, i)
			c.User = aimAtOwner(out, c.Target, &r, user(&r, p))
		case Reduce:
			c.Target = aimRecent(&r, i)
			c.User = aimAtOwner(out, c.Target, &r, user(&r, p))
			c.NewQty = aimBelow(out, c.Target, &r, p)
		case CancelAll:
			c.User = user(&r, p)
		}
		out = append(out, c)
	}
	return out
}

// accumulating marks every index that falls inside a pre-open or closing-auction
// phase, where the generator draws submits and nothing else.
//
// This is not cosmetic. An accumulating phase takes orders without matching them, so
// the crossed book it builds is what the uncross on the way out has to resolve — and
// a halt, a cancel-only or a cancel-all landing inside the phase refuses or removes
// exactly the orders the auction was going to clear. When the recovery alphabet was
// first widened without this, the 400-command sweep went from 11 auction prints to
// ZERO while every one of its assertions still passed, because the assertion that
// would have caught it (auctionPrints() != 0) was itself satisfied by a single stray
// print until it was not. A wider command alphabet that quietly stops running the
// auction is the same failure docs/JOURNAL-COMPLETENESS.md §1 describes, arriving by
// the opposite route.
func accumulating(p Profile, n int) map[int]bool {
	if len(p.Phases) == 0 {
		return nil
	}
	starts := make([]int, 0, len(p.Phases))
	for i := range p.Phases {
		starts = append(starts, i)
	}
	sortInts(starts)
	out := map[int]bool{}
	for k, i := range starts {
		if p.Phases[i] != PhasePreOpen && p.Phases[i] != PhaseClosingAuction {
			continue
		}
		end := n
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		for j := i + 1; j < end; j++ {
			out[j] = true
		}
	}
	return out
}

// sortInts is an insertion sort. There are four phase indices; anything cleverer
// would be a dependency this package refuses to take for no reason.
func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}

func pick(r *lcg, w [KindCount]int, total int) Kind {
	if total == 0 {
		return Submit
	}
	v := int(r.intn(int64(total)))
	for k := Kind(0); k < KindCount; k++ {
		v -= w[k]
		if v < 0 {
			return k
		}
	}
	return Submit
}

func user(r *lcg, p Profile) string { return fmt.Sprintf("u%d", r.intn(int64(p.Users))) }

// aimAtOwner points a cancel, reduce or replace at the account that submitted the
// order it names — three times in four.
//
// Without it the account is drawn independently and almost every such command comes
// back "not found", because a venue answers "not yours" and "does not exist"
// identically so a probe cannot tell them apart. The first version of this generator
// did exactly that, and the sweep it produced reached a SUCCESSFUL reduce zero times
// in sixteen tapes while reporting healthy coverage of the reduce command. That is
// the same failure shape docs/JOURNAL-COMPLETENESS.md §1 describes, one level down:
// the kind was drawn, so it looked exercised.
//
// The remaining one in four keeps the mismatched-owner path swept, and it stays a
// pure function of the seed because the generator knows what account it drew for
// every earlier position.
func aimAtOwner(out []Cmd, target int, r *lcg, fallback string) string {
	if r.intn(4) == 0 || target < 0 || target >= len(out) {
		return fallback
	}
	if u := out[target].User; u != "" && (out[target].Kind == Submit || out[target].Kind == Replace) {
		return u
	}
	return fallback
}

// aimRecent picks the position a cancel, reduce or replace names, from a window of
// recent commands three times in four and from the whole tape otherwise.
//
// Uniform over all earlier positions sounds fairer and is much weaker: most of a
// tape's orders have already filled or been pulled, so a uniform target names a dead
// order almost every time and the command under test never gets past its first
// check. The remaining quarter keeps the long-dead and never-existed targets swept,
// which is where the deletion-closure property gets its exercise.
func aimRecent(r *lcg, i int) int {
	const window = 24
	if r.intn(4) == 0 || i <= window {
		return int(r.intn(int64(i)))
	}
	return i - 1 - int(r.intn(window))
}

// aimBelow draws a reduce's new total below the target's original size — three times
// in four — so the reduction is a legal one. A reduce is a REDUCTION: a new total at
// or above the original, or at or below what has already filled, is refused, and a
// generator that only ever drew those would sweep the refusal and never the verb.
func aimBelow(out []Cmd, target int, r *lcg, p Profile) int64 {
	if r.intn(4) != 0 && target >= 0 && target < len(out) {
		if q := out[target].Qty; q > 1 {
			return 1 + r.intn(q-1)
		}
	}
	return 1 + r.intn(p.QtyMax)
}

func fillOrder(r *lcg, p Profile, c *Cmd) {
	c.User = user(r, p)
	c.Sell = r.intn(2) == 1
	c.Price = p.PriceLo + r.intn(p.PriceSpan)
	c.Qty = 1 + r.intn(p.QtyMax)
	if !p.Exotic {
		return
	}
	// Every draw below is taken at the same POSITION in the stream whatever the damp
	// is, so a profile at damp 0/1 generates exactly the tape it generated before
	// ExoticDamp existed. Only the two depth-costing denominators move.
	d := p.ExoticDamp
	if d < 1 {
		d = 1
	}
	switch r.intn(12 * d) {
	case 0:
		c.MarketOrd = true
	case 1:
		c.PostOnly = true
	}
	switch r.intn(6 * d) {
	case 0:
		c.TIF = 1 // IOC
	case 1:
		c.TIF = 2 // FOK
	}
	// A per-order self-trade-prevention mode on one order in three, so all five
	// branches are reached in combination with the rest of the alphabet rather than
	// one at a time in a hand-written scenario.
	if v := r.intn(3 * d); v == 0 {
		c.STP = uint8(1 + r.intn(5))
	}
	if p.TradeGroups > 0 && r.intn(4*d) == 0 {
		c.TradeGroup = 1 + r.intn(p.TradeGroups)
	}
	if r.intn(40*d) == 0 {
		c.Privileged = true
	}
}

// Literal renders a tape as a compilable []tape.Cmd literal, ready to paste into a
// regression test verbatim. A differential failure that prints two thousand
// commands is a failure nobody will debug; one that prints nine and the line to
// paste them into is one somebody fixes before lunch.
func Literal(cmds []Cmd) string {
	var b strings.Builder
	b.WriteString("[]tape.Cmd{\n")
	for _, c := range cmds {
		var f []string
		add := func(format string, args ...any) { f = append(f, fmt.Sprintf(format, args...)) }
		add("Pos: %d", c.Pos)
		add("Kind: tape.%s", c.Kind)
		if c.User != "" {
			add("User: %q", c.User)
		}
		if c.Sell {
			add("Sell: true")
		}
		if c.MarketOrd {
			add("MarketOrd: true")
		}
		if c.Price != 0 {
			add("Price: %d", c.Price)
		}
		if c.Qty != 0 {
			add("Qty: %d", c.Qty)
		}
		if c.TIF != 0 {
			add("TIF: %d", c.TIF)
		}
		if c.PostOnly {
			add("PostOnly: true")
		}
		if c.STP != 0 {
			add("STP: %d", c.STP)
		}
		if c.TradeGroup != 0 {
			add("TradeGroup: %d", c.TradeGroup)
		}
		if c.Privileged {
			add("Privileged: true")
		}
		if c.Kind == Cancel || c.Kind == Reduce || c.Kind == Replace {
			add("Target: %d", c.Target)
		}
		if c.NewQty != 0 {
			add("NewQty: %d", c.NewQty)
		}
		if c.Kind == SetPhase {
			add("Phase: %d", c.Phase)
		}
		fmt.Fprintf(&b, "\t{%s},\n", strings.Join(f, ", "))
	}
	b.WriteString("}")
	return b.String()
}
