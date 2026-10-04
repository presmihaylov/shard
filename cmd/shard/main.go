// Command shard is a single-node sandbox manager.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/presmihaylov/shard/cli"
)

// version is set at build time via -ldflags.
var version = "dev"

// stopSignals holds three, because shard run reads them one after the other while it stops its app.
const stopSignals = 3

func main() {
	// A pull is the long verb, so a stop signal has to cancel it rather than kill it mid-write.
	signals := make(chan os.Signal, stopSignals)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interrupts := cli.NewInterrupts(signals, cancel, func() { os.Exit(cli.InterruptedExitCode) })
	go interrupts.Watch()

	app := cli.App{Version: version, Out: os.Stdout, Err: os.Stderr, Interrupts: interrupts}

	if err := app.Run(ctx, os.Args[1:]); err != nil {
		// An exec'd command that failed is not a shard failure, so its code leaves without a word.
		// A command that never ran is shard's to explain, and it carries the words for it.
		var exit *cli.ExitError
		if errors.As(err, &exit) {
			if exit.Message != "" {
				fmt.Fprintln(os.Stderr, "shard:", exit.Message)
			}

			os.Exit(exit.Code)
		}

		fmt.Fprintln(os.Stderr, "shard:", err)
		os.Exit(1)
	}
}
