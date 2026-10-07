package wal

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// docs/WAL-SYNC.md §4.

func syncOrder(t *testing.T, i int) *types.Order {
	t.Helper()
	o, err := types.NewOrder("u", "BTC-USD", types.SideBuy, types.OrderTypeLimit, int64(100+i%50), 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatal(err)
	}
	o.ID = int64(i + 1)
	return o
}

// slowFsync makes every hooked fsync take d, and reports when one starts.
func slowFsync(w *Writer, d time.Duration) <-chan struct{} {
	started := make(chan struct{}, 64)
	w.fsyncHook = func(f *os.File) error {
		select { // never block an fsync on a reader that is not listening
		case started <- struct{}{}:
		default:
		}
		time.Sleep(d)
		return f.Sync()
	}
	return started
}

// TestAppendDoesNotWaitForAnInFlightFsync is the stall stage attribution found: an
// append made while a group commit is in its fsync must not wait for the disk.
func TestAppendDoesNotWaitForAnInFlightFsync(t *testing.T) {
	const d = 150 * time.Millisecond
	w, err := Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.AppendSubmit(syncOrder(t, 0)); err != nil {
		t.Fatal(err)
	}
	started := slowFsync(w, d)
	done := make(chan error, 1)
	go func() { done <- w.Sync() }()
	<-started

	at := time.Now()
	if _, err := w.AppendSubmit(syncOrder(t, 1)); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(at); took > d/3 {
		t.Fatalf("an append during a %v fsync took %v: it waited for the disk", d, took)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestSyncCoversEverythingAppendedBeforeIt: after Sync returns, another reader sees
// every record appended before it was called; records appended during it are there
// after the next Sync.
func TestSyncCoversEverythingAppendedBeforeIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 100; i++ {
		if _, err := w.AppendSubmit(syncOrder(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	started := slowFsync(w, 50*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- w.Sync() }()
	<-started
	for i := 100; i < 150; i++ {
		if _, err := w.AppendSubmit(syncOrder(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	es, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) < 100 {
		t.Fatalf("after Sync, %d records readable; the 100 appended before it must be", len(es))
	}
	w.fsyncHook = nil
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if es, err = ReadAll(path); err != nil || len(es) != 150 {
		t.Fatalf("after the next Sync: %d records, %v", len(es), err)
	}
	for i, e := range es {
		if e.Seq != int64(i+1) {
			t.Fatalf("record %d has seq %d", i, e.Seq)
		}
	}
}

// TestRotationDuringAnFsync: tiny segments force rotations while two goroutines
// sync in tight loops, each holding a file outside the lock. Nothing latches, the
// appends finish in bounded time (a rotation that only waited, without holding new
// syncs off, could lose the lock to the next one indefinitely), and every record
// comes back in order.
func TestRotationDuringAnFsync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	w, err := OpenWith(path, Options{MaxSegmentBytes: 2048, MinSegments: 1})
	if err != nil {
		t.Fatal(err)
	}
	slowFsync(w, 3*time.Millisecond)
	stop := make(chan struct{})
	var syncs sync.WaitGroup
	errs := make(chan error, 2)
	for g := 0; g < 2; g++ {
		syncs.Add(1)
		go func() {
			defer syncs.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := w.Sync(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	const n = 400
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := w.AppendSubmit(syncOrder(t, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	took := time.Since(start)
	close(stop)
	syncs.Wait()
	select {
	case err := <-errs:
		t.Fatalf("a sync running across rotations failed: %v", err)
	default:
	}
	if w.Rotations() < 10 {
		t.Fatalf("%d rotations; the test needs many to test anything", w.Rotations())
	}
	if took > 20*time.Second {
		t.Fatalf("%d appends across %d rotations took %v: rotations are starving behind syncs", n, w.Rotations(), took)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	es, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != n {
		t.Fatalf("%d records after %d appends across %d rotations", len(es), n, w.Rotations())
	}
	for i, e := range es {
		if e.Seq != int64(i+1) {
			t.Fatalf("record %d has seq %d", i, e.Seq)
		}
	}
	t.Logf("%d appends, %d rotations, %v", n, w.Rotations(), took)
}

// TestAFailedFsyncLatches: the hook fails once; Sync reports it, Failed holds it,
// and the next append refuses.
func TestAFailedFsyncLatches(t *testing.T) {
	w, err := Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.AppendSubmit(syncOrder(t, 0)); err != nil {
		t.Fatal(err)
	}
	disk := errors.New("simulated EIO")
	w.fsyncHook = func(*os.File) error { return disk }
	if err := w.Sync(); !errors.Is(err, disk) {
		t.Fatalf("Sync: %v, want the fsync's error", err)
	}
	if !errors.Is(w.Failed(), disk) {
		t.Fatalf("Failed: %v", w.Failed())
	}
	if _, err := w.AppendSubmit(syncOrder(t, 1)); err == nil {
		t.Fatal("an append after a failed fsync was accepted")
	}
}

// TestCloseWaitsForAnInFlightFsync, and a Sync after Close refuses.
func TestCloseWaitsForAnInFlightFsync(t *testing.T) {
	w, err := Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendSubmit(syncOrder(t, 0)); err != nil {
		t.Fatal(err)
	}
	started := slowFsync(w, 80*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- w.Sync() }()
	<-started
	w.fsyncHook = nil
	if err := w.Close(); err != nil {
		t.Fatalf("Close during an fsync: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the in-flight Sync failed because Close did not wait: %v", err)
	}
	if err := w.Sync(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Sync after Close: %v, want ErrClosed", err)
	}
}

// TestSyncsDoNotOverlap: two Syncs called together run their fsyncs one after the
// other. Overlapping, the first to finish would clear the in-flight mark while the
// second still held a file, and a rotation could close it under that fsync.
func TestSyncsDoNotOverlap(t *testing.T) {
	w, err := Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.AppendSubmit(syncOrder(t, 0)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	inFlight, most := 0, 0
	w.fsyncHook = func(f *os.File) error {
		mu.Lock()
		inFlight++
		if inFlight > most {
			most = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return f.Sync()
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Sync(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d fsyncs ran at once", most)
	}
}
