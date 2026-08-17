package matching

import (
	"errors"
	"testing"
)

// Deliverable 5 of docs/JOURNAL-COMPLETENESS.md, and the thing that makes writing
// a phase into the log as a NAME safe rather than merely nicer to read.
//
// EngineState now has two mappings that can drift: String, which the writer uses
// to encode a KindSetPhase, and ParseEngineState, which replay uses to decode one.
// Two mappings that can drift are worse than one that is ugly, and the only thing
// stopping the drift is a test that ENUMERATES the block rather than restating it
// — which is what engineStateCount is for. A test with a hand-written list of the
// six states would pass forever after a seventh was added.

// TestEngineStateNamesRoundTrip walks every declared EngineState and requires
// ParseEngineState(s.String()) == s.
//
// The failure it exists to catch is specific and silent. String's default arm
// returns "OPEN", so a state added to the iota block without its own String case
// renders as "OPEN", is journalled as "OPEN", and replays as StateOpen — a phase
// transition durably recorded under a name that means a different phase, with no
// error anywhere. Enumerating to the sentinel is what turns that into a failure at
// the moment the constant is written.
func TestEngineStateNamesRoundTrip(t *testing.T) {
	if engineStateCount == 0 {
		t.Fatal("engineStateCount is zero; the sentinel must stay last in the block")
	}

	seen := map[string]EngineState{}
	for s := EngineState(0); s < engineStateCount; s++ {
		name := s.String()

		// A duplicate name is the symptom of the missing-String-case bug, and it is
		// worth naming separately because it says WHICH pair collided.
		if prev, dup := seen[name]; dup {
			t.Errorf("EngineState(%d) and EngineState(%d) both render as %q — one of them is "+
				"missing a String case and is falling through to the default arm, so its phase "+
				"records would be written under a name that means the other state", prev, s, name)
		}
		seen[name] = s

		got, err := ParseEngineState(name)
		if err != nil {
			t.Errorf("EngineState(%d).String() = %q, which ParseEngineState refuses: %v — "+
				"a phase this build can WRITE and cannot READ is a log it cannot recover from", s, name, err)
			continue
		}
		if got != s {
			t.Errorf("EngineState(%d) round-trips to EngineState(%d) via %q; String and "+
				"ParseEngineState have drifted apart", s, got, name)
		}
	}

	if len(seen) != int(engineStateCount) {
		t.Errorf("%d declared states share %d distinct names", engineStateCount, len(seen))
	}
}

// TestParseEngineStateRefusesUnknownNames is the other half of §4.1's argument for
// names over ordinals: an unknown name must fail LOUDLY.
//
// A log segment outlives the build that wrote it, so a phase a newer build
// invented will reach an older reader. As an ordinal it would decode into a
// valid-looking EngineState nobody ever defined and recovery would carry on into a
// phase that does not exist. As a name it is refusable, and refused.
func TestParseEngineStateRefusesUnknownNames(t *testing.T) {
	for _, name := range []string{
		"",
		"AUCTION_FREEZE", // the hypothetical newer phase §4.1 is written around
		"open",           // case matters: these are wire names, not prose
		"6",              // an ordinal that leaked into the name field
		"OPEN ",
	} {
		got, err := ParseEngineState(name)
		if err == nil {
			t.Errorf("ParseEngineState(%q) = %v with no error; an unrecognised phase must be "+
				"refused rather than silently resolved to a real one", name, got)
			continue
		}
		if !errors.Is(err, ErrUnknownEngineState) {
			t.Errorf("ParseEngineState(%q) error = %v, which does not wrap ErrUnknownEngineState; "+
				"callers distinguish an unknown phase from an I/O failure with errors.Is", name, err)
		}
	}
}

// TestEngineStateSentinelIsNotAState guards the enumeration itself. The sentinel is
// a bound, not a phase, and it renders as "OPEN" through String's default arm — so
// a loop written as `s <= engineStateCount` would quietly assert that the bound is
// a valid state and pass.
func TestEngineStateSentinelIsNotAState(t *testing.T) {
	declared := []EngineState{
		StateOpen, StateCancelOnly, StateHalted,
		StatePreOpen, StateClosed, StateClosingAuction,
	}
	if int(engineStateCount) != len(declared) {
		t.Fatalf("engineStateCount is %d but %d states are declared — a state was added to the "+
			"iota block without being given a name here, and TestEngineStateNamesRoundTrip is the "+
			"test that decides whether it survives a restart", engineStateCount, len(declared))
	}
	for i, s := range declared {
		if s != EngineState(i) {
			t.Errorf("declared[%d] is EngineState(%d); the block was reordered, which reinterprets "+
				"every EngineSnapshot.State already on disk", i, s)
		}
	}
}
