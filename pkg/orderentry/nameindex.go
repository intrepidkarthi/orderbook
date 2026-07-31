package orderentry

import (
	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

// NameIndex records, synchronously with the book, that a newly accepted order can
// be addressed by its client's own identifier.
//
// # Why this is separate from the Publisher
//
// The Publisher's contract is that the matching goroutine never waits for a client:
// it copies each event batch into a bounded queue and returns, and a pump goroutine
// does the encoding and the fan-out whenever it can. That is right, and it is the
// reason a slow consumer cannot stop the venue.
//
// But two different things were riding on that queue. One is the conversation with
// the client — execution reports, acknowledgements, the stream a disconnected
// session resumes. That may lag; a message delivered late is still delivered, and
// the sequence numbers make the lag detectable.
//
// The other is the venue's answer to "which order does this client mean?", and that
// may not lag, because a client does not retry a cancel that was refused. It was told
// no such order exists. So while the pump was behind, the venue would refuse a cancel
// for an order that was live in its own book, and neither side would ever revisit it:
// the order stayed in the book, addressable by nobody, until the venue restarted.
//
// The two ran on the same queue because they were written at the same time, not
// because they belong together. This splits them. Naming happens here, on the
// matching goroutine, in one map write. Everything expensive stays on the pump.
//
// # What it cost to find
//
// Nothing found this. Not 480 tests, not the fuzzers, not the race detector, not any
// benchmark — because it is not a race and not a wrong answer, it is a right answer
// arriving after the question stopped being asked. It took a soak: 30 seconds at
// 10,000 messages a second orphaned 12,843 orders, and the same workload at 4,000
// orphaned none. See docs/SOAK.md.
//
// # Use
//
// Attach it ahead of the Publisher, so an order is nameable no later than the moment
// the engine has accepted it:
//
//	sink := matching.MultiSink{orderentry.NewNameIndex(reg), pub, feed}
type NameIndex struct {
	reg *Registry
}

// NewNameIndex builds a sink that keeps reg's naming index level with the book.
func NewNameIndex(reg *Registry) *NameIndex { return &NameIndex{reg: reg} }

// OnEvents implements matching.EventSink. It runs on the matching goroutine, so it
// does exactly one thing and does it without allocating.
//
// Only acceptance is handled here. Forgetting a name stays with the pump, and
// deliberately: a name that outlives its order resolves to an id the engine no
// longer has, and the engine refuses that cancel correctly and cheaply. A name that
// has not arrived yet is the failure that has no recovery.
func (n *NameIndex) OnEvents(evs []matching.Event) {
	for i := range evs {
		if evs[i].Kind == matching.EventAccepted {
			n.reg.Name(evs[i].Order)
		}
	}
}
