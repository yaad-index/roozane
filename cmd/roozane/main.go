// Command roozane is the single binary that runs the pipeline described in
// ADR-0001. This is the scaffolding entry point: it carries the release-version
// stamp and nothing else yet, so the collector, aggregator and sink layers have
// somewhere to hang off once they are built.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is overwritten at link time with the release version:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/roozane
//
// An unstamped build reports "dev", which is honest about being one rather than
// claiming a release it is not.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable body of main: it takes its arguments and streams rather
// than reading globals, and returns the process exit code instead of calling
// os.Exit, so the behaviour below can be asserted directly.
func run(args []string, stdout, stderr io.Writer) int {
	// Writes to the process's own stdout/stderr: there is nowhere useful to
	// report a failed write to, and the exit code already carries the outcome,
	// so the error is discarded explicitly rather than ignored implicitly.
	if len(args) == 1 && args[0] == "version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}

	_, _ = fmt.Fprintf(stderr, "roozane %s\n\nusage: roozane version\n", version)
	return 2
}
