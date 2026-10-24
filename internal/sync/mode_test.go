package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func TestCheckMode(t *testing.T) {
	dir := t.TempDir()
	// No record: a sync dir made before modes were recorded is a path-mode one.
	if err := CheckMode(dir, config.ProjectKeyPath); err != nil {
		t.Errorf("no record, path: %v", err)
	}
	err := CheckMode(dir, config.ProjectKeyRemote)
	if err == nil || !strings.Contains(err.Error(), "fresh sync dir") {
		t.Errorf("no record, remote must be refused with the way out, got %v", err)
	}

	if err := RecordMode(dir, config.ProjectKeyRemote); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, ".state", "mode")); strings.TrimSpace(got) != "remote" {
		t.Errorf("mode file = %q", got)
	}
	if err := CheckMode(dir, config.ProjectKeyRemote); err != nil {
		t.Errorf("recorded remote, remote: %v", err)
	}
	if err := CheckMode(dir, config.ProjectKeyPath); err == nil || !strings.Contains(err.Error(), "fresh sync dir") {
		t.Errorf("recorded remote, path must be refused, got %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".state", ".*tmp")); len(left) > 0 {
		t.Errorf("temp file left behind: %v", left)
	}

	if err := os.WriteFile(filepath.Join(dir, ".state", "mode"), []byte("sideways\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckMode(dir, config.ProjectKeyRemote); err == nil {
		t.Error("an unreadable record must not pass as a match")
	}
}

// The daemon will not start in a mode other than the one its sync dir was
// initialised for, and says to init into a fresh one.
func TestLoopRun_RefusesAModeThatDiffersFromTheSyncDirs(t *testing.T) {
	gitEnv(t)
	for name, record := range map[string]string{"no record": "", "recorded path": "path"} {
		t.Run(name, func(t *testing.T) {
			a := newMachine(t, "alice") // remote
			if err := gitwt.New(a.cfg.SyncDir).Init("main"); err != nil {
				t.Fatal(err)
			}
			if record != "" {
				if err := RecordMode(a.cfg.SyncDir, record); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := (&Loop{Cfg: a.cfg}).Run(ctx)
			if err == nil || !strings.Contains(err.Error(), "fresh sync dir") {
				t.Fatalf("Run = %v; want a refusal that names init into a fresh sync dir", err)
			}
			if _, err := os.Stat(ProjectCachePath(a.cfg.SyncDir)); err == nil {
				t.Error("a refused start must not touch the cache")
			}
		})
	}
}
