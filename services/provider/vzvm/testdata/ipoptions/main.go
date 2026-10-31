// Command ipoptions dials a guest-local listener plain and then with IPv4 options, and prints how each dial ended, one line each.
package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	report("plain", dial(l.Addr().String(), nil))
	// Three NOPs and an end fill one word, so the header grows to six words.
	report("options", dial(l.Addr().String(), []byte{1, 1, 1, 0}))
}

func dial(addr string, options []byte) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	if options != nil {
		d.Control = func(_, _ string, raw syscall.RawConn) error {
			var set error
			if err := raw.Control(func(fd uintptr) {
				set = unix.SetsockoptString(int(fd), unix.IPPROTO_IP, unix.IP_OPTIONS, string(options))
			}); err != nil {
				return err
			}

			return set
		}
	}
	c, err := d.Dial("tcp4", addr)
	if err != nil {
		return err
	}

	return c.Close()
}

func report(dial string, err error) {
	if err != nil {
		fmt.Printf("%s: %v\n", dial, err)
		return
	}
	fmt.Printf("%s: connected\n", dial)
}
