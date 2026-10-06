// Command benchgate runs the benchmark gate: it builds the base and head trees,
// runs each gated benchmark interleaved between them, and judges the result
// (docs/BENCH-GATE.md §5).
//
//	benchgate compare -base-dir <checkout> [-head-dir .] [-enforce] [-out bench-result.json]
//
// The binary that runs is the judge. In CI it is built from the BASE tree, so a head
// commit cannot loosen the gate that judges it (docs/BENCH-GATE.md §5.2).
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		os.Exit(compare(os.Args[2:]))
	}
	fmt.Fprintln(os.Stderr, "usage: benchgate compare -base-dir <checkout> [-head-dir .] [-enforce] [-out bench-result.json]")
	os.Exit(2)
}
