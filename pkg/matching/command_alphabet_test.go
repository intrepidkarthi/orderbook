package matching

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The durability guard from docs/JOURNAL-COMPLETENESS.md §4.4.
//
// Three mutating commands have escaped the journal so far. Reduce was applied and
// not recorded, so a restart brought an order back at its original size. An
// operator's Halt was applied and not recorded, so a restart brought a
// deliberately halted venue back Open. SetPhase was applied and not recorded, so a
// restart brought a venue back in the wrong phase with an un-uncrossed book and no
// auction prints.
//
// Each of the three was fixed with a test for that command. This file is the other
// half: a test for the SHAPE. CommandLog is a method-per-command interface, so a
// new mutating command needs a new method (a breaking change, therefore
// discouraged) and a new switch case (silent if forgotten) — the design makes the
// correct thing expensive and the wrong thing free. What follows does not fix that
// design; it makes forgetting it a failing test rather than a post-mortem.
//
// The table below must name EVERY cmdKind. There is no default, no "the rest are
// fine", and the read-only verdict costs a written reason rather than a flipped
// boolean — so a future author reclassifying a command has to say why, and a
// reviewer gets a sentence to disagree with.

// classification is what one command kind is, for durability purposes: either the
// CommandLog method that must record it, or a named reason it genuinely needs no
// record.
type classification struct {
	// journal is the CommandLog method this kind must reach. Empty means read-only.
	journal string
	// reason is why this kind needs no record. Required when journal is empty.
	reason string
	// sample is a representative command value, used to drive logCommand directly.
	// It answers "does this KIND reach the log", with no inference from which
	// public method was called.
	sample func(t *testing.T) command
	// drive prepares whatever the command needs and returns the closure that
	// issues it through the Runner's PUBLIC API. It answers the other half: that
	// the public entry point actually produces that kind.
	drive func(t *testing.T, r *Runner) func()
}

func journalled(method string, sample func(t *testing.T) command, drive func(t *testing.T, r *Runner) func()) classification {
	return classification{journal: method, sample: sample, drive: drive}
}

func readOnly(reason string, sample func(t *testing.T) command, drive func(t *testing.T, r *Runner) func()) classification {
	return classification{reason: reason, sample: sample, drive: drive}
}

func alphabetOrder(t *testing.T, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

// restForAlphabet puts one order on the book and returns its engine id, so the
// commands that name an existing order have something real to name.
func restForAlphabet(t *testing.T, r *Runner, side types.Side, price, qty int64) int64 {
	t.Helper()
	res := r.Process(alphabetOrder(t, "u", side, price, qty))
	if res == nil || res.Order == nil {
		t.Fatal("order not accepted")
	}
	return res.Order.ID
}

var commandClassification = map[cmdKind]classification{
	cmdSubmit: journalled("AppendSubmit",
		func(t *testing.T) command {
			return command{kind: cmdSubmit, order: alphabetOrder(t, "u", types.SideBuy, 100, 1)}
		},
		func(t *testing.T, r *Runner) func() {
			o := alphabetOrder(t, "u", types.SideBuy, 100, 1)
			return func() { r.Process(o) }
		}),

	cmdCancel: journalled("AppendCancel",
		func(t *testing.T) command { return command{kind: cmdCancel, cancelID: 1, userID: "u"} },
		func(t *testing.T, r *Runner) func() {
			id := restForAlphabet(t, r, types.SideBuy, 100, 1)
			return func() { r.Cancel(id, "u") }
		}),

	cmdStop: journalled("AppendStop",
		func(t *testing.T) command {
			s, err := types.NewStopOrder(alphabetOrder(t, "u", types.SideBuy, 100, 1), 105)
			if err != nil {
				t.Fatalf("NewStopOrder: %v", err)
			}
			return command{kind: cmdStop, stop: s}
		},
		func(t *testing.T, r *Runner) func() {
			s, err := types.NewStopOrder(alphabetOrder(t, "u", types.SideBuy, 100, 1), 105)
			if err != nil {
				t.Fatalf("NewStopOrder: %v", err)
			}
			return func() { r.ProcessStop(s) }
		}),

	cmdOCO: journalled("AppendOCO",
		func(t *testing.T) command { return command{kind: cmdOCO, oco: alphabetOCO(t)} },
		func(t *testing.T, r *Runner) func() {
			o := alphabetOCO(t)
			return func() { r.ProcessOCO(o) }
		}),

	cmdIceberg: journalled("AppendIceberg",
		func(t *testing.T) command { return command{kind: cmdIceberg, iceberg: alphabetIceberg(t)} },
		func(t *testing.T, r *Runner) func() {
			ib := alphabetIceberg(t)
			return func() { r.ProcessIceberg(ib) }
		}),

	cmdPegged: journalled("AppendPegged",
		func(t *testing.T) command { return command{kind: cmdPegged, pegged: alphabetPegged(t)} },
		func(t *testing.T, r *Runner) func() {
			p := alphabetPegged(t)
			return func() { r.ProcessPegged(p) }
		}),

	cmdTrailing: journalled("AppendTrailing",
		func(t *testing.T) command { return command{kind: cmdTrailing, trailing: alphabetTrailing(t)} },
		func(t *testing.T, r *Runner) func() {
			ts := alphabetTrailing(t)
			return func() { r.ProcessTrailingStop(ts) }
		}),

	cmdHalt: journalled("AppendHalt",
		func(t *testing.T) command { return command{kind: cmdHalt} },
		func(t *testing.T, r *Runner) func() { return func() { r.Halt() } }),

	cmdResume: journalled("AppendResume",
		func(t *testing.T) command { return command{kind: cmdResume} },
		func(t *testing.T, r *Runner) func() {
			r.Halt()
			return func() { r.Resume() }
		}),

	cmdCancelOnly: journalled("AppendCancelOnly",
		func(t *testing.T) command { return command{kind: cmdCancelOnly} },
		func(t *testing.T, r *Runner) func() { return func() { r.SetCancelOnly() } }),

	cmdSetMark: journalled("AppendSetMark",
		func(t *testing.T) command { return command{kind: cmdSetMark, cancelID: 10_000} },
		func(t *testing.T, r *Runner) func() { return func() { r.SetMarkPrice(10_000) } }),

	cmdBust: journalled("AppendBust",
		func(t *testing.T) command {
			return command{kind: cmdBust, cancelID: 1, bustReason: "erroneous order entry"}
		},
		func(t *testing.T, r *Runner) func() {
			restForAlphabet(t, r, types.SideSell, 100, 5)
			restForAlphabet(t, r, types.SideBuy, 100, 5) // prints trade 1
			return func() { r.Bust(1, "erroneous order entry") }
		}),

	cmdCancelAll: journalled("AppendCancelAll",
		func(t *testing.T) command { return command{kind: cmdCancelAll, userID: "u"} },
		func(t *testing.T, r *Runner) func() {
			restForAlphabet(t, r, types.SideBuy, 100, 1)
			return func() { r.CancelAllForUser("u") }
		}),

	cmdReduce: journalled("AppendReduce",
		func(t *testing.T) command {
			return command{kind: cmdReduce, cancelID: 1, reduceQty: 1, userID: "u"}
		},
		func(t *testing.T, r *Runner) func() {
			id := restForAlphabet(t, r, types.SideBuy, 100, 10)
			return func() { r.Reduce(id, 4, "u") }
		}),

	cmdReplace: journalled("AppendReplace",
		func(t *testing.T) command {
			return command{kind: cmdReplace, cancelID: 1, userID: "u", replace: alphabetOrder(t, "u", types.SideBuy, 99, 1)}
		},
		func(t *testing.T, r *Runner) func() {
			id := restForAlphabet(t, r, types.SideBuy, 100, 10)
			repl := alphabetOrder(t, "u", types.SideBuy, 99, 4)
			return func() {
				ch, err := r.TryReplaceAsync(id, "u", repl)
				if err != nil {
					t.Errorf("TryReplaceAsync: %v", err)
					return
				}
				<-ch
			}
		}),

	// SetPhase is the reason this table exists. It runs the opening or closing
	// uncross, prints the auction trades, and sets the state EngineSnapshot carries
	// — and it reached logCommand's default branch for three releases.
	cmdSetPhase: journalled("AppendSetPhase",
		func(t *testing.T) command { return command{kind: cmdSetPhase, phase: StatePreOpen} },
		func(t *testing.T, r *Runner) func() { return func() { r.SetPhase(StatePreOpen) } }),

	cmdOpenOrders: readOnly("a query: returns the account's resting orders and changes nothing",
		func(t *testing.T) command { return command{kind: cmdOpenOrders, userID: "u"} },
		func(t *testing.T, r *Runner) func() { return func() { r.OpenOrdersFor("u") } }),

	cmdTrailingCount: readOnly("a query: counts pending trailing stops on the matching goroutine",
		func(t *testing.T) command { return command{kind: cmdTrailingCount} },
		func(t *testing.T, r *Runner) func() { return func() { r.TrailingStopCount() } }),

	cmdIndicative: readOnly("a query: reports what an uncross WOULD produce, without running one",
		func(t *testing.T) command { return command{kind: cmdIndicative} },
		func(t *testing.T, r *Runner) func() { return func() { r.IndicativeAuction() } }),

	cmdCheckpoint: readOnly("takes a snapshot; it reads engine state and changes none of it",
		func(t *testing.T) command {
			return command{kind: cmdCheckpoint, snapOut: make(chan *EngineSnapshot, 1)}
		},
		func(t *testing.T, r *Runner) func() {
			return func() {
				if _, err := r.Checkpoint(); err != nil {
					t.Errorf("Checkpoint: %v", err)
				}
			}
		}),

	// ExpireDue is the one entry here that removes orders and is still not
	// journalled, so the reason has to carry more weight than the others.
	//
	// It cancels nothing of its own: it fires deadlines that were RESOLVED ON
	// INTAKE and stored on the order, which the submit record already carries (see
	// expiry.go, "Why the deadline is resolved on intake"). Replay reaches the same
	// verdict from the same deadlines without the command, and every command that
	// arrives during replay drives the same lazy sweep. A record would say only
	// "someone asked what time it was".
	cmdExpireDue: readOnly("fires deadlines the submit records already carry; replay re-derives them and cannot rewind the clock anyway",
		func(t *testing.T) command { return command{kind: cmdExpireDue} },
		func(t *testing.T, r *Runner) func() { return func() { r.ExpireDue() } }),
}

func alphabetOCO(t *testing.T) *types.OCOOrder {
	t.Helper()
	s, err := types.NewStopOrder(alphabetOrder(t, "u", types.SideSell, 90, 1), 90)
	if err != nil {
		t.Fatalf("NewStopOrder: %v", err)
	}
	o, err := types.NewOCOOrder(alphabetOrder(t, "u", types.SideSell, 110, 1), s)
	if err != nil {
		t.Fatalf("NewOCOOrder: %v", err)
	}
	return o
}

func alphabetIceberg(t *testing.T) *types.IcebergOrder {
	t.Helper()
	ib, err := types.NewIcebergOrder(alphabetOrder(t, "u", types.SideBuy, 100, 10), 2)
	if err != nil {
		t.Fatalf("NewIcebergOrder: %v", err)
	}
	return ib
}

func alphabetPegged(t *testing.T) *types.PeggedOrder {
	t.Helper()
	p, err := types.NewPeggedOrder(alphabetOrder(t, "u", types.SideBuy, 100, 1), types.PegToBid, 0)
	if err != nil {
		t.Fatalf("NewPeggedOrder: %v", err)
	}
	return p
}

func alphabetTrailing(t *testing.T) *types.TrailingStop {
	t.Helper()
	ts, err := types.NewTrailingStop(alphabetOrder(t, "u", types.SideSell, 100, 1), 5)
	if err != nil {
		t.Fatalf("NewTrailingStop: %v", err)
	}
	return ts
}

// loggedCall is one CommandLog method call, with the payload the durability of a
// phase transition actually depends on. Counting records is not enough: a journal
// that wrote StateOpen whatever the target phase was would satisfy a count.
type loggedCall struct {
	method string
	phase  EngineState
}

// recordingCommandLog is a CommandLog that records which method was called
// instead of writing anything.
type recordingCommandLog struct {
	calls []loggedCall
	seq   int64
}

var _ CommandLog = (*recordingCommandLog)(nil)

func (l *recordingCommandLog) record(method string) (int64, error) {
	l.calls = append(l.calls, loggedCall{method: method})
	l.seq++
	return l.seq, nil
}

func (l *recordingCommandLog) AppendSubmit(*types.Order) (int64, error) {
	return l.record("AppendSubmit")
}
func (l *recordingCommandLog) AppendCancel(int64, string) (int64, error) {
	return l.record("AppendCancel")
}

func (l *recordingCommandLog) AppendReduce(int64, int64, string) (int64, error) {
	return l.record("AppendReduce")
}

func (l *recordingCommandLog) AppendReplace(int64, string, *types.Order) (int64, error) {
	return l.record("AppendReplace")
}
func (l *recordingCommandLog) AppendCancelAll(string) (int64, error) {
	return l.record("AppendCancelAll")
}
func (l *recordingCommandLog) AppendStop(*types.StopOrder) (int64, error) {
	return l.record("AppendStop")
}
func (l *recordingCommandLog) AppendOCO(*types.OCOOrder) (int64, error) { return l.record("AppendOCO") }
func (l *recordingCommandLog) AppendIceberg(*types.IcebergOrder) (int64, error) {
	return l.record("AppendIceberg")
}

func (l *recordingCommandLog) AppendPegged(*types.PeggedOrder) (int64, error) {
	return l.record("AppendPegged")
}

func (l *recordingCommandLog) AppendTrailing(*types.TrailingStop) (int64, error) {
	return l.record("AppendTrailing")
}
func (l *recordingCommandLog) AppendHalt() (int64, error)       { return l.record("AppendHalt") }
func (l *recordingCommandLog) AppendResume() (int64, error)     { return l.record("AppendResume") }
func (l *recordingCommandLog) AppendCancelOnly() (int64, error) { return l.record("AppendCancelOnly") }
func (l *recordingCommandLog) AppendSetMark(int64) (int64, error) {
	return l.record("AppendSetMark")
}
func (l *recordingCommandLog) AppendBust(int64, string) (int64, error) { return l.record("AppendBust") }

func (l *recordingCommandLog) AppendSetPhase(phase EngineState) (int64, error) {
	l.calls = append(l.calls, loggedCall{method: "AppendSetPhase", phase: phase})
	l.seq++
	return l.seq, nil
}

func (l *recordingCommandLog) methods() []string {
	out := make([]string, 0, len(l.calls))
	for _, c := range l.calls {
		out = append(out, c.method)
	}
	return out
}

func alphabetCfg() Config {
	c := DefaultConfig("X")
	c.DedupClientOrderIDs = 256
	return c
}

// TestEveryCommandKindIsClassified is the guard that outlives this bug. Adding a
// command kind without deciding whether it is durable fails here, at the moment
// the constant is written, rather than after a crash three releases later.
func TestEveryCommandKindIsClassified(t *testing.T) {
	logMethods := map[string]bool{}
	iface := reflect.TypeOf((*CommandLog)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		logMethods[iface.Method(i).Name] = false
	}

	for k := cmdKind(0); k < cmdKindCount; k++ {
		c, ok := commandClassification[k]
		if !ok {
			t.Errorf("cmdKind %d is not in commandClassification: decide whether it is journalled "+
				"(and name the CommandLog method) or read-only (and say why). See docs/JOURNAL-COMPLETENESS.md §4.4", k)
			continue
		}
		switch {
		case c.journal != "" && c.reason != "":
			t.Errorf("cmdKind %d is classified both journalled (%s) and read-only (%q)", k, c.journal, c.reason)
		case c.journal == "" && c.reason == "":
			t.Errorf("cmdKind %d is classified read-only with no reason; a bare boolean is what this table exists to replace", k)
		case c.journal != "":
			if _, found := logMethods[c.journal]; !found {
				t.Errorf("cmdKind %d names CommandLog.%s, which does not exist on the interface", k, c.journal)
			} else {
				logMethods[c.journal] = true
			}
		}
		if c.sample == nil {
			t.Errorf("cmdKind %d has no sample command; the guard cannot drive what it cannot build", k)
		}
		if c.drive == nil {
			t.Errorf("cmdKind %d has no public driver; a kind nothing can issue is a kind nothing proves", k)
		}
	}

	for k := range commandClassification {
		if k >= cmdKindCount {
			t.Errorf("commandClassification holds cmdKind %d, which is past the sentinel — a stale entry", k)
		}
	}

	// The other direction, which catches the opposite mistake: a CommandLog method
	// added for a command that was never wired into logCommand's switch.
	for name, used := range logMethods {
		if !used {
			t.Errorf("CommandLog.%s is claimed by no command kind; either a command is missing "+
				"from logCommand or the method is dead", name)
		}
	}
}

// TestEveryMutatingCommandReachesTheLog is the test with teeth: it is the one that
// would have caught Reduce, Halt and SetPhase.
//
// It drives each journalled kind twice — straight through logCommand, which
// answers "does this KIND reach the log", and again through the Runner's public
// API, which answers "does the public entry point produce that kind" — and
// requires exactly one record of the expected method each time.
func TestEveryMutatingCommandReachesTheLog(t *testing.T) {
	for k := cmdKind(0); k < cmdKindCount; k++ {
		// Comma-ok, not a bare index. An unclassified kind yields a zero-value
		// classification whose sample is nil, and calling it panics — which kills the
		// test binary and takes every other test in this package down with it, hiding
		// the very message TestEveryCommandKindIsClassified exists to print. That test
		// reports the missing entry; this one must not pre-empt it with a stack trace.
		c, ok := commandClassification[k]
		if !ok || c.journal == "" {
			continue
		}

		t.Run("switch/"+c.journal, func(t *testing.T) {
			log := &recordingCommandLog{}
			// No goroutine: logCommand is called directly, so the assertion is about
			// the switch and nothing else.
			r := &Runner{log: log}
			r.logCommand(c.sample(t))
			if got := log.methods(); len(got) != 1 || got[0] != c.journal {
				t.Fatalf("cmdKind %d wrote %v, want exactly one %s — a mutating command that is "+
					"applied and not recorded is state the log cannot reproduce", k, got, c.journal)
			}
		})

		t.Run("public/"+c.journal, func(t *testing.T) {
			log := &recordingCommandLog{}
			r := NewRunner(RunnerConfig{Engine: alphabetCfg(), Log: log})
			defer r.Close()
			issue := c.drive(t, r)
			mark := len(log.calls)
			issue()
			got := log.methods()[mark:]
			if len(got) != 1 || got[0] != c.journal {
				t.Fatalf("the public entry point for cmdKind %d wrote %v, want exactly one %s", k, got, c.journal)
			}
		})
	}
}

// TestSetPhaseRecordsItsTargetPhase is the payload half of the test above. A
// journal that recorded a phase transition but always wrote StateOpen would
// satisfy "exactly one AppendSetPhase" and still restore a venue to a phase it was
// never in.
func TestSetPhaseRecordsItsTargetPhase(t *testing.T) {
	log := &recordingCommandLog{}
	r := NewRunner(RunnerConfig{Engine: alphabetCfg(), Log: log})
	defer r.Close()

	want := []EngineState{StatePreOpen, StateOpen, StateClosingAuction, StateClosed, StateHalted, StateCancelOnly}
	for _, p := range want {
		r.SetPhase(p)
	}
	if len(log.calls) != len(want) {
		t.Fatalf("logged %d calls for %d transitions", len(log.calls), len(want))
	}
	for i, c := range log.calls {
		if c.method != "AppendSetPhase" {
			t.Fatalf("call %d is %s, want AppendSetPhase", i, c.method)
		}
		if c.phase != want[i] {
			t.Errorf("record %d carries phase %s, want %s", i, c.phase, want[i])
		}
	}
}

// TestReadOnlyCommandsWriteNothing stops the table being gamed. Without it, the
// cheap way to make the test above pass for an awkward command is to call it
// read-only.
func TestReadOnlyCommandsWriteNothing(t *testing.T) {
	for k := cmdKind(0); k < cmdKindCount; k++ {
		// Comma-ok for the same reason as above: an unclassified kind must reach
		// TestEveryCommandKindIsClassified's sentence, not a nil-func panic here.
		c, ok := commandClassification[k]
		if !ok || c.journal != "" {
			continue
		}
		// Subtests are named by kind. Go dedupes identical subtest names into
		// "switch#01", "switch#02" …, so a bare "switch" reports a failure the reader
		// has to count table entries to identify.
		name := fmt.Sprintf("kind%d", k)

		t.Run(name+"/switch", func(t *testing.T) {
			log := &recordingCommandLog{}
			r := &Runner{log: log}
			r.logCommand(c.sample(t))
			if got := log.methods(); len(got) != 0 {
				t.Fatalf("cmdKind %d is classified read-only (%q) and wrote %v", k, c.reason, got)
			}
		})

		t.Run(name+"/public", func(t *testing.T) {
			log := &recordingCommandLog{}
			r := NewRunner(RunnerConfig{Engine: alphabetCfg(), Log: log})
			defer r.Close()
			issue := c.drive(t, r)
			mark := len(log.calls)
			issue()
			if got := log.methods()[mark:]; len(got) != 0 {
				t.Fatalf("cmdKind %d is classified read-only (%q) and its public entry point wrote %v", k, c.reason, got)
			}
		})
	}
}

// TestReadOnlyCommandsChangeNoRecoverableState is what makes the read-only verdict
// a CHECKED claim rather than an author's assurance.
//
// TestReadOnlyCommandsWriteNothing only catches the case where logCommand still
// writes a record. It does not catch the shape that actually produced all three
// escapes: a command that mutates the engine, is journalled nowhere, and is
// classified read-only because it "looked like it carried no book state". Reduce,
// Halt and SetPhase were each exactly that, and each would have passed a guard
// built only on the author's prose.
//
// So the prose is checked against the digest. EngineSnapshot.Digest() covers
// everything a recovery must reproduce — the book, the trading state, the mark, the
// sequence counters — which makes it the mechanical statement of "changes nothing
// recovery must reproduce". A read-only command must leave it byte-identical. A
// command that moves it is journalled or it is a defect; there is no third answer,
// and no reason string can settle it.
func TestReadOnlyCommandsChangeNoRecoverableState(t *testing.T) {
	for k := cmdKind(0); k < cmdKindCount; k++ {
		c, ok := commandClassification[k]
		if !ok || c.journal != "" {
			continue
		}

		t.Run(fmt.Sprintf("kind%d", k), func(t *testing.T) {
			r := NewRunner(RunnerConfig{Engine: alphabetCfg()})
			defer r.Close()

			// The driver's setup runs first and is allowed to change the book — it is
			// how the command gets something real to act on. Only the issue() itself
			// is under test, so the digest is taken after setup and before it.
			issue := c.drive(t, r)

			before, err := r.Checkpoint()
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			issue()
			after, err := r.Checkpoint()
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}

			if got, want := after.Digest(), before.Digest(); got != want {
				t.Fatalf("cmdKind %d is classified read-only (%q) but moved the engine digest:\n"+
					" before %s\n  after %s\nIt changes state a recovery must reproduce, so it "+
					"needs a CommandLog method and a logCommand case, not a reason. "+
					"See docs/JOURNAL-COMPLETENESS.md §4.4", k, c.reason, want, got)
			}
		})
	}
}
