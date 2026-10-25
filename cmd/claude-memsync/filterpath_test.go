package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// upgradeEnv builds two real builds that report different versions, outside
// every temp root, and an encrypted clone set up by build A.
type upgradeEnv struct {
	cfg        config.Config
	repo       *gitwt.Repo
	msA, mmA   string
	msB, mmB   string
	home       string
	logs       *bytes.Buffer
	restoreAll func()
}

func newUpgradeEnv(t *testing.T) *upgradeEnv {
	t.Helper()
	cfg, _ := initFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	e := &upgradeEnv{cfg: cfg, home: home, logs: &bytes.Buffer{}}
	e.msA, e.mmA = testenv.BinariesVersion(t, "vA")
	e.msB, e.mmB = testenv.BinariesVersion(t, "vB")

	oldRoots, oldExe := tempRoots, executable
	tempRoots = func() []string { return []string{filepath.Join(os.TempDir(), "never-matches")} }
	log.SetOutput(e.logs)
	e.restoreAll = func() { tempRoots, executable = oldRoots, oldExe; log.SetOutput(os.Stderr) }
	t.Cleanup(e.restoreAll)

	e.cfg.MergeDriverPath = e.mmA
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: e.msA}
	if err := bootstrapWith(e.cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	e.repo = gitwt.New(e.cfg.SyncDir)
	return e
}

func (e *upgradeEnv) runAs(exe string) {
	executable = func() (string, error) { return exe, nil }
}

func (e *upgradeEnv) get(t *testing.T, key string) string {
	t.Helper()
	out, _ := e.repo.Run("config", "--get", key)
	return strings.TrimSpace(out)
}

var ownedKeys = []string{
	"filter.memage.process", "diff.memage.textconv",
	"merge.claude-memory-index.driver", "merge.memtext.driver", "merge.memunion.driver",
}

func (e *upgradeEnv) assertAllPoint(t *testing.T, ms, mm string) {
	t.Helper()
	for _, k := range ownedKeys {
		want := ms
		if strings.HasPrefix(k, "merge.") {
			want = mm
		}
		if got := e.get(t, k); !strings.Contains(got, "'"+want+"'") {
			t.Errorf("%s = %q, want it to name %s", k, got, want)
		}
	}
}

// The reported bug: init with build A, the operator switches to build B and
// restarts the daemon; git must follow to B.
func TestRefreshEncryption_RepointsFiltersToTheRunningBuild(t *testing.T) {
	e := newUpgradeEnv(t)
	e.assertAllPoint(t, e.msA, e.mmA)
	e.runAs(e.msB)
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	e.assertAllPoint(t, e.msB, e.mmB)
	if got := e.logs.String(); strings.Count(got, "repointed git filters from vA to vB") != 1 {
		t.Errorf("want one repoint line, got %q", got)
	}
	// Idempotent and quiet the second time.
	e.logs.Reset()
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	e.assertAllPoint(t, e.msB, e.mmB)
	if strings.Contains(e.logs.String(), "repointed") {
		t.Errorf("second refresh logged: %q", e.logs.String())
	}
}

// A different path to the same build is left alone.
func TestRefreshEncryption_SameVersionElsewhereIsKept(t *testing.T) {
	e := newUpgradeEnv(t)
	dir := t.TempDir()
	copyFile(t, e.msA, filepath.Join(dir, "claude-memsync"))
	copyFile(t, e.mmA, filepath.Join(dir, "claude-memmerge"))
	e.runAs(filepath.Join(dir, "claude-memsync"))
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	e.assertAllPoint(t, e.msA, e.mmA)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// With ~/.local/bin/claude-memsync resolving to the running build, the config
// names that symlink, so a later relink upgrades the filters by itself.
func TestRefreshEncryption_WritesTheStableSymlinkPath(t *testing.T) {
	e := newUpgradeEnv(t)
	bin := filepath.Join(e.home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := func(ms, mm string) {
		for from, to := range map[string]string{ms: "claude-memsync", mm: "claude-memmerge"} {
			p := filepath.Join(bin, to)
			os.Remove(p)
			if err := os.Symlink(from, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	stableMS, stableMM := filepath.Join(bin, "claude-memsync"), filepath.Join(bin, "claude-memmerge")
	link(e.msB, e.mmB)
	e.runAs(e.msB)
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	e.assertAllPoint(t, stableMS, stableMM)

	// The operator relinks to a newer build: nothing to rewrite.
	msC, mmC := testenv.BinariesVersion(t, "vC")
	link(msC, mmC)
	e.runAs(msC)
	e.logs.Reset()
	before, _ := e.repo.Run("config", "--local", "--list")
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := e.repo.Run("config", "--local", "--list")
	if before != after || strings.Contains(e.logs.String(), "repointed") {
		t.Errorf("a relink must need no rewrite; log %q", e.logs.String())
	}
	e.assertAllPoint(t, stableMS, stableMM)
}

// An already-current versioned path moves to the stable spelling once it exists.
func TestRefreshEncryption_BinDirFromConfig(t *testing.T) {
	e := newUpgradeEnv(t)
	bin := t.TempDir()
	os.Symlink(e.msB, filepath.Join(bin, "claude-memsync"))
	os.Symlink(e.mmB, filepath.Join(bin, "claude-memmerge"))
	e.cfg.BinDir = bin
	e.runAs(e.msB)
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	e.assertAllPoint(t, filepath.Join(bin, "claude-memsync"), filepath.Join(bin, "claude-memmerge"))
}

// A symlink that resolves to some other build is not a stable path for this one.
func TestRefreshEncryption_UnrelatedSymlinkNotUsed(t *testing.T) {
	e := newUpgradeEnv(t)
	bin := filepath.Join(e.home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	os.Symlink(e.msA, filepath.Join(bin, "claude-memsync"))
	e.runAs(e.msB)
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	e.assertAllPoint(t, e.msB, e.mmB)
}

// A build running from a temp dir is a throwaway: nothing is repointed.
func TestRefreshEncryption_TempDirBuildNotRepointed(t *testing.T) {
	e := newUpgradeEnv(t)
	tempRoots = func() []string { return []string{filepath.Dir(e.msB)} }
	e.runAs(e.msB)
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	e.assertAllPoint(t, e.msA, e.mmA)
	if strings.Contains(e.logs.String(), "repointed") {
		t.Errorf("logged a repoint: %q", e.logs.String())
	}
}

// Only memsync's own entries are rewritten.
func TestRefreshEncryption_LeavesOtherGitConfigAlone(t *testing.T) {
	e := newUpgradeEnv(t)
	other := map[string]string{
		"filter.lfs.process":      "git-lfs filter-process",
		"filter.other.process":    "'" + e.msA + "' something-else",
		"diff.other.textconv":     "'" + e.msA + "' cat",
		"merge.other.driver":      "'" + e.mmA + "' %O %A %B",
		"core.hooksPath":          "/x/hooks",
		"alias.st":                "status",
		"remote.origin.pushurl":   "git@example.invalid:o/r.git",
		"memage.somethingelse":    "keep",
		"merge.memtextish.driver": "'" + e.mmA + "' --text %O %A %B",
	}
	for k, v := range other {
		if err := e.repo.ConfigSet(k, v); err != nil {
			t.Fatal(err)
		}
	}
	e.runAs(e.msB)
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	for k, v := range other {
		if got := e.get(t, k); got != v {
			t.Errorf("%s changed: %q -> %q", k, v, got)
		}
	}
}

func TestEncryptionLines_WarnsWhenFilterBuildDiffers(t *testing.T) {
	e := newUpgradeEnv(t)
	e.runAs(e.msB)
	got := strings.Join(encryptionLines(e.cfg), "\n")
	if !strings.Contains(got, "vA") || !strings.Contains(got, "vB") || !strings.Contains(got, "git filters") {
		t.Errorf("no version warning in:\n%s", got)
	}
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(encryptionLines(e.cfg), "\n"); strings.Contains(got, "git filters") {
		t.Errorf("warning after the repoint:\n%s", got)
	}
	// A throwaway build gives no verdict.
	e.runAs(e.msA)
	if err := e.repo.ConfigSet("filter.memage.process", "'"+e.msA+"' age-filter"); err != nil {
		t.Fatal(err)
	}
	e.runAs(e.msB)
	tempRoots = func() []string { return []string{filepath.Dir(e.msB)} }
	if got := strings.Join(encryptionLines(e.cfg), "\n"); strings.Contains(got, "git filters") {
		t.Errorf("warned for a temp-dir build:\n%s", got)
	}
}

// The scenario end to end through git: the old build cannot read a newer
// recipients file (it fails closed), so after the upgrade the commit only goes
// through once the filters run the new build.
func TestRefreshEncryption_RecipientsWithHostLabelsCommitAfterUpgrade(t *testing.T) {
	e := newUpgradeEnv(t)
	// Build A, as the old build: right version, but its filter rejects the
	// newer recipients format.
	old := filepath.Join(t.TempDir(), "claude-memsync")
	script := "#!/bin/sh\n" +
		"case \"$1\" in version) echo 'claude-memsync vA'; exit 0;; esac\n" +
		"echo 'age: unsupported recipients line' >&2; exit 1\n"
	if err := os.WriteFile(old, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"filter.memage.process": "'" + old + "' age-filter",
		"diff.memage.textconv":  "'" + old + "' age-textconv",
	} {
		if err := e.repo.ConfigSet(k, v); err != nil {
			t.Fatal(err)
		}
	}
	rp := filepath.Join(e.cfg.SyncDir, ".age-recipients")
	b, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") && !strings.Contains(l, "# ") {
			l += " # host:laptop"
		}
		lines = append(lines, l)
	}
	if err := os.WriteFile(rp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(e.cfg.SyncDir, "note.md")
	if err := os.WriteFile(note, []byte("a note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit := func() error {
		if _, err := e.repo.Run("add", "-A"); err != nil {
			return err
		}
		_, err := e.repo.Run("commit", "-q", "-m", "labels")
		return err
	}
	if err := commit(); err == nil {
		t.Fatal("the old build should fail closed on the newer recipients file")
	}

	e.runAs(e.msB)
	if err := refreshEncryption(e.cfg); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatalf("commit after the upgrade: %v", err)
	}
	blob, _ := e.repo.Run("show", "HEAD:note.md")
	if strings.Contains(blob, "a note") {
		t.Error("note.md must be stored encrypted")
	}
	if rec, _ := e.repo.Run("show", "HEAD:.age-recipients"); !strings.Contains(rec, "# host:laptop") {
		t.Errorf("labelled recipients not committed:\n%s", rec)
	}
}
