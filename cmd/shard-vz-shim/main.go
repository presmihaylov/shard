//go:build darwin

// shard-vz-shim holds one Virtualization.framework VM behind a unix socket, so it outlives the daemon; the shape follows hypeman's cmd/vz-shim/main.go (561e34fd), see NOTICE.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	vzfw "github.com/Code-Hex/vz/v3"

	"github.com/presmihaylov/shard/pkg/vz"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-vz-shim:", err)
		os.Exit(1)
	}
}

func run() error {
	encoded := flag.String("config", "", "the VM configuration as JSON")
	flag.Parse()

	var cfg vz.Config
	if err := json.Unmarshal([]byte(*encoded), &cfg); err != nil {
		return fmt.Errorf("decode -config: %w", err)
	}

	// The socket is claimed before the boot, so a second shim for the same VM refuses instead of orphaning the first.
	listener, err := vz.Listen(cfg.Socket)
	if err != nil {
		return err
	}
	machine, err := vz.NewMachine(&cfg)
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	if err := machine.Boot(&cfg); err != nil {
		return errors.Join(err, listener.Close(), machine.Close())
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	served := make(chan error, 1)
	go func() { served <- vz.Serve(listener, machine, logger) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	changed := machine.Changed()
	// A stop the framework accepted but never reports past this ends the shim anyway: the VM dies with its process.
	var overdue <-chan time.Time
	for {
		select {
		case sig := <-signals:
			// SIGUSR1 forces a collection, so a test can prove the device files outlive one.
			if sig == syscall.SIGUSR1 {
				runtime.GC()
				debug.FreeOSMemory()
				logger.Printf("%s: collected", sig)

				continue
			}
			logger.Printf("%s: stopping the vm", sig)
			if err := machine.Stop(); err != nil {
				return err
			}
			overdue = time.After(stopGrace)
		case <-overdue:
			return fmt.Errorf("the vm did not report its stop within %s: exiting", stopGrace)
		case state := <-changed:
			if state != vzfw.VirtualMachineStateStopped && state != vzfw.VirtualMachineStateError {
				continue
			}
			logger.Printf("vm %s: exiting", vz.State(state.String()))
			if err := listener.Close(); err != nil {
				return fmt.Errorf("close the shim socket: %w", err)
			}

			return errors.Join(awaitServed(served), machine.Close())
		}
	}
}

// The VM is gone, so a connection still spliced on a guest stream is not worth more than this.
const stopGrace = 5 * time.Second

func awaitServed(served <-chan error) error {
	select {
	case err := <-served:
		return err
	case <-time.After(stopGrace):
		return fmt.Errorf("a shim connection still runs %s after the vm stopped: exiting", stopGrace)
	}
}
