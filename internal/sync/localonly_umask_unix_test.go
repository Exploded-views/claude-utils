//go:build !windows

package sync

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Under umask 077 git would check a planted Config.json out as 0600 and owned
// by the user, so the mode and owner rule on config.json cannot stop it: the
// folded per-PC name check alone must refuse the fetch, and the host's file
// stays as it was.
func TestPull_RefusesCaseVariantUnderUmask077(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	for _, c := range []struct{ tracked, local string }{
		{"Config.json", "config.json"},
		{"CONFIG.JSON", "config.json"},
		{"Denylist.txt", "denylist.txt"},
	} {
		t.Run(c.tracked, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
			captureLog(t)
			mustGit(t, f.repo, "config", "core.ignorecase", "true")
			local := filepath.Join(f.repo.Dir, c.local)
			if err := os.WriteFile(local, []byte("LOCAL"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.peerForce(c.tracked, `{"scrub":false}`)
			f.loop.pullTick(f.repo, f.roots)
			if err := runPull(f.repo, "main"); !errors.Is(err, ErrLocalOnlyTracked) {
				t.Fatalf("pull of %q must be refused by the name check, got %v", c.tracked, err)
			}
			if got, _ := os.ReadFile(local); string(got) != "LOCAL" {
				t.Errorf("host file changed: %q", got)
			}
		})
	}
}
