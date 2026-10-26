package orderbook

// ladder is one side of the book: its price levels in one slice, sorted so the best
// level is last (docs/BOOK-LADDER.md §5.1). The best level, which every match step
// reads, is then the last element, with no map between the price and its level, and
// emptying it is a truncation rather than a shift of every level behind it.
//
// Both sides sort the same way by key: a bid's key is its price and an ask's is its
// negated price, so on either side a larger key is a better price.
type ladder struct {
	levels []*PriceLevel
	sign   int64 // +1 for bids, -1 for asks
}

// probe is how many levels from the best a search compares one at a time before it
// falls back to binary search. Most adds and removals land at or near the touch.
const probe = 4

func (l *ladder) key(price int64) int64 { return price * l.sign }

func (l *ladder) len() int { return len(l.levels) }

// best is the best level, or nil on an empty side.
func (l *ladder) best() *PriceLevel {
	if n := len(l.levels); n > 0 {
		return l.levels[n-1]
	}
	return nil
}

// at is the i-th best level: at(0) is the best.
func (l *ladder) at(i int) *PriceLevel { return l.levels[len(l.levels)-1-i] }

// search returns where price's level is, or where it would be inserted to keep the
// order, and whether it is there.
func (l *ladder) search(price int64) (int, bool) {
	k := l.key(price)
	n := len(l.levels)
	stop := max(n-probe, 0)
	for i := n - 1; i >= stop; i-- {
		switch lk := l.key(l.levels[i].Price); {
		case lk == k:
			return i, true
		case lk < k:
			return i + 1, false
		}
	}
	// Every probed level is better than price, so it belongs below them: the first
	// index in [0, stop) whose key is not smaller.
	lo, hi := 0, stop
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if l.key(l.levels[mid].Price) < k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < stop && l.levels[lo].Price == price
}

// get is price's level, or nil.
func (l *ladder) get(price int64) *PriceLevel {
	if i, ok := l.search(price); ok {
		return l.levels[i]
	}
	return nil
}

// insertAt puts lvl at index i, which search returned for its price.
func (l *ladder) insertAt(i int, lvl *PriceLevel) {
	l.levels = append(l.levels, nil)
	copy(l.levels[i+1:], l.levels[i:])
	l.levels[i] = lvl
}

// remove takes lvl out. The best level, the common case, is a truncation.
func (l *ladder) remove(lvl *PriceLevel) {
	n := len(l.levels)
	if n > 0 && l.levels[n-1] == lvl {
		l.levels[n-1] = nil
		l.levels = l.levels[:n-1]
		return
	}
	if i, ok := l.search(lvl.Price); ok {
		copy(l.levels[i:], l.levels[i+1:])
		l.levels[n-1] = nil
		l.levels = l.levels[:n-1]
	}
}
