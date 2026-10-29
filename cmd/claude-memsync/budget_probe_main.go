//go:build budgetprobe

package main

import (
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// Counting seam for TestBudgetB9_StartupSpawns: with MEMSYNC_PROBE_OUT set,
// every child the daemon starts through proc.Run is recorded (via the proc
// spawn seam), with the peak number running at once (all children, and the
// resolution ones: those run with -C outside the sync repos), and SIGUSR1
// writes {"spawns","peak_all","peak_res"} to that file. Off otherwise.
func init() {
	out := os.Getenv("MEMSYNC_PROBE_OUT")
	if out == "" {
		return
	}
	var mu sync.Mutex
	var spawns [][]string
	var curAll, peakAll, curRes, peakRes int
	syncRoot := ""
	if h, err := os.UserHomeDir(); err == nil {
		syncRoot = filepath.Join(h, ".claudesync.d")
	}
	isRes := func(args []string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-C" {
				return !strings.HasPrefix(args[i+1], syncRoot)
			}
		}
		return false
	}
	proc.SetSpawnHook(func(name string, args []string) {
		mu.Lock()
		spawns = append(spawns, append([]string{name}, args...))
		curAll++
		peakAll = max(peakAll, curAll)
		if isRes(args) {
			curRes++
			peakRes = max(peakRes, curRes)
		}
		mu.Unlock()
	})
	proc.SetSpawnEndHook(func(name string, args []string) {
		mu.Lock()
		curAll--
		if isRes(args) {
			curRes--
		}
		mu.Unlock()
	})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			mu.Lock()
			b, _ := json.Marshal(map[string]any{"spawns": spawns, "peak_all": peakAll, "peak_res": peakRes})
			mu.Unlock()
			tmp := out + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, out)
			}
		}
	}()
}
