package wal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAppendLatchesAWriteFailure: a buffered write only fails when the flush inside
// it failed, so bytes went at the file and did not land — the exact state the
// `failed` field's own comment describes and the exact state it did not cover. The
// two frame writes in append returned the error bare: no latch, so the next append
// wrote a record into the middle of a torn one, and no rollback, so the sequence it
// had already handed out named a record that does not exist.
func TestAppendLatchesAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "log-wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	if _, err := w.AppendHalt(); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Take the fd out from under the buffer. Everything short of a flush still
	// succeeds; the append that fills the buffer is the one that discovers it.
	w.mu.Lock()
	fdErr := w.f.Close()
	w.mu.Unlock()
	if fdErr != nil {
		t.Fatalf("closing the segment: %v", fdErr)
	}

	good := w.Seq()
	var appendErr error
	for i := 0; i < 4000 && appendErr == nil; i++ {
		if _, appendErr = w.AppendCancel(int64(i), "u"); appendErr == nil {
			good = w.Seq()
		}
	}
	if appendErr == nil {
		t.Fatal("no append ever failed against a closed segment")
	}

	// The sequence names records that exist.
	if got := w.Seq(); got != good {
		t.Errorf("Seq() = %d after the failed append, want %d — the failure consumed a sequence", got, good)
	}

	// The failure is latched, and the writer is done.
	latched := w.Failed()
	if latched == nil {
		t.Fatal("Failed() is nil after a write that did not land")
	}
	if !errors.Is(latched, os.ErrClosed) {
		t.Errorf("latched error does not wrap the cause: %v", latched)
	}
	if _, err := w.AppendHalt(); !errors.Is(err, latched) {
		t.Errorf("a later append returned %v, want the latched %v", err, latched)
	}
	if got := w.Seq(); got != good {
		t.Errorf("Seq() = %d after a refused append, want %d", got, good)
	}
}
