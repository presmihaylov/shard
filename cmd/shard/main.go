// Command shard is a single-node sandbox manager.
package main

import (
	"context"
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

func main() {
	// A pull is the long verb, so a stop signal has to cancel it rather than kill it mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go escape(ctx)

	app := cli.App{Version: version, Out: os.Stdout, Err: os.Stderr}

	if err := app.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "shard:", err)
		os.Exit(1)
	}
}

// escape leaves the second signal to the process rather than to the work the first one cancelled, so
// a give-back that hangs cannot trap the user at the keyboard.
func escape(ctx context.Context) {
	<-ctx.Done()

	second := make(chan os.Signal, 1)
	signal.Notify(second, os.Interrupt, syscall.SIGTERM)
	<-second
	signal.Stop(second)

	os.Exit(interruptedExitCode)
}
