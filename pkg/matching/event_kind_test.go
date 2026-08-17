package matching

import "testing"

// TestEventKindNamesAreTotalOverTheBlock is what makes EventKind ENUMERABLE from
// outside this package.
//
// eventKindCount is unexported, deliberately — a sentinel that reached a wire, a file
// or the frozen surface would be a value somebody could send. So internal/semcheck's
// coverage guard cannot loop to it, and instead walks String() until it answers
// "UNKNOWN". That loop is only as good as String being a faithful enumerator of the
// block, which is exactly what this asserts: every declared kind has a name, and the
// first undeclared value does not.
//
// Without it the guard would be a hand-written list wearing a loop. With it, a kind
// added to the block and not to String fails HERE, and a kind added to both is picked
// up by the guard and fails THERE until the corpus reaches it. Either way, declaring a
// kind and doing nothing else fails the build, which is the property
// docs/SEMANTICS-VERSION.md §5.5 asks for.
func TestEventKindNamesAreTotalOverTheBlock(t *testing.T) {
	for k := EventKind(0); k < eventKindCount; k++ {
		if k.String() == "UNKNOWN" {
			t.Errorf("EventKind(%d) is declared and String() does not name it — internal/semcheck enumerates "+
				"this block through String(), so an unnamed kind is a kind the coverage guard cannot see", uint8(k))
		}
	}
	if got := eventKindCount.String(); got != "UNKNOWN" {
		t.Errorf("EventKind(%d) is one past the block and String() answers %q; the enumeration would run off "+
			"the end", uint8(eventKindCount), got)
	}
}

// TestEventKindCountIsLast pins the sentinel's position. It is the entryKindCount
// treatment (pkg/wal/wal.go): a kind appended AFTER the sentinel is a kind every
// enumeration silently skips, and nothing else in the build would notice.
func TestEventKindCountIsLast(t *testing.T) {
	// EventBusted is the last declared kind. If a new one is added it belongs before
	// eventKindCount, and this line is where that is stated.
	if eventKindCount != EventBusted+1 {
		t.Errorf("eventKindCount is %d and the last declared kind is EventBusted at %d — a kind declared after "+
			"the sentinel is invisible to every guard that enumerates the block",
			uint8(eventKindCount), uint8(EventBusted))
	}
}
