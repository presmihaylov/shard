//go:build !linux

package main

import "os"

func freezeBound(*os.File) error { return nil }

func thawBound(*os.File) error { return nil }

func oomKilledGuest() (bool, error) { return false, errNotLinux }

const exposeFlag = "-expose"

func expose(string, []string) error { return errNotLinux }
