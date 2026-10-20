//go:build linux

package main

import "syscall"

// noDumps sets the process non-dumpable (PR_SET_DUMPABLE 0): no core dump
// even through a piped collector like Apport (which ignores RLIMIT_CORE), and
// no ptrace attach by other processes of the same user (deploy review r2 #5).
func noDumps() error {
	const prSetDumpable = 4
	_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
