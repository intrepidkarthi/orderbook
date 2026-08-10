package wal

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// A sharded venue crash-recovers every book. Until ShardsConfig.NewLog existed
// there was no way to give a shard a command log at all, so this test could not
// have been written and a multi-symbol venue could not survive a restart —
// docs/MULTI-SYMBOL.md §3.3.
//
// The assertion is per symbol and deliberately not venue-wide: shards recover to
// different points in wall-clock time, and that is correct rather than tolerated,
// because no invariant spans two books (§4.4). What must hold is that each symbol
// comes back byte-identical to an uninterrupted engine fed the same commands.

func shardSymbols() []string { return []string{"AAA", "BBB", "CCC"} }

// driveShardTape applies a deterministic per-symbol tape to whatever venue is
// handed to it, so the replicated arm and the uninterrupted control run the same
// commands.
func driveShardTape(t *testing.T, place func(*types.Order), n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		for s, sym := range shardSymbols() {
			side := types.SideBuy
			price := int64(100 - i%7)
			if (i+s)%2 == 0 {
				side = types.SideSell
				price = int64(100 + i%5)
			}
			o, err := types.NewOrder("mm", sym, side, types.OrderTypeLimit, price, int64(1+i%4), types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			o.ClientOrderID = fmt.Sprintf("%s-%d", sym, i)
			place(o)
		}
	}
}

func shardCfg(symbol string) matching.Config {
	c := matching.DefaultConfig(symbol)
	c.DedupClientOrderIDs = 256
	return c
}

// TestShardedVenueRecoversEveryBook is deliverable #3.
func TestShardedVenueRecoversEveryBook(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "venue.json")
	walPath := func(sym string) string { return filepath.Join(dir, sym+".wal") }
	snapPath := func(sym string) string { return filepath.Join(dir, sym+".snap") }

	man := matching.NewManifest(manifestPath)
	writers := map[string]*Writer{}
	sh := matching.NewShards(matching.ShardsConfig{
		NewConfig: shardCfg,
		Manifest:  man,
		NewLog: func(sym string) (matching.CommandLog, error) {
			w, err := Open(walPath(sym))
			if err != nil {
				return nil, err
			}
			writers[sym] = w
			return w, nil
		},
	})

	const n = 60
	driveShardTape(t, func(o *types.Order) { sh.Process(o) }, n)
	if err := sh.Err(); err != nil {
		t.Fatalf("shard creation failed: %v", err)
	}

	// Checkpoint one symbol only. The others recover from their logs alone, so a
	// single run covers both halves of the recovery path — and proves the shards
	// are genuinely independent, since a snapshot for AAA cannot help BBB.
	want := map[string]string{}
	for _, sym := range shardSymbols() {
		r := sh.Runner(sym)
		if sym == "AAA" {
			snap, err := r.Checkpoint()
			if err != nil {
				t.Fatalf("Checkpoint(%s): %v", sym, err)
			}
			if err := WriteSnapshot(snapPath(sym), snap); err != nil {
				t.Fatalf("WriteSnapshot(%s): %v", sym, err)
			}
		}
		live, err := r.Checkpoint()
		if err != nil {
			t.Fatalf("Checkpoint(%s): %v", sym, err)
		}
		want[sym] = live.Digest()
	}

	sh.Close()
	for sym, w := range writers {
		if err := w.Close(); err != nil {
			t.Fatalf("wal Close(%s): %v", sym, err)
		}
	}

	// Restart: reload the manifest from disk — the indices must come back — and
	// recover each shard from its own files.
	reloaded, err := matching.LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	for _, sym := range shardSymbols() {
		idx, err := reloaded.IndexFor(sym)
		if err != nil {
			t.Fatalf("IndexFor(%s): %v", sym, err)
		}
		cfg := shardCfg(sym)
		cfg.ShardIndex = idx

		eng, err := Recover(cfg, snapPath(sym), walPath(sym))
		if err != nil {
			t.Fatalf("Recover(%s): %v", sym, err)
		}
		if got := eng.TakeSnapshot().Digest(); got != want[sym] {
			t.Errorf("%s recovered to a different book\n got %s\nwant %s", sym, got, want[sym])
		}
	}
}

// TestShardedRecoveryReproducesPartitionedIDs is the property a shared counter
// would have destroyed. Each shard replays its OWN log, alone, with no knowledge
// of how its traffic interleaved with the others' — and must mint the same ids it
// minted live. That only works because the shard field is config and the counter
// is per shard.
func TestShardedRecoveryReproducesPartitionedIDs(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "venue.json")
	walPath := func(sym string) string { return filepath.Join(dir, sym+".wal") }

	man := matching.NewManifest(manifestPath)
	writers := map[string]*Writer{}
	sh := matching.NewShards(matching.ShardsConfig{
		NewConfig: shardCfg,
		Manifest:  man,
		NewLog: func(sym string) (matching.CommandLog, error) {
			w, err := Open(walPath(sym))
			if err != nil {
				return nil, err
			}
			writers[sym] = w
			return w, nil
		},
	})

	// Interleave the symbols so a shared counter would have produced ids that
	// depend on the interleaving.
	liveIDs := map[string][]int64{}
	const n = 25
	for i := 1; i <= n; i++ {
		for _, sym := range shardSymbols() {
			o, err := types.NewOrder("mm", sym, types.SideBuy, types.OrderTypeLimit, 100, 1, types.TIFGoodTillCancel)
			if err != nil {
				t.Fatalf("NewOrder: %v", err)
			}
			res := sh.Process(o)
			liveIDs[sym] = append(liveIDs[sym], res.Order.ID)
		}
	}
	sh.Close()
	for _, w := range writers {
		w.Close()
	}

	reloaded, err := matching.LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	for _, sym := range shardSymbols() {
		idx, err := reloaded.IndexFor(sym)
		if err != nil {
			t.Fatalf("IndexFor(%s): %v", sym, err)
		}
		cfg := shardCfg(sym)
		cfg.ShardIndex = idx

		eng, err := Recover(cfg, filepath.Join(dir, "absent.snap"), walPath(sym))
		if err != nil {
			t.Fatalf("Recover(%s): %v", sym, err)
		}
		var got []int64
		for _, o := range eng.TakeSnapshot().Orders {
			got = append(got, o.ID)
		}
		if len(got) != len(liveIDs[sym]) {
			t.Fatalf("%s: recovered %d orders, want %d", sym, len(got), len(liveIDs[sym]))
		}
		for i, id := range liveIDs[sym] {
			if got[i] != id {
				t.Errorf("%s order %d: recovered id %d, live id %d", sym, i, got[i], id)
			}
			if shard, _ := matching.SplitID(id); shard != idx {
				t.Errorf("%s id %d carries shard %d, want %d", sym, id, shard, idx)
			}
		}
	}
}
