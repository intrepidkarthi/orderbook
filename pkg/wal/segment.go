package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The segment set: what a WAL path names after rotation.
//
// A log used to be one file that grew forever. It is now a SET — a stem path and
// its numbered siblings — because the unit you can delete without leaving half a
// file behind is a whole file, and the unit you can archive is a whole file.
// Rotation is what makes retention safe; retention is what bounds the read.
//
//	/var/lib/obgw/BTC-USD.wal                   the set marker, 18 bytes, no records
//	/var/lib/obgw/BTC-USD.wal.0000000000000001  segment, base sequence 1
//	/var/lib/obgw/BTC-USD.wal.0000000000610422  segment, base sequence 610,422  <- active
//
// The suffix is the segment's BASE SEQUENCE — the Seq of its first record — in
// decimal, zero-padded to 16 digits so that lexical order is numeric order. That
// buys two things the design turned out to need. Enumeration and ordering cost one
// directory read and no opens. And last(Sᵢ) = base(S₍ᵢ₊₁₎) − 1 falls out of the
// listing, which is why retention never has to open a segment to decide whether it
// may delete one.
//
// See docs/LOG-ROTATION.md for the reasoning, and §2.3 in particular for the trap
// this file exists to avoid.

const (
	// SegMagic identifies a segment written before matching semantics were stamped,
	// and the SET MARKER at the stem, which is still written in this form. A
	// segment's RECORDS are byte-identical to what an OBWAL\x01 file would hold at
	// the same point in the stream — only the file's first 18 bytes are new.
	//
	// It is READ and no longer written for record-bearing segments; SegMagicV3 is
	// what Writer produces. Both names are frozen exported surface
	// (docs/COMPATIBILITY.md: renaming anything exported is breaking), so the pair
	// ends up mildly awkward and that is the cost of the promise.
	SegMagic = "OBWAL\x02"

	// SegHeaderBytes is the whole of a SegMagic segment header: SegMagic, the base
	// sequence as a big-endian int64, and a CRC-32C over those eight bytes. It is
	// also the whole of the set marker, which keeps this shape forever — see
	// writeMarker.
	//
	// The CRC covers the base and nothing else, and four bytes is cheap for what it
	// buys: an undetected bit flip in a base sequence shifts a whole segment's
	// sequence space, which either double-applies or silently drops a run of
	// commands. It is the one number the entire design rests on, so it checks itself.
	SegHeaderBytes = len(SegMagic) + 8 + 4

	// SegMagicV3 identifies a segment that DECLARES THE MATCHING SEMANTICS its
	// records were produced under. It is what this build writes.
	//
	// The magic is bumped rather than the header extended in place, and the reason is
	// which failure an operator meets on a downgrade. A build that knows only
	// SegMagic would read the four extra bytes at offset 18 as a record length
	// prefix: a semantics of 1 is 00 00 00 01, a declared length of 1, which passes
	// the MaxRecordBytes bound and fails its CRC — so the old build says "wal:
	// corrupt record" and sends an operator to check SMART data for what is a
	// version mismatch. With the magic bumped it takes the path
	// docs/LOG-ROTATION.md §2.5 already built: six bytes that are not OBWAL\x01 are
	// treated as a headerless v1 log, "OBWA" is read as a length prefix of
	// 1,329,747,777, and it refuses with the exact message the runbook already
	// explains means a version mismatch and not a disk.
	SegMagicV3 = "OBWAL\x03"

	// SegHeaderBytesV3 is the whole of a SegMagicV3 header: the magic, the base
	// sequence as a big-endian int64, the semantics version as a big-endian uint32,
	// and a CRC-32C over the TWELVE bytes of base-and-semantics together.
	//
	// The CRC covers the semantics for the same reason it covers the base, and the
	// direction that matters is the quiet one: an undetected bit flip that turns some
	// other build's number into this build's lets a mismatched log through the gate
	// silently. It is the number the gate rests on, so it checks itself, and it costs
	// nothing to fold into a CRC that was already there.
	SegHeaderBytesV3 = len(SegMagicV3) + 8 + 4 + 4

	// segDigits is the width of the zero-padded decimal suffix. Sixteen digits is
	// 10^16 records, about 126,000 years at 2,500 messages/s; a base that needs a
	// seventeenth is refused at rotation rather than silently changing the shape of
	// the name.
	segDigits = 16

	// markerBase is the base a set marker declares. Zero is reserved and means "not a
	// segment": no record ever carries sequence 0, because Writer.append
	// pre-increments.
	markerBase = int64(0)
)

// ErrLogGap reports a set whose sequences are not contiguous, or a snapshot that
// sits below the oldest sequence any retained segment holds.
//
// It is deliberately distinct from ErrCorrupt. Corruption is bytes that changed;
// this is files that are missing — a segment deleted by hand, a retention pass that
// outran its snapshot, a partial restore from a backup. The recovered book would be
// plausible and wrong, with every remaining record verifying perfectly, so the only
// safe answer is to refuse and name the two sequences involved.
var ErrLogGap = errors.New("wal: log gap")

// ErrNotALog reports a path that is not a log and cannot become one — most often a
// directory.
//
// Before segments, a directory passed to Recover opened, failed its first read, and
// returned a fresh engine with a nil error: an embedder that called Recover without
// a following Open started an EMPTY VENUE and said nothing. That is fixed here
// because the enumerator has to stat the stem anyway.
var ErrNotALog = errors.New("wal: not a log")

// segKind is how a member of the set declares its framing.
type segKind uint8

const (
	// kindDeclared is a segment written before semantics were stamped: an 18-byte
	// header declaring its base, then checksummed records. It declares no semantics,
	// which is not the same as declaring zero — see segment.semantics.
	kindDeclared segKind = iota
	// kindDeclaredV3 is a segment written by this build: a 22-byte header declaring
	// its base AND the matching semantics its records were produced under, then
	// checksummed records byte-identical to every earlier format's.
	kindDeclaredV3
	// kindHeadered is an OBWAL\x01 file — the single-file format that preceded
	// rotation. Implicit base 1, checksummed records.
	kindHeadered
	// kindLegacy is a v1 headerless file, written before checksums existed. Implicit
	// base 1, no checksums, so "it decodes" is its only integrity signal.
	kindLegacy
	// kindMarker is the 18-byte set marker at the stem. It holds no records.
	kindMarker
)

// segment is one member of a set, as learned from its name and its header. Nothing
// here requires reading a record.
type segment struct {
	path string // full path
	name string // base name, which is what an error message shows an operator
	base int64  // the Seq of its first record
	kind segKind
	// semantics is the matching.SemanticsVersion this segment's records were
	// produced under, and ZERO means UNKNOWN rather than "version zero". Only a
	// kindDeclaredV3 segment can carry a non-zero one; every earlier shape declares
	// nothing, and nothing is not compatible with anything. See
	// docs/SEMANTICS-VERSION.md §4.
	semantics int
	// named reports that the base came from the file's 16-digit suffix as well as
	// from its header. A legacy stem carries no suffix and declares base 1 by being
	// the only file there is.
	named bool
	size  int64
}

func (s segment) checksummed() bool { return s.kind != kindLegacy }

// headerBytes is how many bytes precede the first record.
func (s segment) headerBytes() int64 {
	switch s.kind {
	case kindDeclared, kindMarker:
		return int64(SegHeaderBytes)
	case kindDeclaredV3:
		return int64(SegHeaderBytesV3)
	case kindHeadered:
		return int64(len(Magic))
	default:
		return 0
	}
}

// segPath returns the name a segment based at base must have.
func segPath(stem string, base int64) string {
	return fmt.Sprintf("%s.%0*d", stem, segDigits, base)
}

// segBaseFromName extracts a base from a set member's name, or reports that the
// name is not a member.
//
// Enumeration is an ALLOW-LIST, never a glob, and the strictness is load-bearing
// rather than fastidious. `BTC-USD.snap` lives in the same directory, WriteSnapshot
// drops `BTC-USD.snap.tmp` there while it works, cmd/obgw writes `venue.json`
// beside it, and examples/multisymbol derives its snapshot path as walPath+".snap"
// — so a <stem>.* glob would hand a snapshot to a frame parser and report the
// venue's log as corrupt. Exactly 16 ASCII digits excludes all four by
// construction, and excludes tomorrow's .gz, .bak and .2026-08-16 too.
func segBaseFromName(stemName, name string) (int64, bool) {
	if len(name) != len(stemName)+1+segDigits {
		return 0, false
	}
	if !strings.HasPrefix(name, stemName+".") {
		return 0, false
	}
	digits := name[len(stemName)+1:]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	base, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return base, true
}

// segHeader renders the 18 bytes that open a SegMagic segment. It is what
// writeMarker still writes; record-bearing segments take segHeaderV3.
func segHeader(base int64) []byte {
	b := make([]byte, 0, SegHeaderBytes)
	b = append(b, SegMagic...)
	b = binary.BigEndian.AppendUint64(b, uint64(base))
	return binary.BigEndian.AppendUint32(b, crc32.Checksum(b[len(SegMagic):], crcTable))
}

// segHeaderV3 renders the 22 bytes that open a segment written by this build: the
// magic, the base, the semantics the records were produced under, and one CRC over
// the twelve bytes of the two numbers together.
//
// sem is passed in rather than read from matching.SemanticsVersion here so a test
// can construct a segment declaring somebody else's semantics without byte surgery.
// The production caller is materialiseSegment, and it passes the constant.
func segHeaderV3(base int64, sem int) []byte {
	b := make([]byte, 0, SegHeaderBytesV3)
	b = append(b, SegMagicV3...)
	b = binary.BigEndian.AppendUint64(b, uint64(base))
	b = binary.BigEndian.AppendUint32(b, uint32(sem))
	return binary.BigEndian.AppendUint32(b, crc32.Checksum(b[len(SegMagicV3):], crcTable))
}

// readSegHeader classifies an open file by its first bytes and, for a declared
// segment, returns the base and the semantics it declares.
//
// A file whose first six bytes are a segment magic and whose remaining header bytes
// are missing is corrupt rather than "some other format": the rotation protocol in
// docs/LOG-ROTATION.md §3.2 materialises a segment by writing and fsyncing a
// complete header into a temp file and only then linking it into place, so
// "segment exists, header does not" is a state no crash can produce. Finding one
// means something else wrote it.
//
// A returned semantics of zero means the file declares none. It is NOT version zero
// and it is not equal to any version, including another file that also declares
// none — see the gate in Recover and docs/SEMANTICS-VERSION.md §4.
func readSegHeader(f *os.File) (kind segKind, base int64, sem int, err error) {
	var buf [SegHeaderBytesV3]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, 0, 0, err
	}
	name := filepath.Base(f.Name())
	// The two declared shapes share everything but their length and the extra field,
	// so the base check, the CRC and the marker rule are written once.
	checkDeclared := func(magic string, hdrLen int) (int64, int, error) {
		if n < hdrLen {
			return 0, 0, fmt.Errorf("%w: %s declares a segment header and holds only %d of its %d bytes",
				ErrCorrupt, name, n, hdrLen)
		}
		covered := buf[len(magic) : hdrLen-4]
		want := binary.BigEndian.Uint32(buf[hdrLen-4 : hdrLen])
		if got := crc32.Checksum(covered, crcTable); got != want {
			return 0, 0, fmt.Errorf("%w: %s segment header checksum %08x, want %08x — the base sequence cannot be trusted",
				ErrCorrupt, name, got, want)
		}
		b := int64(binary.BigEndian.Uint64(buf[len(magic) : len(magic)+8]))
		var s int
		if len(covered) > 8 {
			s = int(binary.BigEndian.Uint32(buf[len(magic)+8 : len(magic)+12]))
		}
		return b, s, nil
	}
	switch {
	case n >= len(SegMagicV3) && string(buf[:len(SegMagicV3)]) == SegMagicV3:
		base, sem, err := checkDeclared(SegMagicV3, SegHeaderBytesV3)
		if err != nil {
			return 0, 0, 0, err
		}
		// A V3 header never carries the marker base: the marker keeps the SegMagic
		// shape forever (docs/SEMANTICS-VERSION.md Rule 7), so base 0 here is a file
		// something else wrote.
		if base < 1 {
			return 0, 0, 0, fmt.Errorf("%w: %s declares base sequence %d", ErrCorrupt, name, base)
		}
		return kindDeclaredV3, base, sem, nil
	case n >= len(SegMagic) && string(buf[:len(SegMagic)]) == SegMagic:
		base, _, err := checkDeclared(SegMagic, SegHeaderBytes)
		if err != nil {
			return 0, 0, 0, err
		}
		if base == markerBase {
			return kindMarker, 0, 0, nil
		}
		if base < 0 {
			return 0, 0, 0, fmt.Errorf("%w: %s declares base sequence %d", ErrCorrupt, name, base)
		}
		return kindDeclared, base, 0, nil
	case n >= len(Magic) && string(buf[:len(Magic)]) == Magic:
		return kindHeadered, 1, 0, nil
	case n == 0:
		// An empty file. Treated as the current format, exactly as hasHeader did:
		// there is nothing there to be a v1 log.
		return kindHeadered, 1, 0, nil
	default:
		return kindLegacy, 1, 0, nil
	}
}

// classify opens one candidate member of the set and reads its header.
func classify(path, name string, named bool, nameBase int64) (segment, error) {
	s := segment{path: path, name: name, named: named}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The directory listing is a moment old and retention deleted this one in
			// between. The reader restarts; see retryWhileVanishing.
			return s, errSegmentVanished
		}
		return s, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return s, err
	}
	if st.IsDir() {
		return s, fmt.Errorf("%w: %s is a directory, not a log", ErrNotALog, path)
	}
	if !st.Mode().IsRegular() {
		return s, fmt.Errorf("%w: %s is not a regular file", ErrNotALog, path)
	}
	s.size = st.Size()
	kind, base, sem, err := readSegHeader(f)
	if err != nil {
		return s, err
	}
	s.kind, s.base, s.semantics = kind, base, sem

	// The name and the header are written by one process in one operation, so they
	// can only disagree if the file was renamed, copied, restored from a backup, or
	// written by something else. The reader cannot tell which of the two is the lie
	// and both choices are bad — trusting the name puts records into the wrong
	// sequence space, trusting the header puts the set in the wrong order — so it
	// refuses and names both numbers.
	if named {
		switch kind {
		case kindDeclared, kindDeclaredV3:
			if base != nameBase {
				return s, fmt.Errorf("%w: %s declares base sequence %d in its header and %d in its name — "+
					"the file was renamed, copied or restored, and neither number can be trusted over the other",
					ErrCorrupt, name, base, nameBase)
			}
		case kindMarker:
			return s, fmt.Errorf("%w: %s carries a set marker (base 0) under a segment name declaring base %d",
				ErrCorrupt, name, nameBase)
		default:
			// A legacy artifact carrying a segment name. It has no header to declare a
			// base, so its implicit base is 1 and the name must say so.
			if nameBase != 1 {
				return s, fmt.Errorf("%w: %s has no segment header, so its base is 1, but its name declares %d",
					ErrCorrupt, name, nameBase)
			}
			s.base = 1
		}
	}
	return s, nil
}

// segmentSet is a stem and its members, in base order.
type segmentSet struct {
	stem string
	dir  string
	// segs are the record-bearing members, ascending by base. The last is the
	// ACTIVE segment: the one Open appends to and the one retention never deletes.
	segs []segment
	// marker reports an 18-byte set marker at the stem.
	marker bool
	// present distinguishes an empty set from an absent one. A snapshot ahead of a
	// log that does not exist yet is not a condition worth reporting.
	present bool
}

func (s *segmentSet) empty() bool { return len(s.segs) == 0 }

// floor is the oldest sequence any retained segment can hold.
func (s *segmentSet) floor() int64 {
	if len(s.segs) == 0 {
		return 0
	}
	return s.segs[0].base
}

// newest is the active segment.
func (s *segmentSet) newest() (segment, bool) {
	if len(s.segs) == 0 {
		return segment{}, false
	}
	return s.segs[len(s.segs)-1], true
}

// bytes is the size of the whole retained set, which is what -wal-retain budgets
// and what a restart has to read.
func (s *segmentSet) bytes() int64 {
	var n int64
	for _, seg := range s.segs {
		n += seg.size
	}
	return n
}

// enumerateSet reads a directory once and returns the set at stem, in base order,
// with every member's header verified against its name.
//
// A missing stem and no siblings is not an error: it is a venue that has never
// written a log, and it recovers to the snapshot alone exactly as it always did.
func enumerateSet(stem string) (*segmentSet, error) {
	set := &segmentSet{stem: stem}
	if stem == "" {
		return set, nil
	}
	set.dir = filepath.Dir(stem)
	stemName := filepath.Base(stem)

	// The stem itself. Three shapes: a marker (this build owns the path and has
	// rotated), a legacy or unrotated single file (a segment with implicit base 1),
	// or nothing.
	if st, err := os.Lstat(stem); err == nil {
		switch {
		case st.IsDir():
			return set, fmt.Errorf("%w: %s is a directory, not a log", ErrNotALog, stem)
		case !st.Mode().IsRegular():
			return set, fmt.Errorf("%w: %s is not a regular file", ErrNotALog, stem)
		}
		seg, err := classify(stem, stemName, false, 0)
		if err != nil {
			return set, err
		}
		set.present = true
		if seg.kind == kindMarker {
			set.marker = true
		} else {
			set.segs = append(set.segs, seg)
		}
	} else if !os.IsNotExist(err) {
		return set, err
	}

	ents, err := os.ReadDir(set.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return set, err
	}
	for _, ent := range ents {
		nameBase, ok := segBaseFromName(stemName, ent.Name())
		if !ok {
			continue
		}
		seg, err := classify(filepath.Join(set.dir, ent.Name()), ent.Name(), true, nameBase)
		if err != nil {
			return set, err
		}
		set.present = true
		set.segs = append(set.segs, seg)
	}
	sort.Slice(set.segs, func(i, j int) bool { return set.segs[i].base < set.segs[j].base })

	if err := set.validateStructure(); err != nil {
		return set, err
	}
	return set, nil
}

// validateStructure checks what a directory listing plus headers can decide, which
// is everything except contiguity across a segment's records.
//
// It runs BEFORE any record is walked, so a structural problem is reported as a
// structural problem rather than surfacing as a corrupt record four hundred
// thousand records later.
func (s *segmentSet) validateStructure() error {
	for i := 1; i < len(s.segs); i++ {
		prev, cur := s.segs[i-1], s.segs[i]
		if prev.base == cur.base {
			return fmt.Errorf("%w: %s and %s both declare base sequence %d — two files claim the same commands",
				ErrCorrupt, prev.name, cur.name, cur.base)
		}
	}
	return nil
}

// validateAgainstSnapshot is the check every retention bug trips, and it is
// specified before the retention predicate for exactly that reason.
//
// The snapshot is the base; the retained log is the tail. If the oldest retained
// sequence is more than one past the snapshot's WALSeq, the commands in between
// exist in no file this venue can read, and recovery would produce a book that
// skipped them — plausible, wrong, and with every remaining record verifying
// perfectly. Refusing here converts "retention deleted something it should not
// have" from a silently wrong book into an outage with two numbers in it.
func (s *segmentSet) validateAgainstSnapshot(snapSeq int64, haveSnap bool) error {
	floor := s.floor()
	if floor <= 1 {
		return nil
	}
	if !haveSnap {
		return fmt.Errorf("%w: there is no snapshot and the oldest retained segment %s starts at sequence %d — "+
			"sequences 1..%d are in no file this venue can read. "+
			"See docs/RUNBOOKS.md \"A gap between the snapshot and the log\".",
			ErrLogGap, s.segs[0].name, floor, floor-1)
	}
	if snapSeq+1 < floor {
		return fmt.Errorf("%w: snapshot covers through sequence %d but the oldest retained segment %s starts at %d — "+
			"sequences %d..%d are in no file this venue can read. "+
			"See docs/RUNBOOKS.md \"A gap between the snapshot and the log\".",
			ErrLogGap, snapSeq, s.segs[0].name, floor, snapSeq+1, floor-1)
	}
	return nil
}

// contiguity is checked between segments as the walk crosses from one to the next,
// because base(S₍ᵢ₊₁₎) has to be compared against last(Sᵢ) and last(Sᵢ) is only
// known once Sᵢ's records have been read.
//
// A torn tail is legal in the newest segment, and legal in any segment immediately
// followed by one whose base is exactly last+1. That is not a special case for the
// sealed-torn segments Open creates on purpose (docs/LOG-ROTATION.md §3.4, Rule 8);
// it is one contiguity test that those segments happen to satisfy, and the same
// test catches a segment truncated by a filesystem, a partial copy or an operator's
// head -c.
func contiguityError(prev segment, prevLast int64, next segment) error {
	switch {
	case next.base > prevLast+1:
		return fmt.Errorf("%w: %s ends at sequence %d and %s starts at %d — sequences %d..%d are in no file this venue can read",
			ErrLogGap, prev.name, prevLast, next.name, next.base, prevLast+1, next.base-1)
	case next.base <= prevLast:
		return fmt.Errorf("%w: %s ends at sequence %d and %s starts at %d — two segments claim the same commands, "+
			"which is the shape two concurrent writers produce",
			ErrCorrupt, prev.name, prevLast, next.name, next.base)
	}
	return nil
}
