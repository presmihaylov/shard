// Command keyprobe calls each keyring syscall once and prints how the kernel answered, one line each.
package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func main() {
	_, err := unix.AddKey("user", "shard-keyprobe", []byte("x"), unix.KEY_SPEC_SESSION_KEYRING)
	report("add_key", err)
	_, err = unix.KeyctlInt(unix.KEYCTL_GET_KEYRING_ID, unix.KEY_SPEC_SESSION_KEYRING, 0, 0, 0)
	report("keyctl", err)
	_, err = unix.RequestKey("user", "shard-keyprobe-absent", "", 0)
	report("request_key", err)
}

func report(call string, err error) {
	var errno unix.Errno
	if errors.As(err, &errno) {
		fmt.Printf("%s: %s\n", call, unix.ErrnoName(errno))
		return
	}
	fmt.Printf("%s: ok\n", call)
}
