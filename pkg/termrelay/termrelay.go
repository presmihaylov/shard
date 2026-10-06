// Package termrelay runs a terminal exec under a guest pty, because runsc hands the guest the host's tty, which it cannot make a controlling terminal.
package termrelay

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"syscall"

	"github.com/presmihaylov/shard/pkg/launch"
)

// Mode is the relay's first argument to shard-init; the work directory and the command follow it.
const Mode = "terminal"

// FD is the guest fd the relay writes its record to.
const FD = 3

// The record is one started byte, or one errno record of a command that never ran.
const (
	started byte = 'S'
	failed  byte = 'E'
	// unentered is a record of the chdir into the work directory, which fails before the search starts.
	unentered byte = 'D'
)

// ErrNoRecord is a relay that ended without saying whether the command started.
var ErrNoRecord = errors.New("the terminal relay ended before it said whether the command started")

// Args is the process the runtime starts for one terminal exec. The relay enters workDir itself, so a refusal there is its own record.
func Args(relay, workDir string, argv []string) []string {
	return append([]string{relay, Mode, workDir}, argv...)
}

// Await reads the relay's record. onStart runs once the command runs, and a command that never ran is a launch.NotStartedError.
func Await(r io.Reader, onStart func()) error {
	// The relay writes its record in one write under PIPE_BUF, so one read holds all of it and no EOF is needed.
	blob := make([]byte, 32)
	n, err := r.Read(blob)
	if n > 0 && blob[0] == started {
		onStart()

		return nil
	}
	if n > 0 {
		return parseRecord(blob[:n])
	}
	if err == nil || errors.Is(err, io.EOF) {
		return ErrNoRecord
	}

	return fmt.Errorf("read the record of the terminal relay: %w", err)
}

func parseRecord(blob []byte) error {
	if len(blob) < 2 || (blob[0] != failed && blob[0] != unentered) {
		return fmt.Errorf("the terminal relay sent %q where it reports its command", blob)
	}

	errno, err := strconv.Atoi(string(blob[1:]))
	if err != nil || errno <= 0 || errno > 4095 {
		return fmt.Errorf("the terminal relay sent %q where it reports an errno", blob)
	}

	return &launch.NotStartedError{Errno: syscall.Errno(errno), Chdir: blob[0] == unentered}
}
