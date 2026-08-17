package wal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Retention: the mechanism that actually bounds the read.
//
// Rotation on its own fixes nothing — a log split into 350 files a day is the same
// number of bytes. What rotation buys is that the unit you can delete without
// leaving half a file behind is a whole file, and the unit you can archive is a
// whole file. Deleting a prefix of the set under a predicate that cannot outrun the
// snapshot is what turns restart cost from O(total history) into O(retained log),
// which is a number the operator chose.
//
// Everything here runs off the matching goroutine, on the checkpoint cadence, and
// nothing here ever opens a segment: base(S) comes from the name, and
// last(Sᵢ) = base(S₍ᵢ₊₁₎) − 1 follows from the listing. That is a direct consequence
// of putting the base in the filename, and it is why the predicate is cheap enough
// to evaluate every cycle.

// RetentionResult is what one retention cycle did.
type RetentionResult struct {
	// Deleted and Archived name the segments, oldest first.
	Deleted  []string
	Archived []string
	// Floor is the base sequence of the oldest segment still on disk afterwards.
	Floor int64
	// RetainedBytes is what the set weighs afterwards.
	RetainedBytes int64
	// Skipped explains a cycle that deleted nothing when it might have. Empty when
	// there was nothing to do.
	Skipped string
}

// Retain deletes the oldest sealed segments of the set at stem, oldest first, under
// the predicate in docs/LOG-ROTATION.md §5.1. It is a no-op unless opts.RetainBytes
// is set: deletion is off by default and stays off until an operator asks for it.
//
// The predicate, and why each term is there:
//
//	(a) sealed(S) — never the active segment. Not only because it is being written,
//	    but because Open derives the next sequence from the newest segment. Delete it
//	    and the venue restarts its sequence space underneath a snapshot that already
//	    claims those numbers. At least one segment always remains.
//	(b) a snapshot on disk, READ BACK AND VERIFIED, with WALSeq >= last(S). Not
//	    >= base(S): partial coverage of a segment is no coverage, because the records
//	    above the snapshot's sequence are unrecoverable the moment the file is gone.
//	    Not os.Stat either: a snapshot that exists and fails its CRC is refused by
//	    ReadSnapshot and recovery falls back to the log, so gating on existence rather
//	    than verifiability deletes the fallback for a snapshot that cannot be used.
//	(c) at least MinSegments sealed segments newer than S. Not a correctness term —
//	    (b) is — but an operational one: recent history for the forensics RUNBOOKS
//	    assumes, and a reconnecting follower's catch-up kept out of the bootstrap path.
//	(d) a prefix, never a hole. A missing middle segment is the shape startup
//	    validation exists to catch and it must never be self-inflicted.
//	(f) archival before deletion, when configured.
//
// Term (e) — the log-shipping consumer — is satisfied by making the reader
// restartable rather than by making retention wait: a catch-up read opens every
// segment it selected before reading any of them, an already-open fd survives an
// unlink on POSIX, and ENOENT on open means "re-enumerate, and bootstrap if the
// floor has moved past me". No lease, no refcount, no lock.
//
// The ordering that matters is that the snapshot is durable before the first unlink,
// which WriteSnapshot already gives (temp file, fsync, rename, directory fsync) and
// which this reads back afterwards. A crash between the snapshot and the deletion
// leaves everything and the next cycle re-runs. A crash between two deletions leaves
// a prefix of length k for some k less than intended, which is a VALID set — that is
// the entire reason deletion is oldest-first, and it is what makes one directory
// fsync at the end sufficient.
func Retain(stem, snapPath string, opts Options) (RetentionResult, error) {
	opts = opts.withDefaults()
	var res RetentionResult
	if stem == "" || opts.RetainBytes <= 0 {
		return res, nil
	}
	set, err := enumerateSetByName(stem)
	if err != nil {
		return res, err
	}
	res.Floor, res.RetainedBytes = set.floor(), set.bytes()
	// Before anything is deleted, and every cycle rather than once, because a
	// directory can be replaced by a symlink under a running venue.
	if err := CheckArchiveDir(opts.ArchiveDir, set.dir); err != nil {
		return res, err
	}
	if len(set.segs) < 2 {
		return res, nil
	}

	snap, err := ReadSnapshot(snapPath)
	if err != nil {
		// A snapshot that does not verify is one recovery will refuse, so the log is
		// the only base the venue has left. Deleting any of it now would delete the
		// fallback.
		res.Skipped = fmt.Sprintf("snapshot %s does not verify (%v), so nothing was deleted this cycle", snapPath, err)
		return res, nil
	}
	if snap == nil {
		res.Skipped = fmt.Sprintf("no snapshot at %s, so nothing was deleted this cycle", snapPath)
		return res, nil
	}

	// Sealed segments are everything but the newest. last(Sᵢ) is base(S₍ᵢ₊₁₎) − 1,
	// straight off the names.
	sealed := len(set.segs) - 1
	bytes := set.bytes()
	for i := 0; i < sealed; i++ {
		seg := set.segs[i]
		last := set.segs[i+1].base - 1
		newerSealed := sealed - i - 1
		if bytes <= opts.RetainBytes {
			break // within budget: the rest stays
		}
		if snap.WALSeq < last {
			res.Skipped = fmt.Sprintf("%s ends at sequence %d and the snapshot only covers %d",
				seg.name, last, snap.WALSeq)
			break
		}
		if newerSealed < opts.MinSegments {
			res.Skipped = fmt.Sprintf("%s is within the %d-segment floor", seg.name, opts.MinSegments)
			break
		}
		if opts.ArchiveDir != "" {
			if err := archiveSegment(seg, opts.ArchiveDir); err != nil {
				res.Skipped = fmt.Sprintf("archiving %s failed (%v), so it was not deleted", seg.name, err)
				break
			}
			res.Archived = append(res.Archived, seg.name)
		}
		if err := reclaim(seg, stem); err != nil {
			res.Skipped = fmt.Sprintf("deleting %s failed: %v", seg.name, err)
			break
		}
		res.Deleted = append(res.Deleted, seg.name)
		bytes -= seg.size
		res.Floor = set.segs[i+1].base
	}
	if len(res.Deleted) > 0 {
		// One fsync, after the last unlink. A crash part-way through leaves a shorter
		// prefix, which is a valid set.
		_ = syncDir(set.dir)
	}
	res.RetainedBytes = bytes
	return res, nil
}

// reclaim removes a segment's records from the set.
//
// For a numbered segment that is an unlink. For the STEM — which can still be a
// pre-rotation file holding records 1..k, sealed where it stands by a torn tail — it
// is an atomic replacement by the 18-byte marker instead, because the one state a
// downgrade cannot survive is a set whose stem does not exist: an older build finds
// no file at -wal, concludes there is no log, and starts an empty venue beside
// everything. Unlinking it and writing the marker afterwards would leave exactly that
// state in the window between; the rename inside writeMarker does not.
func reclaim(seg segment, stem string) error {
	if seg.path == stem {
		return writeMarker(stem)
	}
	if err := os.Remove(seg.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ErrArchiveIsTheLog reports an ArchiveDir that resolves to the directory holding
// the segment set. It is refused rather than tolerated because the failure is silent
// and total.
//
// Archiving into the set's own directory makes the copy's target the segment itself.
// archiveSegment's idempotency check — a file already at the target with a matching
// size, which is how a cycle that crashed between the copy and the unlink is made
// safe to repeat — then sees the LIVE segment, agrees it is already archived, and
// returns success. Retention deletes it immediately afterwards. Every segment is
// reported archived and every one is destroyed: the log line reads
// "archived N", the archive is empty, and there is no error anywhere.
//
// It is easy to reach on purpose. Retention without archival has a recovery point of
// "the newest snapshot", so every doc here tells an operator to set -wal-archive the
// moment they set -wal-retain, and the log directory is the first path that comes to
// hand.
var ErrArchiveIsTheLog = errors.New("wal: archive directory is the log's own directory")

// CheckArchiveDir refuses an archive target that is the segment set's own directory,
// where setDir is the directory holding the set. An empty dir — archival off — is
// always fine.
//
// Retain calls it every cycle before it deletes anything, so a misconfigured archive
// stops retention rather than emptying it. It is exported so a caller can also refuse
// the configuration at startup, which is a better moment to learn than the first
// checkpoint tick under disk pressure.
//
// Both string comparison and os.SameFile, because neither is sufficient alone: the
// strings catch a target that does not exist yet, and the inode comparison catches
// the same directory reached by a symlink, a bind mount, a relative path or a
// trailing slash. It creates nothing and reports nothing else — a target that cannot
// be created, or a log directory that is not there at all, is somebody else's error to
// return, and this must not turn either into a different one.
func CheckArchiveDir(dir, setDir string) error {
	if dir == "" {
		return nil
	}
	same := func() error {
		return fmt.Errorf("%w: %s holds the segment set, so archiving into it would delete every segment "+
			"it claims to have archived. Point the archive at a directory outside the log's own, ideally on "+
			"another filesystem", ErrArchiveIsTheLog, dir)
	}
	clean := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			return filepath.Clean(abs)
		}
		return filepath.Clean(p)
	}
	if clean(dir) == clean(setDir) {
		return same()
	}
	// Beyond here both have to exist to be compared. Either one absent is answer
	// enough: two directories cannot be the same one when only a single directory is
	// there, and the string comparison above has already ruled out the case where the
	// names agree.
	a, err := os.Stat(dir)
	if err != nil {
		return nil //nolint:nilerr // a missing or unreadable archive dir is archiveSegment's to report
	}
	b, err := os.Stat(setDir)
	if err != nil {
		return nil //nolint:nilerr // a missing log directory fails at Open, with a better message than this
	}
	if os.SameFile(a, b) {
		return same()
	}
	return nil
}

// archiveSegment copies a segment into dir under the same name, durably, and
// byte-identically: nothing is compressed, encrypted or re-checksummed, so an
// archived segment is the segment.
//
// The copy is written as <name>.partial and renamed, the same shape WriteSnapshot
// uses, because the one state that is not benign is a crash INSIDE the copy leaving
// a truncated file under a legitimate name. An archive reader ignores anything not
// matching the 16-digit pattern — the same rule as the live set, restated because an
// archive directory is exactly where a second, looser enumerator gets written by
// someone in a hurry.
func archiveSegment(seg segment, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, seg.name)
	if st, err := os.Stat(target); err == nil && st.Size() == seg.size {
		return nil // idempotent by name: an earlier cycle crashed between copy and unlink
	}
	tmp := target + ".partial"
	src, err := os.Open(seg.path)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		return fail(err)
	}
	if err := dst.Sync(); err != nil {
		return fail(err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// enumerateSetByName is the enumeration retention uses: names and sizes only, no
// file opened, no header read.
//
// It is a separate function from enumerateSet on purpose. Retention runs every
// checkpoint interval and must not pay 350 opens to learn something the directory
// already says; recovery must pay them, because the header is the AUTHORITY and the
// name is only an index. Neither should be tempted to use the other's version.
func enumerateSetByName(stem string) (*segmentSet, error) {
	set := &segmentSet{stem: stem, dir: filepath.Dir(stem)}
	if stem == "" {
		return set, nil
	}
	stemName := filepath.Base(stem)
	ents, err := os.ReadDir(set.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return set, err
	}
	for _, ent := range ents {
		base, ok := segBaseFromName(stemName, ent.Name())
		if !ok || !ent.Type().IsRegular() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			return set, err
		}
		set.present = true
		set.segs = append(set.segs, segment{
			path: filepath.Join(set.dir, ent.Name()), name: ent.Name(),
			base: base, named: true, kind: kindDeclared, size: info.Size(),
		})
	}
	// A log that has never rotated is one file at the stem, and it is the active
	// segment, so retention has nothing to consider. It is included so floor() and
	// bytes() answer for the whole set rather than for the numbered part of it.
	if st, err := os.Lstat(stem); err == nil && st.Mode().IsRegular() && st.Size() > int64(SegHeaderBytes) {
		set.present = true
		set.segs = append(set.segs, segment{
			path: stem, name: stemName, base: 1, kind: kindHeadered, size: st.Size(),
		})
	}
	sort.Slice(set.segs, func(i, j int) bool { return set.segs[i].base < set.segs[j].base })
	return set, nil
}

// SetInfo describes a segment set without reading any of it: what an operator gets
// from ls, and what a venue exports as a metric.
type SetInfo struct {
	Segments int
	Bytes    int64
	// Floor is the base sequence of the oldest segment on disk. Above 1 means
	// retention has fired, which is the fact that decides whether "delete the
	// snapshot and replay from the beginning" is still a procedure that works.
	Floor int64
	Names []string
}

// Stat describes the set at stem from its directory entries alone.
func Stat(stem string) (SetInfo, error) {
	set, err := enumerateSetByName(stem)
	if err != nil {
		return SetInfo{}, err
	}
	info := SetInfo{Segments: len(set.segs), Bytes: set.bytes(), Floor: set.floor()}
	for _, s := range set.segs {
		info.Names = append(info.Names, s.name)
	}
	return info, nil
}

// ArchivedSegments lists the archive directory with the same allow-list the live set
// uses, so a half-written .partial copy is never mistaken for a segment.
func ArchivedSegments(dir, stem string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	stemName := filepath.Base(stem)
	var out []string
	for _, ent := range ents {
		if strings.HasSuffix(ent.Name(), ".partial") {
			continue
		}
		if _, ok := segBaseFromName(stemName, ent.Name()); ok {
			out = append(out, ent.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ErrBelowFloor reports a reader asking for sequences the retained set no longer
// holds. A log-shipping primary answers it with a snapshot rather than with the
// records it happens to still have: shipping a set that starts above the follower's
// position is exactly the gap the follower would refuse, arriving from the one
// source that is supposed to be authoritative.
var ErrBelowFloor = errors.New("wal: below the retention floor")

// ReadAfter returns the retained records with Seq greater than afterSeq, and reports
// ErrBelowFloor when the set no longer holds the record immediately after it.
//
// It is the primitive docs/BOUNDED-RECOVERY.md §6 declined to export for slice 1 and
// which retention makes necessary rather than merely convenient: a catch-up reader
// that filtered ReadAll itself would ship a partial catch-up starting at some
// segment's base, and the consumer's gap check would kill it with "gap in the feed"
// every time the primary rotated.
//
// Every selected segment is opened before any is read, so a segment unlinked by a
// retention pass running in the same process mid-read is survived rather than raced:
// on POSIX an already-open fd outlives the unlink.
func ReadAfter(stem string, afterSeq int64) ([]Entry, error) {
	var out []Entry
	err := retryWhileVanishing(func() error {
		out = nil
		set, err := enumerateSet(stem)
		if err != nil {
			return err
		}
		if len(set.segs) == 0 {
			return nil
		}
		if floor := set.floor(); afterSeq+1 < floor {
			return fmt.Errorf("%w: sequence %d is below the oldest retained segment %s, which starts at %d",
				ErrBelowFloor, afterSeq+1, set.segs[0].name, floor)
		}
		walk, err := walkSetIn(set, afterSeq)
		if err != nil {
			return err
		}
		if walk.suspect {
			// The same rule recovery follows: once the declared bases and the records
			// disagree, nothing selected by sequence can be trusted, so read it all.
			if walk, err = walkSetIn(set, 0); err != nil {
				return err
			}
			out = walk.entries[:0]
			for _, e := range walk.entries {
				if e.Seq > afterSeq {
					out = append(out, e)
				}
			}
			return nil
		}
		out = walk.entries
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
