//go:build !linux

package main

func oomKilledGuest() (bool, error) { return false, errNotLinux }

func exposeToOOMKiller(int) error { return nil }
