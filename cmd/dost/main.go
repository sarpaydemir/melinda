// Command dost runs a self-hosted AI agent for Mattermost.
package main

import (
	"fmt"
	"io"
	"os"
)

// Build metadata, set with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintf(stdout, "dost %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}
	fmt.Fprintln(stderr, "usage: dost version")
	return 2
}
