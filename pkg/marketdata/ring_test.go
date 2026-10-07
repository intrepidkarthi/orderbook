package marketdata

import (
	"errors"
	"testing"
)

// TestRingKeepsTheRetainedWindow publishes three times the ring's capacity and, at
// every step, checks Since and Retained against a model that keeps everything: the
// last `retain` updates are served, anything older is ErrSequenceEvicted, and an
// up-to-date subscriber gets nothing (docs/STAGES.md §7).
func TestRingKeepsTheRetainedWindow(t *testing.T) {
	const retain = 7
	f := NewFeed("INC", retain)
	var all []uint64 // sequence of every update published, in order
	for i := 1; i <= 3*retain+2; i++ {
		f.PublishIndicative(int64(i), int64(i), 0)
		all = append(all, uint64(i))

		oldestWant := uint64(1)
		if len(all) > retain {
			oldestWant = all[len(all)-retain]
		}
		oldest, n := f.Retained()
		if oldest != oldestWant || n != min(len(all), retain) {
			t.Fatalf("after %d: Retained = (%d, %d), want (%d, %d)", i, oldest, n, oldestWant, min(len(all), retain))
		}
		for from := uint64(0); from <= uint64(i); from++ {
			got, err := f.Since(from)
			if from+1 < oldestWant {
				if !errors.Is(err, ErrSequenceEvicted) {
					t.Fatalf("after %d: Since(%d) = %v, %v; want evicted", i, from, got, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("after %d: Since(%d): %v", i, from, err)
			}
			if len(got) != i-int(from) {
				t.Fatalf("after %d: Since(%d) gave %d updates, want %d", i, from, len(got), i-int(from))
			}
			for k, u := range got {
				if u.Seq != from+1+uint64(k) || u.IndicativePrice != int64(u.Seq) {
					t.Fatalf("after %d: Since(%d)[%d] = seq %d price %d", i, from, k, u.Seq, u.IndicativePrice)
				}
			}
		}
	}
}

// TestPublishIntoAFullRingIsConstantTime: publishing into a full ring of 65,536 must
// cost about what publishing into a full ring of 1,024 does. Shifting the ring on
// every eviction made the large one about 64 times slower.
func TestPublishIntoAFullRingIsConstantTime(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmarks two ring sizes")
	}
	per := func(retain int) float64 {
		r := testing.Benchmark(func(b *testing.B) {
			f := NewFeed("INC", retain)
			for i := 0; i < retain; i++ {
				f.PublishIndicative(1, 1, 0)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f.PublishIndicative(1, 1, 0)
			}
		})
		return float64(r.NsPerOp())
	}
	small, large := per(1<<10), per(1<<16)
	if large > 3*small {
		t.Fatalf("publish into a full ring: %.0f ns at 1,024, %.0f ns at 65,536; eviction is not constant time", small, large)
	}
	t.Logf("publish into a full ring: %.0f ns at 1,024, %.0f ns at 65,536", small, large)
}
