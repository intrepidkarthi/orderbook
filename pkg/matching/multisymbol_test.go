package matching

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Multi-symbol identity: docs/MULTI-SYMBOL.md deliverables #1 and #2.
//
// The property under test is narrow and load-bearing: an id names an order at a
// venue with many books, AND replaying one shard's log alone still reproduces that
// shard's ids. A design that buys the first by centralising the counter loses the
// second, which is why the id is partitioned rather than shared.

func msOrder(t *testing.T, sym, user string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, sym, side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	return o
}

func TestComposeSplitRoundTrip(t *testing.T) {
	cases := []struct {
		shard int
		seq   int64
	}{
		{0, 1}, {0, MaxShardSeq}, {1, 1}, {7, 42},
		{MaxShardIndex, 1}, {MaxShardIndex, MaxShardSeq},
	}
	for _, c := range cases {
		id := ComposeID(c.shard, c.seq)
		if id <= 0 {
			t.Errorf("ComposeID(%d,%d) = %d — ids must stay positive", c.shard, c.seq, id)
		}
		shard, seq := SplitID(id)
		if shard != c.shard || seq != c.seq {
			t.Errorf("SplitID(ComposeID(%d,%d)) = (%d,%d)", c.shard, c.seq, shard, seq)
		}
	}
	// Shard 0 leaves ids exactly as they were, which is what keeps every existing
	// single-symbol deployment, snapshot and golden vector unchanged.
	if got := ComposeID(0, 12345); got != 12345 {
		t.Errorf("ComposeID(0, 12345) = %d, want 12345", got)
	}
}

// TestIDsAreUniqueAcrossSymbols is the gap PRODUCTION-READINESS named. Before the
// partition both shards issued order 1 and trade 1.
func TestIDsAreUniqueAcrossSymbols(t *testing.T) {
	man := NewManifest(filepath.Join(t.TempDir(), "venue.json"))
	sh := NewShards(ShardsConfig{Manifest: man})
	defer sh.Close()

	seen := map[int64]string{}
	for _, sym := range []string{"AAA", "BBB", "CCC"} {
		sh.Process(msOrder(t, sym, "mm", types.SideSell, 100, 5))
		res := sh.Process(msOrder(t, sym, "t", types.SideBuy, 100, 5))
		if res == nil || len(res.Trades) != 1 {
			t.Fatalf("%s: setup did not trade", sym)
		}
		orderID, tradeID := res.Order.ID, res.Trades[0].ID
		for _, id := range []int64{orderID, tradeID} {
			if prev, dup := seen[id]; dup {
				t.Errorf("id %d issued by both %s and %s", id, prev, sym)
			}
			seen[id] = sym
		}
	}
}

// TestShardsRefuseASecondSymbolWithoutAManifest — the alternative is minting
// colliding ids silently, a failure whose only symptom is two orders that cannot
// be told apart, long afterwards.
func TestShardsRefuseASecondSymbolWithoutAManifest(t *testing.T) {
	sh := NewShards(ShardsConfig{})
	defer sh.Close()

	if res := sh.Process(msOrder(t, "AAA", "mm", types.SideSell, 100, 5)); res == nil || res.Status == types.OrderStatusRejected {
		t.Fatal("the first symbol should work without a manifest")
	}
	res := sh.Process(msOrder(t, "BBB", "mm", types.SideSell, 100, 5))
	if res == nil || res.Status != types.OrderStatusRejected {
		t.Fatal("a second symbol was accepted with no manifest, so two shards now mint the same ids")
	}
	if !errors.Is(sh.Err(), ErrNoManifest) {
		t.Errorf("Err() = %v, want ErrNoManifest", sh.Err())
	}
}

// TestRunnerForRoutesByOrderID — the arithmetic the partition buys.
func TestRunnerForRoutesByOrderID(t *testing.T) {
	man := NewManifest(filepath.Join(t.TempDir(), "venue.json"))
	sh := NewShards(ShardsConfig{Manifest: man})
	defer sh.Close()

	a := sh.Process(msOrder(t, "AAA", "mm", types.SideSell, 100, 5))
	b := sh.Process(msOrder(t, "BBB", "mm", types.SideSell, 200, 5))

	if got := sh.RunnerFor(a.Order.ID); got != sh.Runner("AAA") {
		t.Error("an AAA order id did not route to the AAA shard")
	}
	if got := sh.RunnerFor(b.Order.ID); got != sh.Runner("BBB") {
		t.Error("a BBB order id did not route to the BBB shard")
	}
	if got := sh.RunnerFor(ComposeID(MaxShardIndex, 1)); got != nil {
		t.Error("an id from a shard this venue never opened routed somewhere")
	}
}

// TestBustRefusesAnotherSymbolsTrade — the sharpest consequence of partitioning
// trade ids. Unpartitioned, "bust trade 1" would annul whichever local print
// happened to share the number.
func TestBustRefusesAnotherSymbolsTrade(t *testing.T) {
	man := NewManifest(filepath.Join(t.TempDir(), "venue.json"))
	sh := NewShards(ShardsConfig{Manifest: man})
	defer sh.Close()

	trades := map[string]int64{}
	for _, sym := range []string{"AAA", "BBB"} {
		sh.Process(msOrder(t, sym, "mm", types.SideSell, 100, 5))
		res := sh.Process(msOrder(t, sym, "t", types.SideBuy, 100, 5))
		trades[sym] = res.Trades[0].ID
	}

	// BBB's own print busts; AAA's does not, even though the two differ only in
	// the shard field.
	if err := sh.Runner("BBB").Bust(trades["BBB"], "erroneous"); err != nil {
		t.Fatalf("busting BBB's own trade: %v", err)
	}
	if err := sh.Runner("BBB").Bust(trades["AAA"], "wrong symbol"); !errors.Is(err, ErrUnknownTrade) {
		t.Errorf("BBB busted AAA's trade: err = %v, want ErrUnknownTrade", err)
	}
}

// --- the manifest ---

func TestManifestAssignsStableIndices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "venue.json")
	m := NewManifest(path)

	want := map[string]int{}
	for _, sym := range []string{"AAA", "BBB", "CCC"} {
		idx, err := m.IndexFor(sym)
		if err != nil {
			t.Fatalf("IndexFor(%s): %v", sym, err)
		}
		want[sym] = idx
	}
	// Repeat calls are stable within the process.
	for sym, idx := range want {
		got, err := m.IndexFor(sym)
		if err != nil || got != idx {
			t.Errorf("IndexFor(%s) = %d,%v — want %d", sym, got, err, idx)
		}
	}
	// And across a restart, which is the whole point: an index that moves makes
	// every id its symbol ever issued ambiguous.
	reloaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	for sym, idx := range want {
		got, err := reloaded.IndexFor(sym)
		if err != nil || got != idx {
			t.Errorf("after reload IndexFor(%s) = %d,%v — want %d", sym, got, err, idx)
		}
	}
	// A new symbol after reload does not reuse an index already handed out.
	fresh, err := reloaded.IndexFor("DDD")
	if err != nil {
		t.Fatalf("IndexFor(DDD): %v", err)
	}
	for sym, idx := range want {
		if fresh == idx {
			t.Errorf("DDD got index %d, already held by %s", fresh, sym)
		}
	}
}

func TestManifestRefusesCorruption(t *testing.T) {
	dir := t.TempDir()

	t.Run("bad crc", func(t *testing.T) {
		path := filepath.Join(dir, "crc.json")
		m := NewManifest(path)
		if _, err := m.IndexFor("AAA"); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Change a symbol's index and leave the CRC alone: the corruption that
		// parses perfectly and silently restores a mapping that never existed,
		// which is the only kind the checksum is there for.
		//
		// The first draft flipped the first '0' byte in the file. That byte lives
		// inside the magic's \u0001 escape, so the test passed by detecting a bad
		// magic while the CRC check was disabled — green for a reason it was not
		// testing. Caught by running it against that exact sabotage.
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("unmarshal manifest: %v", err)
		}
		entries := raw["entries"].([]any)
		entries[0].(map[string]any)["shard_index"] = float64(7)
		corrupt, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, corrupt, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadManifest(path); !errors.Is(err, ErrManifestCorrupt) {
			t.Errorf("LoadManifest on a silently-altered mapping = %v, want ErrManifestCorrupt", err)
		}
	})

	t.Run("missing file is a new venue", func(t *testing.T) {
		m, err := LoadManifest(filepath.Join(dir, "absent.json"))
		if err != nil {
			t.Fatalf("a missing manifest should start an empty venue: %v", err)
		}
		if m.Len() != 0 {
			t.Errorf("fresh manifest has %d entries", m.Len())
		}
	})
}

// TestManifestAssertCatchesAMovedSymbol — the failure the file exists to prevent,
// surfaced at startup rather than as duplicate ids weeks later.
func TestManifestAssertCatchesAMovedSymbol(t *testing.T) {
	m := NewManifest(filepath.Join(t.TempDir(), "venue.json"))
	if err := m.Assert("AAA", 3); err != nil {
		t.Fatalf("first Assert: %v", err)
	}
	if err := m.Assert("AAA", 3); err != nil {
		t.Errorf("re-asserting the same index should be fine: %v", err)
	}
	if err := m.Assert("AAA", 4); !errors.Is(err, ErrSymbolMoved) {
		t.Errorf("moving AAA to another index = %v, want ErrSymbolMoved", err)
	}
	if err := m.Assert("BBB", 3); !errors.Is(err, ErrSymbolMoved) {
		t.Errorf("handing AAA's index to BBB = %v, want ErrSymbolMoved", err)
	}
}

func TestManifestRejectsOutOfRangeIndex(t *testing.T) {
	m := NewManifest(filepath.Join(t.TempDir(), "venue.json"))
	for _, bad := range []int{-1, MaxShardIndex + 1} {
		if err := m.Assert("AAA", bad); err == nil {
			t.Errorf("Assert(AAA, %d) was accepted", bad)
		}
	}
}
