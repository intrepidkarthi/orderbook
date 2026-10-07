package probe

import "testing"

// TestMemWalksOneCycle: Sattolo's shuffle yields a single cycle through every slot,
// so the walk touches all 64 MiB. A plain Fisher-Yates shuffle (j drawn from [0, i]
// instead of [0, i)) leaves several cycles, and the one the walk starts on covered
// about half the array when this was sabotaged: a probe measuring less memory than it
// says it does, and a shorter cycle could fit in a cache and time nothing at all.
func TestMemWalksOneCycle(t *testing.T) {
	Mem()
	seen := 0
	for p := walk[0]; ; p = walk[p] {
		seen++
		if p == 0 {
			break
		}
		if seen > len(walk) {
			t.Fatal("the walk never returns to its start")
		}
	}
	if seen != len(walk) {
		t.Fatalf("the walk's cycle has %d slots of %d: part of the array is never touched", seen, len(walk))
	}
}

func TestProbesTakeTime(t *testing.T) {
	if ALU() <= 0 || Mem() <= 0 {
		t.Fatal("a probe reported no time; the compiler may have deleted its work")
	}
}
