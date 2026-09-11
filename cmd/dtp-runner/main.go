// Command dtp-runner executes one Eclipse RCP test suite on a worker node.
// It is launched by Nomad (docker or exec driver) or by the master's local
// backend, and is configured entirely by the RunSpec in DTP_RUN_SPEC.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/andrei/distributed-test-platform/internal/runner"
)

func main() {
	specPath := flag.String("spec", "", "path to a RunSpec JSON file (default: $DTP_RUN_SPEC)")
	flag.Parse()

	spec, err := runner.LoadSpec(*specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dtp-runner:", err)
		os.Exit(2)
	}

	// SIGTERM is what Nomad sends on kill; let the runner still upload what it
	// has and report a result before the kill timeout expires.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	os.Exit(runner.New(spec).Execute(ctx))
}
