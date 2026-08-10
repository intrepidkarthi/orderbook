package matching

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// The venue manifest: which symbol owns which shard index.
//
// This file is the price of a venue-wide identifier (docs/MULTI-SYMBOL.md §4.1).
// Ids partition into a shard field and a per-shard counter, the counter comes back
// from the log, and the shard field comes back from here. Lose this mapping, or
// return a symbol under a different index, and every id that symbol ever issued
// becomes ambiguous — a worse outcome than losing a snapshot, which costs only a
// replay.
//
// So it is treated with the suspicion the snapshot had to learn: CRC-32C over the
// payload, an atomic write, and a refusal rather than a repair when the file does
// not check out. The snapshot went four releases with no integrity check at all
// because a small file written rarely feels safe; it is not, and this one is
// smaller and written more rarely.

var (
	// ErrManifestCorrupt reports a manifest whose checksum does not match. It is
	// never recoverable in place: restore the file, do not start the venue.
	ErrManifestCorrupt = errors.New("matching: venue manifest failed its checksum")
	// ErrManifestFull reports a venue that has run out of shard indices.
	ErrManifestFull = errors.New("matching: no shard index left to assign")
	// ErrSymbolMoved reports a symbol whose recorded index disagrees with the one
	// being asserted — the failure this file exists to make loud.
	ErrSymbolMoved = errors.New("matching: symbol already holds a different shard index")
)

const manifestMagic = "OBMANIFEST\x01"

// manifestFile is the on-disk form. Entries are sorted by symbol so the encoding
// is stable and two identical manifests produce identical bytes.
type manifestFile struct {
	Magic   string          `json:"magic"`
	Entries []ManifestEntry `json:"entries"`
	Next    int             `json:"next"` // the next index to hand out
	CRC     uint32          `json:"crc"`  // over the payload with CRC zeroed
	_       struct{}        // keep construction explicit
}

// ManifestEntry binds a symbol to its shard index, permanently.
type ManifestEntry struct {
	Symbol     string `json:"symbol"`
	ShardIndex int    `json:"shard_index"`
}

// Manifest is the venue's symbol-to-shard mapping. It is safe for concurrent use.
//
// Indices are assigned once and never reused, including for a delisted symbol: an
// index that comes back attached to a different instrument makes every id the
// previous holder issued ambiguous, which is the one failure this type exists to
// prevent. A venue that delists is expected to keep the row.
type Manifest struct {
	mu     sync.RWMutex
	path   string
	byName map[string]int
	next   int
}

// NewManifest creates an empty in-memory manifest bound to path. Nothing is
// written until a symbol is assigned.
func NewManifest(path string) *Manifest {
	return &Manifest{path: path, byName: map[string]int{}}
}

// LoadManifest reads a manifest from path. A missing file yields an empty
// manifest, which is how a new venue starts; a corrupt one is an error, because
// guessing at this mapping is how ids stop being unique.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewManifest(path), nil
		}
		return nil, err
	}
	var f manifestFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifestCorrupt, err)
	}
	if f.Magic != manifestMagic {
		return nil, fmt.Errorf("%w: bad magic %q", ErrManifestCorrupt, f.Magic)
	}
	want := f.CRC
	f.CRC = 0
	got, err := manifestCRC(&f)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("%w: crc %08x, want %08x", ErrManifestCorrupt, got, want)
	}

	m := NewManifest(path)
	for _, e := range f.Entries {
		if err := checkShardIndex(e.ShardIndex); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrManifestCorrupt, err)
		}
		if prev, dup := m.byName[e.Symbol]; dup {
			return nil, fmt.Errorf("%w: %s appears twice (%d and %d)",
				ErrManifestCorrupt, e.Symbol, prev, e.ShardIndex)
		}
		m.byName[e.Symbol] = e.ShardIndex
	}
	// An index used twice is the corruption that matters most, since it is the
	// exact state in which two symbols mint the same ids.
	seen := make(map[int]string, len(m.byName))
	for sym, idx := range m.byName {
		if other, dup := seen[idx]; dup {
			return nil, fmt.Errorf("%w: shard index %d claimed by both %s and %s",
				ErrManifestCorrupt, idx, other, sym)
		}
		seen[idx] = sym
	}
	m.next = f.Next
	return m, nil
}

// IndexFor returns the symbol's shard index, assigning and persisting a new one on
// first sight. It is the only way an index is created.
func (m *Manifest) IndexFor(symbol string) (int, error) {
	m.mu.RLock()
	idx, ok := m.byName[symbol]
	m.mu.RUnlock()
	if ok {
		return idx, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if idx, ok = m.byName[symbol]; ok { // re-check under the write lock
		return idx, nil
	}
	if m.next > MaxShardIndex {
		return 0, ErrManifestFull
	}
	idx = m.next
	m.byName[symbol] = idx
	m.next++
	if err := m.writeLocked(); err != nil {
		// Roll back in memory: an index that is not on disk has not been assigned,
		// and handing it out anyway is how a restart reissues it to somebody else.
		delete(m.byName, symbol)
		m.next--
		return 0, err
	}
	return idx, nil
}

// Assert records that symbol holds index, failing if it already holds another. Use
// it when the index comes from somewhere other than this manifest — a config file,
// an operator, another venue — so the disagreement surfaces at startup rather than
// as duplicate ids later.
func (m *Manifest) Assert(symbol string, index int) error {
	if err := checkShardIndex(index); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if have, ok := m.byName[symbol]; ok {
		if have != index {
			return fmt.Errorf("%w: %s holds %d, asserted %d", ErrSymbolMoved, symbol, have, index)
		}
		return nil
	}
	for sym, idx := range m.byName {
		if idx == index {
			return fmt.Errorf("%w: shard index %d is held by %s", ErrSymbolMoved, index, sym)
		}
	}
	m.byName[symbol] = index
	if index >= m.next {
		m.next = index + 1
	}
	if err := m.writeLocked(); err != nil {
		delete(m.byName, symbol)
		return err
	}
	return nil
}

// Symbols returns every symbol in the manifest with its index, sorted by symbol.
func (m *Manifest) Symbols() []ManifestEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entriesLocked()
}

// Len reports how many symbols the venue has ever assigned.
func (m *Manifest) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byName)
}

func (m *Manifest) entriesLocked() []ManifestEntry {
	out := make([]ManifestEntry, 0, len(m.byName))
	for sym, idx := range m.byName {
		out = append(out, ManifestEntry{Symbol: sym, ShardIndex: idx})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// writeLocked persists the manifest durably: a fully-synced temp file renamed over
// the target, then the parent directory synced — the same procedure WriteSnapshot
// uses, for the same reason. Callers hold m.mu.
func (m *Manifest) writeLocked() error {
	f := manifestFile{Magic: manifestMagic, Entries: m.entriesLocked(), Next: m.next}
	crc, err := manifestCRC(&f)
	if err != nil {
		return err
	}
	f.CRC = crc
	b, err := json.Marshal(&f)
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.path)
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, m.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// manifestCRC checksums the payload with CRC zeroed, over the Castagnoli
// polynomial the WAL and snapshot already use.
func manifestCRC(f *manifestFile) (uint32, error) {
	c := *f
	c.CRC = 0
	b, err := json.Marshal(&c)
	if err != nil {
		return 0, err
	}
	return crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)), nil
}
