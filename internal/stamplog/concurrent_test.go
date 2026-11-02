package stamplog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const helperEnv = "STAMPLOG_HELPER_FILE"

// TestHelperProcess is not a test: the process tests run the test binary with
// the env switch set to make it append.
func TestHelperProcess(t *testing.T) {
	p := os.Getenv(helperEnv)
	if p == "" {
		t.Skip("helper only")
	}
	id := os.Getenv("STAMPLOG_HELPER_ID")
	n, _ := strconv.Atoi(os.Getenv("STAMPLOG_HELPER_N"))
	for i := 0; i < n; i++ {
		if err := Append(p, entryText(id, i), time.Now(), Options{LockTimeout: 30 * time.Second}); err != nil {
			fmt.Fprintln(os.Stderr, "helper append:", err)
			os.Exit(3)
		}
	}
}

func entryText(id string, i int) string {
	return fmt.Sprintf("id=%s-%d %s", id, i, strings.Repeat("p", 3072))
}

func verifyLog(t *testing.T, p string, ids []string, per int) {
	t.Helper()
	data := mustRead(t, p)
	if probs := Check(strings.NewReader(data)); len(probs) != 0 {
		t.Fatalf("Check found %d problems, first: %v", len(probs), probs[0])
	}
	lines := strings.SplitAfter(data, "\n")
	lines = lines[:len(lines)-1]
	if want := len(ids) * per; len(lines) != want {
		t.Fatalf("%d lines, want %d", len(lines), want)
	}
	seen := map[string]int{}
	for _, l := range lines {
		// Byte-exact: stamp prefix, then the padded text.
		rest := strings.TrimSuffix(l[len("- 2026-11-02 18:30Z "):], "\n")
		var id string
		fmt.Sscanf(rest, "id=%s", &id)
		var name string
		var i int
		k := strings.LastIndex(id, "-")
		name = id[:k]
		i, _ = strconv.Atoi(id[k+1:])
		if rest != entryText(name, i) {
			t.Fatalf("line is not byte-exact: %.80q", l)
		}
		seen[id]++
	}
	for _, name := range ids {
		for i := 0; i < per; i++ {
			if c := seen[fmt.Sprintf("%s-%d", name, i)]; c != 1 {
				t.Fatalf("id %s-%d seen %d times", name, i, c)
			}
		}
	}
}

func TestAppend_ConcurrentGoroutinesNeverInterleave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.md")
	const workers, per = 16, 200
	now := time.Date(2026, 11, 2, 18, 30, 0, 0, time.UTC)
	var wg sync.WaitGroup
	var ids []string
	for w := 0; w < workers; w++ {
		id := "g" + strconv.Itoa(w)
		ids = append(ids, id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := Append(p, entryText(id, i), now, Options{LockTimeout: 30 * time.Second}); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	verifyLog(t, p, ids, per)
}

func TestAppend_ConcurrentProcessesNeverInterleave(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	p := filepath.Join(t.TempDir(), "log.md")
	const procs, per = 4, 150
	var cmds []*exec.Cmd
	var ids []string
	for i := 0; i < procs; i++ {
		id := "p" + strconv.Itoa(i)
		ids = append(ids, id)
		c := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		c.Env = append(os.Environ(), helperEnv+"="+p, "STAMPLOG_HELPER_ID="+id, "STAMPLOG_HELPER_N="+strconv.Itoa(per))
		c.Stderr = os.Stderr
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("helper: %v", err)
		}
	}
	// The helpers stamp with the real clock; verifyLog only needs the prefix
	// shape, so compare the text part.
	data := mustRead(t, p)
	if probs := Check(strings.NewReader(data)); len(probs) != 0 {
		t.Fatalf("Check: %v", probs[0])
	}
	lines := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	if len(lines) != procs*per {
		t.Fatalf("%d lines, want %d", len(lines), procs*per)
	}
	seen := map[string]bool{}
	for _, l := range lines {
		text := l[len("- 2026-11-02 18:30Z "):]
		var id string
		fmt.Sscanf(text, "id=%s", &id)
		k := strings.LastIndex(id, "-")
		n, _ := strconv.Atoi(id[k+1:])
		if text != entryText(id[:k], n) || seen[id] {
			t.Fatalf("interleaved or duplicate line: %.60q", l)
		}
		seen[id] = true
	}
}
