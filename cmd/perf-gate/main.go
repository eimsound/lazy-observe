// Command perf-gate runs release and performance acceptance gates against real
// artifacts and live lazy-observe replicas. It never starts substitute storage
// services or treats a missing prerequisite as a passing measurement.
package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := runMain(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "perf-gate:", err)
		os.Exit(1)
	}
}

func runMain(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: perf-gate <release-size|query|scale> [flags]")
	}
	switch args[0] {
	case "release-size":
		return runReleaseSize(args[1:])
	case "query":
		return runQueryGate(args[1:])
	case "scale":
		return runScaleGate(args[1:])
	default:
		return fmt.Errorf("unknown gate %q", args[0])
	}
}
