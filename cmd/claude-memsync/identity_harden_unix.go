//go:build !windows

package main

import "syscall"

// hardenProcess keeps the key out of a core dump: the core file size limit
// goes to 0 (the soft limit only; the hard limit is not ours to lower) and,
// on linux, the process becomes undumpable (no ptrace attach by the same
// user, no core). Best effort: a failure is ignored, because the command's
// other guards do not depend on it.
func hardenProcess() {
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_CORE, &lim) == nil && lim.Cur != 0 {
		lim.Cur = 0
		_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &lim)
	}
	hardenDumpable()
}
