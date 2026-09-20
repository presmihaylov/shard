//go:build !linux

package main

func oomKilledGuest() (bool, error) { return false, errNotLinux }

const exposeFlag = "-expose"

func expose(string, []string) error { return errNotLinux }
