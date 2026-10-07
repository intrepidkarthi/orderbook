// Package probe measures the machine, not the code. A benchmark that ran while the
// machine was busy measured the machine; these two probes are how a harness notices.
//
// cmd/obsoak runs ALU before and after a soak and warns on drift, so two runs on
// different machine conditions are not tabled together. cmd/benchgate runs both
// before every benchmark invocation and leaves out a pair whose probes moved. They
// used to carry two copies of the same loop; this is the one copy.
package probe

import "time"

// sink keeps the compiler from deleting the probes' work.
var sink uint64

// ALU runs a fixed amount of dependent arithmetic and reports how long it took: how
// much CPU this process can get right now.
//
// A load average would answer that on Linux and need a different mechanism on
// Darwin. This needs neither, and it measures the thing that matters, how much CPU
// this process can get, rather than a number the kernel keeps about everybody.
func ALU() time.Duration {
	start := time.Now()
	x := uint64(1)
	for i := 0; i < 20_000_000; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 33
	}
	sink = x
	return time.Since(start)
}

// walkBytes is far larger than any last-level cache the runners have, so the walk
// measures memory latency, which ALU cannot see: a neighbour that thrashes the cache
// slows a book walk and leaves the arithmetic untouched.
const walkBytes = 64 << 20

var walk []uint32

// Mem follows a dependent chain of random indices through 64 MiB: each load names the
// next, so no two can overlap and the time is latency, not bandwidth. The chain is
// built once, as a single cycle through every slot (Sattolo's shuffle) from a fixed
// generator, so every call walks the same path.
func Mem() time.Duration {
	if walk == nil {
		walk = make([]uint32, walkBytes/4)
		for i := range walk {
			walk[i] = uint32(i)
		}
		x := uint64(0x5EED)
		for i := len(walk) - 1; i > 0; i-- {
			x = x*6364136223846793005 + 1442695040888963407
			j := int((x >> 33) % uint64(i))
			walk[i], walk[j] = walk[j], walk[i]
		}
	}
	start := time.Now()
	p := uint32(0)
	for i := 0; i < 2_000_000; i++ {
		p = walk[p]
	}
	sink = uint64(p)
	return time.Since(start)
}
