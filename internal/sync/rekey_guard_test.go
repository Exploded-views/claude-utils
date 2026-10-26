package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRekeyRecord puts a rekey-pq restore record into the fixture's git dir.
func (f *pullFixture) writeRekeyRecord(body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo.Dir, ".git", "memsync-pre-pq.json"), []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// N8: the daemon does not sync a repo whose rekey-pq is unfinished.
func TestFlush_RefusesToSyncAnUnfinishedRekey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		record  string
		scheme  string
		blocked bool
	}{
		{"migrated, scheme pq", `{"stage":"migrated"}`, "pq", true},
		{"committed, scheme pq", `{"stage":"committed"}`, "pq", true},
		{"published but scheme not pq", `{"stage":"published"}`, "", true},
		{"record unreadable", `{"stage`, "pq", true},
		{"published, scheme pq", `{"stage":"published"}`, "pq", false},
		{"unknown stage, scheme pq", `{"stage":"done"}`, "pq", true},
		{"record from an older build, scheme pq", `{"tip":"abc"}`, "pq", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"a.md": "one\n"})
			logs := captureLog(t)
			f.writeRekeyRecord(tc.record)
			if tc.scheme != "" {
				mustGit(t, f.repo, "config", "memage.scheme", tc.scheme)
			}
			f.editLocal("b.md", "two\n")
			_ = f.loop.flush(f.repo, f.roots, true)
			pushed := f.originHas("projects/p1/memory/b.md")
			if tc.blocked && pushed {
				t.Error("the daemon synced a repo with an unfinished rekey-pq")
			}
			if tc.blocked && !strings.Contains(logs.String(), "rekey-pq") {
				t.Errorf("the skipped cycle must say why in the log:\n%s", logs)
			}
			if !tc.blocked && !pushed {
				t.Error("a finished rekey must not stop the daemon")
			}
			if !tc.blocked && strings.Contains(logs.String(), "rekey-pq") {
				t.Errorf("a finished rekey must not log a hold:\n%s", logs)
			}
		})
	}
}
