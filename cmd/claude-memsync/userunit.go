package main

import (
	"io"
)

// STUB (RED commit): behaviour arrives in the fix commit.

const userUnitName = "claude-memsync.service"

var unitGOOS = "linux"

var userUnitPathFn = func() (string, error) { return "", nil }

var systemctlFn = func(args ...string) error { return nil }

func fixUnitTarget(content string) (string, bool) { return content, false }

func ensureUserUnitTarget(w io.Writer) error { return nil }

func unitTargetWarning() string { return "" }
