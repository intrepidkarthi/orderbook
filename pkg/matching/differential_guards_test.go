package matching

// Guards that keep the differential harness from quietly narrowing.
//
// docs/JOURNAL-COMPLETENESS.md §1 diagnosed an exhaustive test running over an
// incomplete COMMAND alphabet: it reported completeness, and the report was
// load-bearing. This file applies that lesson twice — once to the command alphabet
// again, and once to a second alphabet that document never had to consider, the
// engine's CONFIGURATION. A knob turned on without the model knowing about it is an
// oracle switched off, and the natural fix once the failures start ("ignore
// rejections whose reason the model does not know") is the switch.

import (
	"fmt"
	"hash/fnv"
	"reflect"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/refmatch"
	"github.com/intrepidkarthi/orderbook/internal/tape"
)

// --- axis one: Config ----------------------------------------------------------

// configRole is what one Config field is, as far as the model is concerned.
type configRole struct {
	// kind is "modelled", "mustBeZero" or "harnessOwned".
	kind string
	// why is required for every role. A future author turning a knob on has to write
	// down why the model can now cope, and a reviewer gets a sentence to disagree
	// with instead of a flipped flag.
	why string
}

func modelled(why string) configRole     { return configRole{kind: "modelled", why: why} }
func mustBeZero(why string) configRole   { return configRole{kind: "mustBeZero", why: why} }
func harnessOwned(why string) configRole { return configRole{kind: "harnessOwned", why: why} }

var configClassification = map[string]configRole{
	"Symbol":              harnessOwned("the harness names the instrument and gives the model the same name"),
	"Clock":               harnessOwned("the harness injects a deterministic step clock; see TestSameSeedSameObservations"),
	"EventSink":           harnessOwned("the harness attaches the recorder that turns events into the model's vocabulary"),
	"SelfTradePrevention": modelled("all five modes are tier-1 matching semantics; see REFERENCE-MATCHER.md §2.3"),
	"ProRata":             modelled("the second allocation rule, specified as prose in REFERENCE-MATCHER.md §2.3"),
	"MaxOrders":           modelled("a book-size cap changes a verdict, so the model applies it"),
	"ShardIndex":          modelled("id composition is customer-visible, so the model composes ids the same way"),

	"PriceBand":           mustBeZero("an admission control on the decimal path; tier-1 tapes run with no collar"),
	"BandBreachPause":     mustBeZero("turns a band breach into a timed halt, which reads the wall clock the model does not have"),
	"MinRestingTime":      mustBeZero("reads the wall clock for a cancel/reduce decision; see REFERENCE-MATCHER.md §3.3(a)"),
	"SessionClose":        mustBeZero("only a DAY order consults it, and DAY is tier 2"),
	"Guardrail":           mustBeZero("a self-output tripwire measured over a wall-clock window"),
	"DisabledClasses":     mustBeZero("gates the exotic families, none of which tier 1 submits"),
	"MaxOrderQty":         mustBeZero("an admission control: it decides whether an order reaches the walk"),
	"MaxOrderNotional":    mustBeZero("an admission control on order size"),
	"MinOrderQty":         mustBeZero("a dust floor, which is admission and not matching"),
	"MinOrderNotional":    mustBeZero("a dust floor, which is admission and not matching"),
	"MaxOrdersPerAccount": mustBeZero("a per-account book-footprint cap; admission, not matching"),
	"DedupClientOrderIDs": mustBeZero("the tape carries no client order ids, and the ring is recovered state the model does not hold"),
	"MaxMarkStep":         mustBeZero("bounds a mark-price update, which only the band consumes"),
	"MinMarkDepth":        mustBeZero("bounds a mark-price update, which only the band consumes"),
	"MarkDepthBand":       mustBeZero("bounds a mark-price update, which only the band consumes"),
	"MaxForceTradeQty":    mustBeZero("caps ForceTrade, which is the risk layer's entry point and not on the tape"),
	"IcebergPeakJitter":   mustBeZero("varies an iceberg reload size, and icebergs are tier 2"),
}

// TestEveryConfigFieldIsClassified makes adding a Config field without deciding
// whether the model knows about it a failure at the moment the field is written.
func TestEveryConfigFieldIsClassified(t *testing.T) {
	ct := reflect.TypeOf(Config{})
	for i := 0; i < ct.NumField(); i++ {
		name := ct.Field(i).Name
		role, ok := configClassification[name]
		if !ok {
			t.Errorf("Config.%s is not in configClassification: decide whether the differential model "+
				"implements it (modelled), whether the harness must leave it off (mustBeZero), or whether "+
				"the harness supplies it (harnessOwned) — and say why. See docs/REFERENCE-MATCHER.md §2.5", name)
			continue
		}
		if role.why == "" {
			t.Errorf("Config.%s is classified %s with no reason; a bare label is what this table exists to replace", name, role.kind)
		}
		switch role.kind {
		case "modelled", "mustBeZero", "harnessOwned":
		default:
			t.Errorf("Config.%s carries unknown role %q", name, role.kind)
		}
	}
	for name := range configClassification {
		if _, ok := ct.FieldByName(name); !ok {
			t.Errorf("configClassification names Config.%s, which does not exist — a stale entry", name)
		}
	}
}

// TestHarnessConfigMatchesItsClassification is the one with teeth. It fails if
// someone "improves" the harness by turning on a control the model cannot predict,
// which would otherwise show up as a stream of divergences whose obvious fix is to
// stop comparing.
func TestHarnessConfigMatchesItsClassification(t *testing.T) {
	for _, p := range diffProfiles() {
		cfg := p.engine()
		v := reflect.ValueOf(cfg)
		ct := v.Type()
		for i := 0; i < ct.NumField(); i++ {
			name := ct.Field(i).Name
			if configClassification[name].kind != "mustBeZero" {
				continue
			}
			if !v.Field(i).IsZero() {
				t.Errorf("profile %s sets Config.%s to a non-zero value, but it is classified mustBeZero (%q). "+
					"The model does not implement it, so the harness would be comparing a model that does not "+
					"know about a control against an engine that is applying it",
					p.name, name, configClassification[name].why)
			}
		}
	}
}

// TestModelledConfigFieldsAreExercised catches the config-axis form of a zero
// generator weight: a field classified modelled and never actually set by any
// profile reports as coverage while proving nothing.
func TestModelledConfigFieldsAreExercised(t *testing.T) {
	exercised := map[string]bool{}
	for _, p := range diffProfiles() {
		v := reflect.ValueOf(p.engine())
		ct := v.Type()
		for i := 0; i < ct.NumField(); i++ {
			if !v.Field(i).IsZero() {
				exercised[ct.Field(i).Name] = true
			}
		}
	}
	for name, role := range configClassification {
		if role.kind == "modelled" && !exercised[name] {
			t.Errorf("Config.%s is classified modelled (%q) but no harness profile sets it away from its "+
				"zero value, so nothing in the sweep depends on the model implementing it", name, role.why)
		}
	}
}

// --- axis two: the command alphabet --------------------------------------------

// tier is what one command kind is, for the model.
type tier struct {
	// modelledAs is the tape kind that issues this command, when the model
	// implements it.
	modelledAs tape.Kind
	isModelled bool
	// deferredTo is the tier that will teach the model this command, when one will.
	deferredTo int
	why        string
}

func modelledAs(k tape.Kind) tier { return tier{modelledAs: k, isModelled: true} }
func deferredToTier(n int, why string) tier {
	return tier{deferredTo: n, why: why}
}
func notACommand(why string) tier { return tier{why: why} }

var commandTier = map[cmdKind]tier{
	cmdSubmit:     modelledAs(tape.Submit),
	cmdCancel:     modelledAs(tape.Cancel),
	cmdReduce:     modelledAs(tape.Reduce),
	cmdReplace:    modelledAs(tape.Replace),
	cmdCancelAll:  modelledAs(tape.CancelAll),
	cmdHalt:       modelledAs(tape.Halt),
	cmdResume:     modelledAs(tape.Resume),
	cmdCancelOnly: modelledAs(tape.CancelOnly),

	cmdStop:     deferredToTier(2, "a trigger that composes with the matching core: a fired stop becomes an ordinary taker"),
	cmdOCO:      deferredToTier(2, "a pairing rule on top of a matching walk it does not change"),
	cmdIceberg:  deferredToTier(2, "a refill rule on top of a matching walk it does not change"),
	cmdPegged:   deferredToTier(2, "resolves a price from the book and then submits an ordinary limit"),
	cmdTrailing: deferredToTier(2, "a ratcheting trigger; the same argument as cmdStop"),
	cmdSetPhase: deferredToTier(2, "modelling the uncross means modelling pkg/auction's clearing-price choice, a SECOND algorithm where a wrong model is a wrong oracle. It is not un-oracled: the boundary sweeps compare every auction against its own replay"),
	cmdBust:     deferredToTier(2, "a bust deliberately does not rewind the book, so its differential content is a registry comparison, not a matching one"),
	cmdSetMark:  deferredToTier(2, "the mark is only ever consumed by the price band, and the band is mustBeZero here"),

	cmdOpenOrders:    notACommand("a query: returns the account's resting orders and changes nothing"),
	cmdTrailingCount: notACommand("a query: counts pending trailing stops"),
	cmdIndicative:    notACommand("a query: reports what an uncross would produce without running one"),
	cmdCheckpoint:    notACommand("takes a snapshot; the harness compares one after every command already"),
	cmdExpireDue:     notACommand("fires deadlines, and every deadline-bearing time-in-force is tier 2"),
}

// TestEveryCommandKindHasATier makes adding a cmdKind without deciding whether the
// model needs to learn it a failure at the moment the constant is written — the one
// point at which the author still knows the answer.
func TestEveryCommandKindHasATier(t *testing.T) {
	for k := cmdKind(0); k < cmdKindCount; k++ {
		ti, ok := commandTier[k]
		if !ok {
			t.Errorf("cmdKind %d has no tier: decide whether the reference model implements it "+
				"(modelledAs a tape kind), defers it (deferredToTier with a reason), or whether it is "+
				"not a command at all. See docs/REFERENCE-MATCHER.md §4.2", k)
			continue
		}
		if !ti.isModelled && ti.why == "" {
			t.Errorf("cmdKind %d is deferred or excluded with no reason", k)
		}
		if ti.isModelled && ti.modelledAs >= tape.KindCount {
			t.Errorf("cmdKind %d claims tape kind %d, which is past the sentinel", k, ti.modelledAs)
		}
	}
	for k := range commandTier {
		if k >= cmdKindCount {
			t.Errorf("commandTier holds cmdKind %d, which is past the sentinel — a stale entry", k)
		}
	}
	// Cross-check against the durability classification: the two tables enumerate
	// the same set, and a kind in one and not the other means one of them is stale.
	for k := cmdKind(0); k < cmdKindCount; k++ {
		_, inDurability := commandClassification[k]
		_, inTier := commandTier[k]
		if inDurability != inTier {
			t.Errorf("cmdKind %d is in commandClassification=%t but commandTier=%t", k, inDurability, inTier)
		}
	}
}

// TestEveryModelledCommandIsGenerated is the guard against a generator weight of
// zero, which is the config-free form of an incomplete alphabet and which reports as
// coverage exactly the way the superseded tape reported completeness while never
// emitting a phase transition.
func TestEveryModelledCommandIsGenerated(t *testing.T) {
	drawn := map[tape.Kind]int{}
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			for _, c := range tape.Gen(p.tapes, seed, p.n) {
				drawn[c.Kind]++
			}
		}
	}
	for k, ti := range commandTier {
		if !ti.isModelled {
			continue
		}
		if drawn[ti.modelledAs] == 0 {
			t.Errorf("cmdKind %d is classified modelled as tape.%s, and the committed sweep draws it zero "+
				"times. A kind the generator never emits is a kind the model is not checked on", k, ti.modelledAs)
		}
	}
}

// --- the sweep is load-bearing --------------------------------------------------

// TestDifferentialSweepReachesEveryOutcome stops the sweep decaying into decoration.
//
// Drawing a command kind is not the same as reaching the behaviour behind it. The
// first version of this generator drew reduce commands at an 8% weight and reached a
// SUCCESSFUL reduce zero times in sixteen tapes, because it named the target's owner
// independently of the target — so every one of them came back "not found", and the
// coverage number said the verb was exercised. This asserts the OUTCOMES.
func TestDifferentialSweepReachesEveryOutcome(t *testing.T) {
	// Reachable from a tier-1 tape, and asserted reached.
	want := map[refmatch.Verdict]string{
		{Status: refmatch.StatusNew}:                                                 "a limit order rests",
		{Status: refmatch.StatusPartiallyFilled}:                                     "a limit order fills part way and rests the rest",
		{Status: refmatch.StatusFilled}:                                              "an order fills completely",
		{Status: refmatch.StatusCancelled}:                                           "an IOC/market remainder or an STP-cancelled taker",
		{Status: refmatch.StatusReduced}:                                             "a reduce shrinks a resting order in place",
		{Status: refmatch.StatusCancelled, Reason: refmatch.RejectMarketNoLiquidity}: "a market order finds nothing",
		{Status: refmatch.StatusRejected, Reason: refmatch.RejectTradingHalted}:      "an order arrives into a halted venue",
		{Status: refmatch.StatusRejected, Reason: refmatch.RejectNewOrdersHalted}:    "an order arrives into a cancel-only venue",
		{Status: refmatch.StatusRejected, Reason: refmatch.RejectPostOnlyWouldCross}: "a post-only order would take",
		{Status: refmatch.StatusRejected, Reason: refmatch.RejectFOKCannotFill}:      "a fill-or-kill order is unwound",
		{Status: refmatch.StatusRejected, Reason: refmatch.RejectOrderBookFull}:      "the book-size cap refuses a resting remainder",
		{Status: refmatch.StatusRejected, Reason: refmatch.RejectOrderNotFound}:      "a command names an order that is not there",
		{Status: refmatch.StatusRejected, Reason: refmatch.RejectInvalidQuantity}:    "a reduce that is not a reduction",
	}
	// Mapped for totality and NOT reachable from a tape, each with the reason. They
	// are listed rather than omitted so the two sets together account for the whole
	// enum: a reason that quietly moved from one list to the other is a change
	// somebody has to make deliberately.
	_ = map[refmatch.Reject]string{
		refmatch.RejectOrderNotActive:   "a resting order is always live; only a stop or an already-filled order is not, and both are tier 2",
		refmatch.RejectNilOrder:         "the driver never builds a replace with no replacement",
		refmatch.RejectNotionalOverflow: "the tape's prices and quantities are two digits; overflow needs values a generated tape does not draw",
	}

	seen := map[refmatch.Verdict]bool{}
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			eng := newEngineSide(p.engine())
			for _, c := range tape.Gen(p.tapes, seed, p.n) {
				v, _, err := eng.apply(c)
				if err != nil {
					t.Fatalf("%s/seed=%d: %v", p.name, seed, err)
				}
				eng.rec.take()
				seen[v] = true
			}
		}
	}
	for v, what := range want {
		if !seen[v] {
			t.Errorf("the committed sweep never reaches %+v (%s). Either the generator has drifted away "+
				"from it or the engine no longer produces it; both are findings", v, what)
		}
	}
}

// TestSelfTradePreventionModesAreAllReached is the same argument one level finer.
// All five modes are the single most likely place in this engine to be
// self-consistently wrong, and a sweep that drew only three of them would still look
// like a sweep over STP.
//
// It asserts modes DECIDED, not modes drawn. The first version counted draws, which
// is the same kind-count-instead-of-outcome mistake this file's neighbours were
// written to correct: a mode carried by orders that never meet their own resting
// liquidity is a mode the sweep reports as exercised and never exercises. The
// decision counter comes from the model, which is where the rule is specified.
func TestSelfTradePreventionModesAreAllReached(t *testing.T) {
	names := map[int]string{1: "CANCEL_NEWEST", 2: "CANCEL_OLDEST", 3: "CANCEL_BOTH", 4: "DECREMENT", 5: "ALLOW"}

	drawn := map[uint8]int{}
	var decided [6]int
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			cmds := tape.Gen(p.tapes, seed, p.n)
			mod := newModelSide(p.model)
			for _, c := range cmds {
				if c.Kind == tape.Submit || c.Kind == tape.Replace {
					drawn[c.STP]++
				}
				if _, err := mod.apply(c); err != nil {
					t.Fatalf("%s/seed=%d: %v", p.name, seed, err)
				}
			}
			got := mod.m.STPDecisions()
			for i := range decided {
				decided[i] += got[i]
			}
		}
	}

	for mode := uint8(1); mode <= 5; mode++ {
		if drawn[mode] == 0 {
			t.Errorf("no order in the committed sweep carries per-order STP mode %s", names[int(mode)])
		}
	}
	if drawn[0] == 0 {
		t.Error("no order in the committed sweep leaves STP unset, so the venue default is never the one that decides")
	}
	// The stronger half: the mode has to have actually governed a self-match.
	for mode := 1; mode <= 5; mode++ {
		if decided[mode] == 0 {
			t.Errorf("STP mode %s is drawn %d times but never DECIDES a match in the whole sweep. Drawing a "+
				"mode is not exercising it — the order has to meet its own resting liquidity",
				names[mode], drawn[uint8(mode)])
		}
	}
	t.Logf("STP decisions across the committed sweep, by mode: %v", decided[1:])
}

// TestOrderAttributesAreGenerated closes the last unguarded alphabet in the tape.
//
// The guards around it enumerate command kinds against a sentinel, Config fields by
// reflection, verdict outcomes and the five STP modes. None of them can see the two
// ORDER ATTRIBUTES that decide whether self-trade prevention fires across accounts
// at all, or is bypassed entirely: TradeGroupID and Privileged. Making fillOrder
// draw both values and discard them — preserving the LCG stream so no other draw
// shifts — leaves `go test ./pkg/matching ./internal/tape` fully green while
// silently removing 436 trade-group orders and 39 privileged orders from the sweep.
//
// That is the incomplete-alphabet failure one axis over from where it was last
// found, so it gets a guard rather than a comment.
func TestOrderAttributesAreGenerated(t *testing.T) {
	var tradeGroup, privileged, postOnly, market, ioc, fok int
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			for _, c := range tape.Gen(p.tapes, seed, p.n) {
				if c.Kind != tape.Submit && c.Kind != tape.Replace {
					continue
				}
				if c.TradeGroup != 0 {
					tradeGroup++
				}
				if c.Privileged {
					privileged++
				}
				if c.PostOnly {
					postOnly++
				}
				if c.MarketOrd {
					market++
				}
				switch c.TIF {
				case 1:
					ioc++
				case 2:
					fok++
				}
			}
		}
	}
	for _, a := range []struct {
		n    int
		name string
		why  string
	}{
		{tradeGroup, "TradeGroupID", "self-trade prevention across two different accounts is unreachable without it"},
		{privileged, "Privileged", "the liquidation exemption that BYPASSES self-trade prevention is never taken"},
		{postOnly, "PostOnly", "the maker-only rejection is never reached"},
		{market, "MarketOrd", "the market-order branch of the walk is never taken"},
		{ioc, "IOC", "the immediate-or-cancel remainder rule is never applied"},
		{fok, "FOK", "the fill-or-kill unwind — the path §9(a) predicted a defect on — is never run"},
	} {
		if a.n == 0 {
			t.Errorf("the committed sweep generates zero orders carrying %s, so %s", a.name, a.why)
		}
	}
	t.Logf("order attributes across the committed sweep: tradeGroup=%d privileged=%d postOnly=%d market=%d ioc=%d fok=%d",
		tradeGroup, privileged, postOnly, market, ioc, fok)
}

// TestDifferentialTapeIsPinned protects the recorded evidence.
//
// This slice's argument rests on measurements taken against specific tapes: 21
// deliberate engine mutations caught, each shrinking to a named 1-4 command
// reproduction, and three engine defects found at named seeds. All of that is
// invalidated, silently, by any change to the generator that shifts the draw stream
// — including a change intended to be neutral. When ExoticDamp was added, the first
// version split one draw into two, which would have re-rolled every differential
// tape while every test still passed.
//
// So the committed sweep's tapes are pinned by digest. This is a golden, and goldens
// are avoided in this package for good reasons (see tape.Vector's comment) — but the
// thing being pinned here is not the alphabet, it is the claim that the alphabet did
// not change by accident. Widening it deliberately means updating this number AND
// re-running the mutation evidence in docs/REFERENCE-MATCHER.md §7.
//
// The pinned value is the digest of the tapes as they stood when that evidence was
// gathered. ExoticDamp was introduced afterwards and is a no-op at damp 0/1 by
// construction — every denominator is multiplied by 1 and no draw is added or
// removed — and the corroboration is that three independent counts taken on the
// pre-ExoticDamp tree still hold exactly here: 436 trade-group orders, 39 privileged
// orders, and STP decisions of 47/9/5/18/4 across the five modes.
func TestDifferentialTapeIsPinned(t *testing.T) {
	h := fnv.New64a()
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			for _, c := range tape.Gen(p.tapes, seed, p.n) {
				fmt.Fprintf(h, "%+v", c)
			}
		}
	}
	const want = "d540ce837a0b4a58"
	if got := fmt.Sprintf("%016x", h.Sum64()); got != want {
		t.Errorf("the committed differential sweep's tapes have changed (digest %s, pinned %s).\n\n"+
			"If that was DELIBERATE — a wider alphabet, a new profile, a new seed — update the constant "+
			"and re-run the mutation evidence in docs/REFERENCE-MATCHER.md §7, because every shrunk "+
			"reproduction recorded there names commands in these tapes.\n\n"+
			"If it was NOT deliberate, a generator change that was meant to be neutral has re-rolled the "+
			"stream, and the sweep is no longer testing what its evidence says it tests.", got, want)
	}
}

// TestDifferentialTapesAreDeletionClosed pins the property the shrinker rests on:
// a command names an earlier order by the POSITION of the submit that created it,
// and that position is strictly earlier, so any subsequence is still a valid tape.
func TestDifferentialTapesAreDeletionClosed(t *testing.T) {
	for _, p := range diffProfiles() {
		for _, seed := range p.seeds {
			cmds := tape.Gen(p.tapes, seed, p.n)
			for i, c := range cmds {
				if c.Pos != i {
					t.Fatalf("%s/seed=%d: command %d carries Pos %d; Target names a Pos, so the two must agree",
						p.name, seed, i, c.Pos)
				}
				switch c.Kind {
				case tape.Cancel, tape.Reduce, tape.Replace:
					if c.Target < 0 || c.Target >= c.Pos {
						t.Fatalf("%s/seed=%d: command %d targets position %d, which is not strictly earlier — "+
							"deleting commands would then change what remains means", p.name, seed, i, c.Target)
					}
				}
			}
		}
	}
}

// TestShrinkerFindsTheSmallestTape exercises the shrinker itself against a planted
// failure, so a broken shrinker fails here rather than by printing an unhelpfully
// large tape on the day something real breaks.
func TestShrinkerFindsTheSmallestTape(t *testing.T) {
	p := diffProfiles()[0]
	cmds := tape.Gen(p.tapes, 1, 60)
	// The planted predicate: "diverges" exactly when the tape still contains the
	// command at position 30. A correct deletion shrinker reduces to that one
	// command; a broken one reduces to more, or loses it.
	pred := func(cand []tape.Cmd) (string, bool) {
		for _, c := range cand {
			if c.Pos == 30 {
				return "planted", true
			}
		}
		return "", false
	}
	got := tape.Shrink(cmds, "planted", pred, 1000)
	if len(got.Cmds) != 1 || got.Cmds[0].Pos != 30 {
		t.Fatalf("shrunk to %d commands (%s), want exactly the one at Pos 30",
			len(got.Cmds), tape.Literal(got.Cmds))
	}
}

// TestSweepSizeIsRecorded is not an assertion so much as a place the numbers live,
// so §7.3's "is the sweep load-bearing or decorative" question has data next to it.
func TestSweepSizeIsRecorded(t *testing.T) {
	total := 0
	for _, p := range diffProfiles() {
		total += len(p.seeds) * p.n
	}
	t.Log(fmt.Sprintf("committed differential sweep: %d profiles, %d commands compared", len(diffProfiles()), total))
	if total < 1000 {
		t.Errorf("the committed sweep is only %d commands; that is small enough that a single seed would do "+
			"the same work, which is the argument against having a sweep at all", total)
	}
}
