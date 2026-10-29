//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/related"
)

// N3: git runs in a process group of its own, so the terminal's Ctrl-C no
// longer reaches it. An interactive command must kill its live git groups on
// SIGINT/SIGTERM before it exits.
func TestInterruptKillsLiveGitBeforeExiting(t *testing.T) {
	for _, tc := range []struct {
		sig  os.Signal
		code int
	}{{syscall.SIGINT, 130}, {syscall.SIGTERM, 143}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			bin := t.TempDir()
			pids := filepath.Join(bin, "pids")
			script := "#!/bin/sh\necho $$ >> " + pids + "\nexec sleep 60\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GIT_SSH_COMMAND", "")
			t.Cleanup(func() { // stop a stray fake git by its exact PID
				b, _ := os.ReadFile(pids)
				for _, s := range strings.Fields(string(b)) {
					if p, err := strconv.Atoi(s); err == nil {
						_ = syscall.Kill(p, syscall.SIGKILL)
					}
				}
			})
			oN, oE := notifySignals, exitFn
			t.Cleanup(func() { notifySignals, exitFn = oN, oE })
			sig := make(chan os.Signal, 2)
			notifySignals = func() (<-chan os.Signal, func()) { return sig, func() {} }
			exited := make(chan int, 1)
			exitFn = func(c int) { exited <- c }
			stop := guardChildren()
			defer stop()

			c, err := related.NewReadClone(t.TempDir(), "mem-other", "")
			if err != nil {
				t.Fatal(err)
			}
			go c.Head()
			var first string
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if b, _ := os.ReadFile(pids); len(strings.Fields(string(b))) > 0 {
					first = strings.Fields(string(b))[0]
					break
				}
			}
			pid, err := strconv.Atoi(first)
			if err != nil {
				t.Fatal("the fake git never ran")
			}
			sig <- tc.sig
			select {
			case code := <-exited:
				if code != tc.code {
					t.Errorf("exit code %d, want %d", code, tc.code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the process did not exit after the signal")
			}
			if syscall.Kill(pid, 0) == nil {
				time.Sleep(200 * time.Millisecond)
				if syscall.Kill(pid, 0) == nil {
					t.Errorf("git %d still running when the process exited", pid)
				}
			}
		})
	}
}
