package main

import (
	"time"

	"github.com/intrepidkarthi/orderbook/internal/probe"
)

// The probes run immediately before every benchmark invocation and measure the
// machine rather than the code: if either moves more than taintLimit between the two
// runs of a pair, the machine changed under the comparison and the pair is left out.
// They live in internal/probe, shared with cmd/obsoak.

func aluProbe() time.Duration { return probe.ALU() }

func memProbe() time.Duration { return probe.Mem() }
