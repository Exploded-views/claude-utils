package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_ScrubDefaultsOnForExistingConfigs(t *testing.T) {
	// A config written before the scrub options existed.
	c, err := Load(writeCfg(t, `{"remoteUrl":"git@example.com:me/mem.git","debounceMs":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Scrub {
		t.Error("scrub should default to true")
	}
	if c.RequireDenylist {
		t.Error("requireDenylist should default to false")
	}
}

func TestLoad_ScrubOptions(t *testing.T) {
	c, err := Load(writeCfg(t, `{"scrub":false,"requireDenylist":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scrub || !c.RequireDenylist {
		t.Errorf("got scrub=%v requireDenylist=%v", c.Scrub, c.RequireDenylist)
	}
}

func TestDenylistPath_SitsNextToConfig(t *testing.T) {
	c := Defaults()
	c.SyncDir = filepath.Join("home", "alice", ".claudesync")
	if filepath.Dir(c.DenylistPath()) != filepath.Dir(c.Path()) {
		t.Errorf("denylist %s not next to config %s", c.DenylistPath(), c.Path())
	}
}

func TestExpiry(t *testing.T) {
	c, err := Load(writeCfg(t, `{"expiresAt":"2026-06-01"}`))
	if err != nil {
		t.Fatal(err)
	}
	day := func(s string) time.Time { x, _ := time.Parse(time.RFC3339, s); return x }
	if c.Expired(day("2026-05-31T23:59:59Z")) || c.Expired(day("2026-06-01T00:00:00Z")) || c.Expired(day("2026-06-01T23:59:59Z")) {
		t.Error("the host is usable through its last day")
	}
	if !c.Expired(day("2026-06-02T00:00:00Z")) {
		t.Error("the day after the date it is expired")
	}
	var none Config
	if none.Expired(day("2100-01-01T00:00:00Z")) {
		t.Error("no expiry never expires")
	}
	if _, ok := none.ExpiryDate(); ok {
		t.Error("no date set")
	}
	if _, err := Load(writeCfg(t, `{"expiresAt":"next week"}`)); err == nil {
		t.Error("a malformed expiry must fail loudly, not silently never expire")
	}
}

// rekey-pq rewrites config.json through Load and SaveAt: the fields it does not
// touch must come back unchanged, and the file stays private.
func TestSaveAt_RoundTripKeepsOtherFieldsAndIsPrivate(t *testing.T) {
	p := writeCfg(t, `{"remoteUrl":"git@example.com:me/mem.git","debounceMs":1234,"encryptionScheme":"classic","hostLabel":"","recoveryRecipients":["old"]}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	c.EncryptionScheme, c.HostLabel, c.RecoveryRecipients = "pq", "vm-1", []string{"age1pq1x"}
	if err := SaveAt(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.EncryptionScheme != "pq" || got.HostLabel != "vm-1" || len(got.RecoveryRecipients) != 1 || got.RecoveryRecipients[0] != "age1pq1x" {
		t.Errorf("changed fields lost: %+v", got)
	}
	if got.RemoteURL != "git@example.com:me/mem.git" || got.DebounceMs != 1234 {
		t.Errorf("untouched fields changed: %+v", got)
	}
	if got.ConfigPath != p {
		t.Errorf("ConfigPath %q, want %q", got.ConfigPath, p)
	}
	if fi, err := os.Stat(p); err != nil || (os.PathSeparator == '/' && fi.Mode().Perm() != 0o600) {
		t.Errorf("config mode: %v %v", fi, err)
	}
}

func TestSave_CreatesTheSyncDirAndWritesConfigJSON(t *testing.T) {
	c := Defaults()
	c.SyncDir = filepath.Join(t.TempDir(), "new", "sync")
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(c.Path()); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{SyncDir: filepath.Join(c.Path(), "x")}); err == nil {
		t.Error("a sync dir under a file cannot be created")
	}
}

func TestFlagMarks(t *testing.T) {
	var c Config
	if c.FromFlag("remoteUrl") || c.FromFlag("githubTokenCmd") {
		t.Error("nothing is marked yet")
	}
	c.MarkFlag("remoteUrl")
	c.MarkFlag("githubTokenCmd")
	d := c // a copy keeps its own view
	d.MarkFlag("syncDir")
	if !c.FromFlag("remoteUrl") || !c.FromFlag("githubTokenCmd") || c.FromFlag("syncDir") || !d.FromFlag("syncDir") || !d.FromFlag("remoteUrl") {
		t.Errorf("marks wrong: %+v / %+v", c, d)
	}
}

func TestForDaemonDropsTheTokenCommandOnlyWhenNotRechecked(t *testing.T) {
	cmd := []string{"pass", "show", "x"}
	no, yes := false, true
	c := Config{GitHubTokenCmd: cmd}
	if c.ForDaemon().GitHubTokenCmd == nil {
		t.Error("kept when recheckTokenCmd is unset")
	}
	c.RecheckTokenCmd = &yes
	if c.ForDaemon().GitHubTokenCmd == nil {
		t.Error("kept when recheckTokenCmd is true")
	}
	c.RecheckTokenCmd = &no
	if c.ForDaemon().GitHubTokenCmd != nil {
		t.Error("the daemon must not run a token command that is not rechecked")
	}
	if c.GitHubTokenCmd == nil {
		t.Error("the caller's config was changed")
	}
}

func TestQuietSecAndDistilledPath(t *testing.T) {
	var c Config
	if c.QuietSec() != DefaultQuietSec {
		t.Errorf("default quiet %d", c.QuietSec())
	}
	n := 3
	c.StartQuietSec = &n
	if c.QuietSec() != 3 {
		t.Errorf("quiet %d", c.QuietSec())
	}
	if _, err := Load(writeCfg(t, `{"startQuietSec":-1}`)); err == nil {
		t.Error("a negative quiet window must be refused")
	}
	c.SyncDir = filepath.Join("s", "dir")
	if c.DistilledPath() != filepath.Join("s", "dir", "distilled") {
		t.Errorf("default distilled path %q", c.DistilledPath())
	}
	c.DistilledDir = filepath.Join("elsewhere", "d")
	if c.DistilledPath() != c.DistilledDir {
		t.Errorf("distilled path %q", c.DistilledPath())
	}
}

func TestDiscoverMergeDriverFailsWhenNoSiblingExists(t *testing.T) {
	if _, err := DiscoverMergeDriver(); err == nil {
		t.Skip("a claude-memmerge sits beside the test binary")
	}
}

func TestLoad_MissingFileReportsNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("want ErrNotExist, got %v", err)
	}
}

func TestLoad_RefusesADirectory(t *testing.T) {
	d := t.TempDir()
	_, err := Load(d)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), d) {
		t.Errorf("want a not-a-regular-file error naming the path, got %v", err)
	}
}

func TestLoad_RejectsBadContent(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"malformed JSON", `{"scrub":`, "parse"},
		{"wrong type", `{"debounceMs":"fast"}`, "parse"},
		{"foreign writer with no kind", `{"foreignWriters":[{}]}`, "foreignWriters[0]"},
		{"bad expiry", `{"expiresAt":"next week"}`, "expiresAt must be a YYYY-MM-DD date"},
		{"negative quiet", `{"startQuietSec":-1}`, "startQuietSec must not be negative"},
	}
	for _, tc := range cases {
		p := writeCfg(t, tc.body)
		_, err := Load(p)
		if err == nil {
			t.Errorf("%s: Load must fail", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), p) {
			t.Errorf("%s: error %q should mention %q and %q", tc.name, err, tc.want, p)
		}
	}
}

func TestLoad_AcceptsZeroQuietAndRecordsThePath(t *testing.T) {
	p := writeCfg(t, `{"startQuietSec":0,"expiresAt":"2030-01-02"}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfigPath != p || c.StartQuietSec == nil || *c.StartQuietSec != 0 || c.ExpiresAt != "2030-01-02" {
		t.Errorf("got %+v", c)
	}
}

func TestSaveAt_FailsWhenTheParentIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveAt(filepath.Join(blocker, "sub", "config.json"), Defaults()); err == nil {
		t.Error("creating a directory under a regular file must fail")
	}
	if b, _ := os.ReadFile(blocker); string(b) != "x" {
		t.Errorf("the blocking file was changed: %q", b)
	}
}

func TestSaveAt_FailsWhenThePathIsADirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveAt(p, Defaults()); err == nil {
		t.Error("writing over a directory must fail")
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		t.Errorf("the directory should be left alone: %v", err)
	}
}

func TestSave_FailsWhenSyncDirCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Defaults()
	c.SyncDir = filepath.Join(blocker, "sync")
	if err := Save(c); err == nil {
		t.Error("Save must report a SyncDir it cannot create")
	}
}

func TestDiscoverMergeDriverFindsASiblingOfTheExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	name := "claude-memmerge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	sibling := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Lstat(sibling); err == nil {
		t.Skip("a real claude-memmerge sits beside the test binary")
	}
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Skipf("cannot write beside the test binary: %v", err)
	}
	t.Cleanup(func() { os.Remove(sibling) })
	got, err := DiscoverMergeDriver()
	if err != nil {
		t.Fatal(err)
	}
	if got != sibling {
		t.Errorf("got %q, want %q", got, sibling)
	}
}

func TestDiscoverMergeDriverErrorNamesTheCandidate(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(exe), "claude-memmerge")); err == nil {
		t.Skip("a claude-memmerge sits beside the test binary")
	}
	_, err = DiscoverMergeDriver()
	if err == nil || !strings.Contains(err.Error(), "merge driver not found at") || !strings.Contains(err.Error(), filepath.Dir(exe)) {
		t.Errorf("want a not-found error naming the directory searched, got %v", err)
	}
}
