// Command benchgate runs the benchmark gate: it builds the base and head trees,
// runs each gated benchmark interleaved between them, and judges the result
// (docs/BENCH-GATE.md §5).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "usage: benchgate compare -base <rev> [-head <rev>] [-enforce] [-out bench-result.json]")
	os.Exit(2)
}
