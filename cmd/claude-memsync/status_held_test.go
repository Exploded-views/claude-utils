package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kardianos/service"
)

func TestStatus_CountsHeldBackNotesWithoutNamingThem(t *testing.T) {
	useFakeHost(t)
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	cfg := cfgs["projects"]
	notes := map[string]string{
		"denylist.txt":                     "nightjar\n",
		"projects/p1/memory/clean.md":      "harmless\n",
		"projects/p1/memory/nightjar-x.md": "harmless\n",
		"projects/p1/memory/body-only.md":  "about Nightjar\n",
	}
	for rel, body := range notes {
		p := filepath.Join(cfg.SyncDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })

	_, out := captureStdout(t, func() int { return runStatus(nil) })
	want := "  [projects] held back by the denylist: 2 notes (not synced; run in that project to see which)"
	if !strings.Contains(out, want) {
		t.Fatalf("missing count line, got:\n%s", out)
	}
	if strings.Contains(out, "[personal] held back") {
		t.Errorf("a repo with nothing held says nothing:\n%s", out)
	}
	for _, leak := range []string{"body-only", "nightjar", "Nightjar", "clean.md"} {
		if strings.Contains(out, leak) {
			t.Errorf("status leaks %q:\n%s", leak, out)
		}
	}
}
