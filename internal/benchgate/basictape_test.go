package benchgate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/intrepidkarthi/orderbook/internal/tape"
)

// The cross-engine tape (docs/BENCH-GATE.md §17): limit GTC submits and cancels only,
// so any engine can replay it and be held to the same digest. Nothing in the gate
// reads it.
const (
	basicTapeFile   = "testdata/bench-basic-v1.obt"
	basicDigestFile = "testdata/bench-basic-v1.digest"
	// basicTapeSHA256 pins the file, under bench-v1's rule: never edited, replaced
	// by a new version.
	basicTapeSHA256 = "d375ca3356f7d50d95c21512ab8bca9fe3a276cd28fa585a0361171ee1f67993"
)

func basicHeader() Header {
	return Header{
		Generator:  generatorLine,
		Provenance: fmt.Sprintf("profile=%s seed=%#x n=%d", tape.Basic.Name, uint64(benchSeed), benchN),
		MaxOrders:  benchCapacity,
	}
}

// TestWriteBasicTape writes the tape when asked to and refuses to overwrite it.
func TestWriteBasicTape(t *testing.T) {
	if os.Getenv("BENCHGATE_WRITE_TAPE") != "1" {
		t.Skip("set BENCHGATE_WRITE_TAPE=1 to write " + basicTapeFile)
	}
	if _, err := os.Stat(basicTapeFile); err == nil {
		t.Fatalf("%s exists and is frozen; cut a new version instead of rewriting it", basicTapeFile)
	}
	b, err := Marshal(basicHeader(), tape.Gen(tape.Basic, benchSeed, benchN))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(basicTapeFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	t.Logf("wrote %s: %d bytes, sha256 %x", basicTapeFile, len(b), sum)
}

func TestBasicTapeFileIsFrozen(t *testing.T) {
	b, _ := readTapeFile(t, basicTapeFile)
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != basicTapeSHA256 {
		t.Fatalf("%s hashes to %s, pinned %s. A frozen tape is never edited; cut bench-basic-v2.obt instead.",
			basicTapeFile, got, basicTapeSHA256)
	}
}

func TestBasicTapeMatchesGenerator(t *testing.T) {
	b, tp := readTapeFile(t, basicTapeFile)
	if tp.Header != basicHeader() {
		t.Fatalf("header %+v, want %+v", tp.Header, basicHeader())
	}
	again, err := Marshal(tp.Header, tape.Gen(tape.Basic, benchSeed, benchN))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(b) {
		t.Fatalf("generator moved: tape.Gen(basic, %#x, %d) no longer reproduces %s; "+
			"leave v1 frozen and cut bench-basic-v2.obt", uint64(benchSeed), benchN, basicTapeFile)
	}
}

// TestBasicTapeIsTheCommonSubset reads the file, not the generator, and requires the
// property the tape is for: plain limit GTC submits and cancels, every cancel that
// names an order sent from that order's account.
func TestBasicTapeIsTheCommonSubset(t *testing.T) {
	_, tp := readTapeFile(t, basicTapeFile)
	for _, c := range tp.Cmds {
		switch c.Kind {
		case tape.Submit:
			if c.MarketOrd || c.PostOnly || c.TIF != 0 || c.STP != 0 || c.Privileged || c.TradeGroup != 0 {
				t.Fatalf("command %d is not a plain limit GTC: %+v", c.Pos, c)
			}
		case tape.Cancel:
			if tg := tp.Cmds[c.Target]; tg.Kind == tape.Submit && tg.User != c.User {
				t.Fatalf("command %d cancels %d from %s, which %s made", c.Pos, c.Target, c.User, tg.User)
			}
		default:
			t.Fatalf("command %d is a %v, outside the subset", c.Pos, c.Kind)
		}
	}
}

// TestBasicTapeShape holds the tape to §2.1's floors.
func TestBasicTapeShape(t *testing.T) {
	_, tp := readTapeFile(t, basicTapeFile)
	s := replayShape(t, tp, tp.MaxOrders)
	t.Logf("shape: %d commands, %d submits (%d rejected), %d cancels (%d reached a live order), "+
		"%d trades, %d resting at the end, peak %d", s.commands, s.submits, s.rejected,
		s.targeted, s.reachedLive, s.trades, s.restingAtEnd, s.peakResting)
	if s.bookFull != 0 {
		t.Errorf("%d commands refused for a full book at capacity %d", s.bookFull, tp.MaxOrders)
	}
	if s.rejected*100 > s.submits*5 {
		t.Errorf("%d of %d submits rejected, over the 5%% floor", s.rejected, s.submits)
	}
	if s.stp != [6]int{} {
		t.Errorf("self-trade prevention decided %v on a Portable tape", s.stp)
	}
	if s.reachedLive*10 < s.targeted {
		t.Errorf("only %d of %d cancels reached a live order, under 10%%", s.reachedLive, s.targeted)
	}
	if s.restingAtEnd < 1000 {
		t.Errorf("%d orders resting at the end, under 1,000", s.restingAtEnd)
	}
	if s.trades*4 < s.commands {
		t.Errorf("%d trades over %d commands, under one per four", s.trades, s.commands)
	}
}

func TestBasicTapeDigest(t *testing.T) {
	_, tp := readTapeFile(t, basicTapeFile)
	holdToDigest(t, tp, basicDigestFile, "bench-basic-v1.obt")
}

func TestBasicTapeRefmatchAgrees(t *testing.T) {
	_, tp := readTapeFile(t, basicTapeFile)
	refmatchAgrees(t, tp)
}
