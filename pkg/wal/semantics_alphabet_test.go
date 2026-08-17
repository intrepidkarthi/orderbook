package wal

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/semcheck"
)

// TestTheFingerprintCorpusReachesEveryEntryKind is the wal half of the alphabet guard,
// and it lives here because entryKindCount lives here.
//
// Rule 22 of docs/SEMANTICS-VERSION.md makes internal/semcheck's corpus load-bearing:
// a bump has to be justified by a change to the fingerprint, so a behaviour on a path
// the corpus never reaches is a behaviour nobody can bump for. EntryKind is the list of
// commands that change the book — it is what the journal records, one arm per verb —
// so "the corpus reaches every EntryKind" is the strongest mechanical statement
// available about the corpus's breadth.
//
// The direction of the import is what makes it work. internal/semcheck does not import
// pkg/wal (it drives matching's public API and nothing else), so this test can import
// semcheck without a cycle, and the sentinel stays unexported on both sides.
//
// The map below is the one hand-written thing, and it is written so that FORGETTING is
// the failing case: a new EntryKind with no entry here fails, and an entry naming a
// command the corpus never issues fails too.
func TestTheFingerprintCorpusReachesEveryEntryKind(t *testing.T) {
	// The corpus's vocabulary, by the stable name semcheck.Kind.Name returns. It is
	// the same vocabulary as the block above it, which is not a coincidence: the
	// journal records exactly the commands that change the book.
	byKind := map[EntryKind]string{
		KindSubmit:     "SUBMIT",
		KindCancel:     "CANCEL",
		KindReduce:     "REDUCE",
		KindCancelAll:  "CANCEL_ALL",
		KindStop:       "STOP",
		KindOCO:        "OCO",
		KindIceberg:    "ICEBERG",
		KindPegged:     "PEGGED",
		KindTrailing:   "TRAILING",
		KindReplace:    "REPLACE",
		KindHalt:       "HALT",
		KindResume:     "RESUME",
		KindCancelOnly: "CANCEL_ONLY",
		KindSetMark:    "SET_MARK",
		KindBust:       "BUST",
		KindSetPhase:   "SET_PHASE",
	}

	_, cov, err := semcheck.Render()
	if err != nil {
		t.Fatalf("semcheck.Render: %v", err)
	}

	for k := KindSubmit; k < entryKindCount; k++ {
		name, ok := byKind[k]
		if !ok {
			t.Errorf("EntryKind %d has no entry in this map, so nothing says whether internal/semcheck's "+
				"corpus reaches it. A journalled command the fingerprint never issues is a behaviour that "+
				"cannot be bumped for: add the kind to internal/semcheck and then to the map above.", uint8(k))
			continue
		}
		if cov.Commands[name] == 0 {
			t.Errorf("internal/semcheck's corpus never issued %s (EntryKind %d). A change to what that command "+
				"does would move no line of testdata/semantics.txt, so matching.SemanticsVersion would not have "+
				"to move either. See docs/SEMANTICS-VERSION.md §5.5.", name, uint8(k))
		}
	}
}
