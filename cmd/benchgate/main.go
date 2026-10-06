// Command benchgate runs the benchmark gate: it builds the base and head trees,
// runs each gated benchmark interleaved between them, and judges the result
// (docs/BENCH-GATE.md §5).
//
//	benchgate compare -base-dir <checkout> [-head-dir .] [-enforce] [-out bench-result.json]
//	benchgate calibrate <dir of bench-result.json files>
//
// The binary that runs is the judge. In CI it is built from the BASE tree, so a head
// commit cannot loosen the gate that judges it (docs/BENCH-GATE.md §5.2).
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "compare":
			os.Exit(compare(os.Args[2:]))
		case "calibrate":
			os.Exit(runCalibrate(os.Args[2:]))
		}
	}
	fmt.Fprintln(os.Stderr, "usage: benchgate compare -base-dir <checkout> [-head-dir .] [-enforce] [-out bench-result.json]\n       benchgate calibrate <dir>")
	os.Exit(2)
}
