package main

import "time"

// The probes run immediately before every benchmark invocation and measure the
// machine rather than the code: if either moves more than taintLimit between the two
// runs of a pair, the machine changed under the comparison and the pair cannot be
// trusted either way.

// aluProbe is cmd/obsoak's speedProbe, copied rather than shared: obsoak is a
// command, and moving the probe into a package both can import is slice B's job
// (docs/BENCH-GATE.md §9.4). It measures how much CPU this process can get.
func aluProbe() time.Duration {
	start := time.Now()
	x := uint64(1)
	for i := 0; i < 20_000_000; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 33
	}
	probeSink = x
	return time.Since(start)
}

// memWalkBytes is far larger than any last-level cache the runners have, so the walk
// measures memory latency, which an ALU loop cannot see: a noisy neighbour that
// thrashes the cache slows a book walk and leaves aluProbe untouched.
const memWalkBytes = 64 << 20

var walk []uint32

// memProbe follows a dependent chain of random indices through 64 MiB: each load
// names the next, so no two can overlap and the time is latency, not bandwidth.
func memProbe() time.Duration {
	if walk == nil {
		walk = make([]uint32, memWalkBytes/4)
		// A single cycle through every slot (Sattolo's shuffle), from a fixed
		// generator so every run walks the same chain.
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
	probeSink = uint64(p)
	return time.Since(start)
}

var probeSink uint64
