//go:build !linux

package main

import "net"

func listenRequests() (net.Listener, error) { return nil, errNotLinux }

func peerIsHost(net.Conn) error { return errNotLinux }
