//go:build budgetprobe

package main

import (
	"encoding/json"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// Counting seam for TestBudgetB9_StartupSpawns: with MEMSYNC_PROBE_OUT set,
// every child the daemon starts through proc.Run is recorded (via the proc
// spawn seam), and SIGUSR1 writes the list to that file. Off otherwise.
func init() {
	out := os.Getenv("MEMSYNC_PROBE_OUT")
	if out == "" {
		return
	}
	var mu sync.Mutex
	var spawns [][]string
	proc.SetSpawnHook(func(name string, args []string) {
		mu.Lock()
		spawns = append(spawns, append([]string{name}, args...))
		mu.Unlock()
	})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			mu.Lock()
			b, _ := json.Marshal(spawns)
			mu.Unlock()
			tmp := out + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, out)
			}
		}
	}()
}
