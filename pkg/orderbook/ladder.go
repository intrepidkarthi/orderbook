package orderbook

// ladder is one side of the book: its price levels in one slice sorted so the best
// level is last, and a price -> level map beside it (docs/BOOK-LADDER.md §8.4).
//
// The two serve different reads. The best level, which every match step reads, is
// the slice's last element, with no map lookup, and emptying it is a truncation
// rather than a shift of every level behind it. A level deep in a wide book is found
// through the map, because a binary search over the slice lost to one hash of the
// price wherever access was at random depth (§6.5, §6.7).
//
// Both sides sort the same way by key: a bid's key is its price and an ask's is its
// negated price, so on either side a larger key is a better price.
type ladder struct {
	levels  []*PriceLevel
	byPrice map[int64]*PriceLevel
	sign    int64 // +1 for bids, -1 for asks
}

// probe is how many levels from the best a lookup compares before it asks the map.
// Most adds and removals land at or near the touch.
const probe = 4

func newLadder(sign int64, capacity int) ladder {
	return ladder{
		levels:  make([]*PriceLevel, 0, capacity),
		byPrice: make(map[int64]*PriceLevel, capacity),
		sign:    sign,
	}
}

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

// get is price's level, or nil: the top of the ladder first, then the map.
func (l *ladder) get(price int64) *PriceLevel {
	n := len(l.levels)
	for i := n - 1; i >= max(n-probe, 0); i-- {
		if l.levels[i].Price == price {
			return l.levels[i]
		}
	}
	return l.byPrice[price]
}

// index is where a level at price belongs in the slice, which holds no level at it:
// the top of the ladder first, then a binary search below it.
func (l *ladder) index(price int64) int {
	k := l.key(price)
	n := len(l.levels)
	stop := max(n-probe, 0)
	for i := n - 1; i >= stop; i-- {
		if l.key(l.levels[i].Price) < k {
			return i + 1
		}
	}
	lo, hi := 0, stop
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if l.key(l.levels[mid].Price) < k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// insert adds lvl, whose price has no level yet.
func (l *ladder) insert(lvl *PriceLevel) {
	i := l.index(lvl.Price)
	l.levels = append(l.levels, nil)
	copy(l.levels[i+1:], l.levels[i:])
	l.levels[i] = lvl
	l.byPrice[lvl.Price] = lvl
}

// remove takes lvl out. The best level, the common case, is a truncation.
func (l *ladder) remove(lvl *PriceLevel) {
	delete(l.byPrice, lvl.Price)
	n := len(l.levels)
	if n > 0 && l.levels[n-1] == lvl {
		l.levels[n-1] = nil
		l.levels = l.levels[:n-1]
		return
	}
	// Below the best, the level is where a new one at its price would go: index
	// finds the first position whose key is not smaller, which is lvl's own.
	if i := l.index(lvl.Price); i < n && l.levels[i] == lvl {
		copy(l.levels[i:], l.levels[i+1:])
		l.levels[n-1] = nil
		l.levels = l.levels[:n-1]
	}
}
