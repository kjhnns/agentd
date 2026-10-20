//go:build !linux

package main

func noDumps() error { return nil }
