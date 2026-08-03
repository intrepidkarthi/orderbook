// Command replication is the reference primary-backup topology from
// docs/REPLICATION.md — the consumer built to notice if "deterministic apply,
// an ordered command log, replay mode and snapshot bootstrap are all here" were
// wrong.
//
// One primary ships its command log over TCP (wal.SetOnAppend → a bounded
// buffer per follower — a follower that falls behind is cut, never waited on).
// One warm follower bootstraps from a snapshot taken mid-stream and then never
// stops replaying. Promotion writes the follower's book as a new venue's base
// snapshot, opens a fresh log, and hands the engine to a Runner. The digest
// (EngineSnapshot.Digest) is how either side proves the books agree.
//
// What "committed" means here: a command is committed when the primary's log
// has it; replication is asynchronous, and the loss window is the visible gap
// between the primary's LogSeq and the follower's Applied — measured, never
// assumed zero. The drills in main_test.go hold this example to the spec's
// pass criteria.
//
//	go run ./examples/replication
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func main() {
	dir, err := os.MkdirTemp("", "replication")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	lim := func(user string, side types.Side, price, qty int64) *types.Order {
		o, _ := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		return o
	}

	fmt.Println("1) A primary journals to its WAL and ships the log:")
	primary, err := NewPrimary("BTC-USD", filepath.Join(dir, "primary.wal"), "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	primary.Runner.Process(lim("mm", types.SideSell, 101, 5))
	primary.Runner.Process(lim("mm", types.SideBuy, 99, 5))
	fmt.Printf("   primary log at seq %d\n", primary.LogSeq())

	fmt.Println("\n2) A follower joins MID-STREAM — snapshot bootstrap, then the live tail:")
	follower, err := StartFollower("BTC-USD", primary.Addr())
	if err != nil {
		log.Fatal(err)
	}
	primary.Runner.Process(lim("taker", types.SideBuy, 101, 2)) // trades against the resting ask
	dead := lim("mm", types.SideSell, 102, 1)
	primary.Runner.Process(dead)
	primary.Runner.Cancel(dead.ID, "mm")
	if err := follower.WaitApplied(primary.LogSeq(), 5*time.Second); err != nil {
		log.Fatal(err)
	}

	pSnap, _ := primary.Runner.Checkpoint()
	fDigest, _ := follower.Digest()
	fmt.Printf("   lag        %d commands (primary %d, follower %d)\n",
		primary.LogSeq()-follower.Applied(), primary.LogSeq(), follower.Applied())
	fmt.Printf("   primary    %s\n   follower   %s\n", pSnap.Digest()[:16], fDigest[:16])
	if pSnap.Digest() != fDigest {
		log.Fatal("BOOKS DIVERGED — this line printing is the reason the digest exists")
	}
	fmt.Printf("   books agree; %d follower(s) shed so far\n", primary.Shed())

	fmt.Println("\n3) The primary dies. Promote the follower:")
	oldIncarnation := "INC-A"
	primary.Close()
	promoted, err := follower.Promote(filepath.Join(dir, "promoted.wal"), filepath.Join(dir, "promoted.snap"))
	if err != nil {
		log.Fatal(err)
	}
	defer promoted.Close()

	// The caller mints the new incarnation — that id is the client fence.
	registry := orderentry.NewRegistry("INC-B", 64)
	if _, err := registry.Resume(oldIncarnation, "client-1", 7); err != nil {
		fmt.Printf("   a client resuming with the dead primary's cursor (%s, seq 7) is refused: %v\n", oldIncarnation, err)
	}

	res := promoted.Process(lim("taker", types.SideBuy, 102, 1))
	fmt.Printf("   promoted venue is live: new order matched %d trade(s), journalling to its own log\n", len(res.Trades))
	fmt.Println("\nWhat this did not do: fence a partitioned old primary that still runs.")
	fmt.Println("That is split-brain prevention — consensus — and it is yours by design (docs/REPLICATION.md §5).")
}
