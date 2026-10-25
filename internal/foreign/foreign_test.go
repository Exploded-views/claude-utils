package foreign

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func env(t *testing.T) Env {
	return Env{
		GOOS:      "linux",
		Home:      t.TempDir(),
		Systemctl: func(args ...string) (string, error) { return "disabled", errors.New("exit 1") },
		Processes: func() ([]string, error) { return nil, nil },
		Logf:      func(string, ...any) {},
	}
}

func TestFileWriter(t *testing.T) {
	e := env(t)
	p := filepath.Join(e.Home, "old", "config.json")
	w := Writer{File: p}
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("missing file must be inert: %v", got)
	}
	writeFile(t, p, `{"repo": ""}`)
	if got := Check([]Writer{w}, e); len(got) != 1 || got[0].Kind != "file" || got[0].Value != p {
		t.Fatalf("existing file without a key is active: %v", got)
	}
	w.JSONKeyNonEmpty = "repo"
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("empty key must be inert: %v", got)
	}
	for _, body := range []string{`{"other": "x"}`, `{"repo": null}`, `{"repo": []}`, `{"repo": {}}`, `{"repo": false}`, `{"repo": "  "}`} {
		writeFile(t, p, body)
		if got := Check([]Writer{w}, e); len(got) != 0 {
			t.Errorf("%s must be inert: %v", body, got)
		}
	}
	for _, body := range []string{`{"repo": "git@host:me/notes.git"}`, `{"repo": ["a"]}`, `{"repo": {"a":1}}`, `{"repo": true}`, `{"repo": 3}`} {
		writeFile(t, p, body)
		if got := Check([]Writer{w}, e); len(got) != 1 {
			t.Errorf("%s must be active: %v", body, got)
		}
	}
	// An unreadable config fails closed: it may well be the predecessor's.
	writeFile(t, p, `{not json`)
	if got := Check([]Writer{w}, e); len(got) != 1 {
		t.Errorf("unparsable file must be active: %v", got)
	}
}

func TestFileWriterExpandsHome(t *testing.T) {
	e := env(t)
	writeFile(t, filepath.Join(e.Home, ".old", "config.json"), `{"repo":"x"}`)
	got := Check([]Writer{{File: "~/.old/config.json", JSONKeyNonEmpty: "repo"}}, e)
	if len(got) != 1 {
		t.Fatalf("~ must expand to the home dir: %v", got)
	}
}

func TestSystemdUnit(t *testing.T) {
	e := env(t)
	var calls []string
	state := map[string]string{"is-enabled": "disabled", "is-active": "inactive"}
	e.Systemctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) != 3 || args[0] != "--user" || args[2] != "old-sync.timer" {
			t.Errorf("unexpected systemctl args %v", args)
		}
		return state[args[1]], errors.New("exit status 1")
	}
	w := Writer{SystemdUnit: "old-sync.timer"}
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("disabled+inactive is inert: %v", got)
	}
	state["is-enabled"] = "enabled"
	if got := Check([]Writer{w}, e); len(got) != 1 || got[0].Kind != "systemdUnit" || got[0].Value != "old-sync.timer" {
		t.Fatalf("enabled is active: %v", got)
	}
	state["is-enabled"], state["is-active"] = "disabled", "active"
	if got := Check([]Writer{w}, e); len(got) != 1 {
		t.Fatalf("active is active: %v", got)
	}
	if len(calls) == 0 {
		t.Fatal("runner never called")
	}
}

func TestSystemdSkippedOffLinux(t *testing.T) {
	e := env(t)
	e.GOOS = "windows"
	e.Systemctl = func(...string) (string, error) { t.Error("systemctl must not run off Linux"); return "enabled", nil }
	var logged []string
	e.Logf = func(f string, a ...any) { logged = append(logged, f) }
	if got := Check([]Writer{{SystemdUnit: "x.timer"}}, e); len(got) != 0 {
		t.Fatalf("skipped: %v", got)
	}
	if len(logged) != 1 {
		t.Errorf("want one skip log line, got %v", logged)
	}
}

func TestProcess(t *testing.T) {
	e := env(t)
	e.Processes = func() ([]string, error) {
		return []string{"/usr/bin/python3 /opt/old/sync.py --daemon", "/bin/sh"}, nil
	}
	if got := Check([]Writer{{Process: "nothing-here"}}, e); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	got := Check([]Writer{{Process: "old/sync.py"}}, e)
	if len(got) != 1 || got[0].Kind != "process" || got[0].Value != "old/sync.py" {
		t.Fatalf("%v", got)
	}
}

func TestClaudePluginUnlessFile(t *testing.T) {
	e := env(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	marker := filepath.Join(e.Home, "neutralised")
	w := Writer{ClaudePlugin: "old-sync@market", UnlessFile: marker}
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("no settings file is inert: %v", got)
	}
	writeFile(t, settings, `{"enabledPlugins": {"old-sync@market": true, "other@market": true}}`)
	if got := Check([]Writer{w}, e); len(got) != 1 || got[0].Kind != "claudePlugin" || got[0].Value != "old-sync@market" {
		t.Fatalf("enabled, unlessFile absent: active, got %v", got)
	}
	writeFile(t, marker, "")
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("unlessFile present: inert, got %v", got)
	}
	os.Remove(marker)
	writeFile(t, settings, `{"enabledPlugins": {"old-sync@market": false}}`)
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("disabled plugin is inert: %v", got)
	}
	writeFile(t, settings, `{"enabledPlugins": {}}`)
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("absent plugin is inert: %v", got)
	}
}

// onlyIfFile is the plugin's own config: removing it makes an enabled plugin
// inert, which is the usual way to neutralise one.
func TestClaudePluginOnlyIfFile(t *testing.T) {
	e := env(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	cfg := filepath.Join(e.Home, "old", "config.json")
	w := Writer{ClaudePlugin: "old-sync@market", OnlyIfFile: "~/old/config.json"}
	writeFile(t, settings, `{"enabledPlugins": {"old-sync@market": true}}`)
	if got := Check([]Writer{w}, e); len(got) != 0 {
		t.Fatalf("enabled but its config is gone: inert, got %v", got)
	}
	writeFile(t, cfg, `{}`)
	if got := Check([]Writer{w}, e); len(got) != 1 {
		t.Fatalf("enabled with its config: active, got %v", got)
	}
}

func TestClaudePluginWithoutFilesIsActiveWhenEnabled(t *testing.T) {
	e := env(t)
	writeFile(t, filepath.Join(e.Home, ".claude", "settings.json"), `{"enabledPlugins": {"p@m": true}}`)
	if got := Check([]Writer{{ClaudePlugin: "p@m"}}, e); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestValidate(t *testing.T) {
	ok := []Writer{{File: "/a"}, {File: "/a", JSONKeyNonEmpty: "k"}, {SystemdUnit: "u"}, {Process: "p"}, {ClaudePlugin: "a@b"}, {ClaudePlugin: "a@b", UnlessFile: "/x"}, {ClaudePlugin: "a@b", OnlyIfFile: "/x"}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	bad := []Writer{{}, {File: "/a", Process: "p"}, {JSONKeyNonEmpty: "k"}, {SystemdUnit: "u", UnlessFile: "/x"}, {ClaudePlugin: "nomarket"}, {Process: "p", OnlyIfFile: "/x"}}
	for _, w := range bad {
		if err := Validate([]Writer{w}); err == nil {
			t.Errorf("%+v should be refused", w)
		}
	}
}

func TestJSONForm(t *testing.T) {
	var ws []Writer
	in := `[{"file":"~/.old/config.json","jsonKeyNonEmpty":"repo"},{"systemdUnit":"o.timer"},{"process":"o.py"},{"claudePlugin":"o@m","unlessFile":"/x"}]`
	if err := json.Unmarshal([]byte(in), &ws); err != nil || len(ws) != 4 || ws[3].UnlessFile != "/x" || ws[0].JSONKeyNonEmpty != "repo" {
		t.Fatalf("%v %+v", err, ws)
	}
}

func TestDescribe(t *testing.T) {
	a := Active{Kind: "systemdUnit", Value: "o.timer"}
	if a.String() != "foreign writer systemdUnit o.timer" {
		t.Error(a.String())
	}
	err := Refusal([]Active{a})
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), "systemdUnit o.timer") || !strings.Contains(err.Error(), "--ignore-foreign-writers") {
		t.Errorf("%v", err)
	}
	if Refusal(nil) != nil {
		t.Error("no active writer, no refusal")
	}
}

// ---- quiet window ----

func memFile(t *testing.T, claude, folder, name, body string) string {
	t.Helper()
	p := filepath.Join(claude, folder, "memory", name)
	writeFile(t, p, body)
	return p
}

func TestQuietUnchangedPasses(t *testing.T) {
	claude := t.TempDir()
	memFile(t, claude, "a", "n.md", "one")
	var waited time.Duration
	err := QuietCheck([]Target{{Claude: claude}}, 15, func(d time.Duration) { waited = d })
	if err != nil {
		t.Fatal(err)
	}
	if waited != 15*time.Second {
		t.Errorf("waited %v", waited)
	}
}

func TestQuietZeroDisables(t *testing.T) {
	claude := t.TempDir()
	memFile(t, claude, "a", "n.md", "one")
	err := QuietCheck([]Target{{Claude: claude}}, 0, func(time.Duration) { t.Error("must not wait") })
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuietDetectsWriterGoroutine(t *testing.T) {
	claude := t.TempDir()
	p := memFile(t, claude, "-home-secret-client", "n.md", "one")
	started := make(chan struct{})
	done := make(chan struct{})
	var writes atomic.Int32
	go func() {
		<-started
		_ = os.WriteFile(p, []byte("two!"), 0o600) // size and hash change
		writes.Add(1)
		close(done)
	}()
	err := QuietCheck([]Target{{Claude: claude}}, 15, func(time.Duration) { close(started); <-done })
	if err == nil {
		t.Fatal("a write during the window must refuse")
	}
	msg := err.Error()
	if !strings.Contains(msg, "owned folder changed during start-up quiet window: something else writes there") {
		t.Errorf("message: %s", msg)
	}
	if strings.Contains(msg, "secret") || strings.Contains(msg, "n.md") {
		t.Errorf("message must name the folder by hash only: %s", msg)
	}
	if !strings.Contains(msg, hash8("-home-secret-client")) {
		t.Errorf("message lacks the folder hash: %s", msg)
	}
}

func TestQuietDetectsSameSizeSameMtimeContentChange(t *testing.T) {
	claude := t.TempDir()
	p := memFile(t, claude, "a", "n.md", "aaaa")
	st, _ := os.Stat(p)
	err := QuietCheck([]Target{{Claude: claude}}, 1, func(time.Duration) {
		_ = os.WriteFile(p, []byte("bbbb"), 0o600)
		_ = os.Chtimes(p, st.ModTime(), st.ModTime())
	})
	if err == nil {
		t.Fatal("hash must catch a content change that keeps size and mtime")
	}
}

func TestQuietDetectsAddedAndRemoved(t *testing.T) {
	claude := t.TempDir()
	p := memFile(t, claude, "a", "n.md", "x")
	if err := QuietCheck([]Target{{Claude: claude}}, 1, func(time.Duration) { memFile(t, claude, "a", "new.md", "y") }); err == nil {
		t.Error("added file must refuse")
	}
	if err := QuietCheck([]Target{{Claude: claude}}, 1, func(time.Duration) { os.Remove(p) }); err == nil {
		t.Error("removed file must refuse")
	}
}

func TestQuietOnlyLooksAtOwnedFolders(t *testing.T) {
	claude := t.TempDir()
	memFile(t, claude, "mine", "n.md", "x")
	other := memFile(t, claude, "theirs", "n.md", "x")
	owns := func(f string) bool { return f == "mine" }
	if err := QuietCheck([]Target{{Claude: claude, Owns: owns}}, 1, func(time.Duration) { _ = os.WriteFile(other, []byte("changed"), 0o600) }); err != nil {
		t.Fatalf("a folder the repo does not own must not count: %v", err)
	}
	mine := filepath.Join(claude, "mine", "memory", "n.md")
	if err := QuietCheck([]Target{{Claude: claude, Owns: owns}}, 1, func(time.Duration) { _ = os.WriteFile(mine, []byte("changed"), 0o600) }); err == nil {
		t.Fatal("an owned folder must count")
	}
}

func TestQuietMissingClaudeDirIsQuiet(t *testing.T) {
	if err := QuietCheck([]Target{{Claude: filepath.Join(t.TempDir(), "none")}}, 1, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
}
