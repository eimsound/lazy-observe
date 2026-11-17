package main

import (
	"fmt"
	"os"
)

// Version, Commit, and Date are populated by release builds with -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "lazy-observe: %v\n", err)
		os.Exit(1)
	}
}
