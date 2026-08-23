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

// interruptedExitCode is what a shell reports for a command a SIGINT ended.
const interruptedExitCode = 130

// stopSignals holds two, because the escape below reads them one after the other.
const stopSignals = 2

func main() {
	// A pull is the long verb, so a stop signal has to cancel it rather than kill it mid-write.
	signals := make(chan os.Signal, stopSignals)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go escape(signals, cancel, func() { os.Exit(interruptedExitCode) })

	app := cli.App{Version: version, Out: os.Stdout, Err: os.Stderr}

	if err := app.Run(ctx, os.Args[1:]); err != nil {
		// An exec'd command that failed is not a shard failure, so its code leaves without a word.
		var exit *cli.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}

		fmt.Fprintln(os.Stderr, "shard:", err)
		os.Exit(1)
	}
}

// escape gives the first stop signal to the work and the second to the process, so a give-back that
// hangs cannot trap the user at the keyboard. Both come off one channel that is registered before
// either arrives: a channel registered only after the cancellation drops the signal that raced it.
func escape(signals <-chan os.Signal, cancel context.CancelFunc, leave func()) {
	<-signals
	cancel()
	<-signals
	leave()
}
