package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/version"
)

// Git runs the filter, textconv and merge drivers it finds in a clone's config,
// not the daemon's code. So the config must follow the build the operator runs:
// init and the daemon's start point it at the running build, preferably through
// the stable link (~/.local/bin/claude-memsync) so a relink alone upgrades it.

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// binSet is the running build's programs as git config should name them.
type binSet struct {
	ms, mm       string // what to write: the stable link when it leads to the build, else the resolved path
	msRes, mmRes string // resolved paths; mm and mmRes are "" when no claude-memmerge sits beside the build
}

// resolveReal is p with symlinks resolved, or p cleaned when it cannot be.
func resolveReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// binDirOf is the directory of the stable links.
func binDirOf(cfg config.Config) string {
	if cfg.BinDir != "" {
		return cfg.BinDir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "bin")
}

// runningBinaries describes the running claude-memsync and the claude-memmerge
// beside it. ok is false when there is no usable running build: it cannot be
// found, no longer exists, or lives under a temp dir or go build cache, where
// it is a throwaway that git config must not name.
func runningBinaries(cfg config.Config) (binSet, bool) {
	exe, err := executable()
	if err != nil {
		return binSet{}, false
	}
	res := resolveReal(exe)
	if !fileExists(res) || underTemp(res) || underTemp(exe) {
		return binSet{}, false
	}
	b := binSet{ms: res, msRes: res}
	mm := filepath.Join(filepath.Dir(res), exeName("claude-memmerge"))
	if fileExists(mm) {
		b.mm, b.mmRes = mm, resolveReal(mm)
	}
	if dir := binDirOf(cfg); dir != "" {
		if c := filepath.Join(dir, exeName("claude-memsync")); fileExists(c) && resolveReal(c) == b.msRes {
			b.ms = c
		}
		if b.mm != "" {
			if c := filepath.Join(dir, exeName("claude-memmerge")); fileExists(c) && resolveReal(c) == b.mmRes {
				b.mm = c
			}
		}
	}
	return b, true
}

// versionOf is what `<exe> version` prints after the program name ("" when it
// cannot be run).
func versionOf(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if f := strings.Fields(s); len(f) > 1 && strings.HasPrefix(f[0], "claude-mem") {
		s = strings.TrimSpace(strings.TrimPrefix(s, f[0]))
	}
	return s
}

// ownVersion is the version of the build at res; this process's own as a
// fallback.
func ownVersion(res string) string {
	if v := versionOf(res); v != "" {
		return v
	}
	return version.String()
}

// staleExe reports whether the program a clone's config names (cur) should be
// replaced by want, which resolves to wantRes and reports wantVer. A missing or
// unrunnable program is stale; so is one of a different version. The running
// build reached by another spelling is stale only when want is the stable one;
// the same version at another path is left alone.
func staleExe(cur, want, wantRes, wantVer string) bool {
	if cur == "" || !fileExists(cur) {
		return true
	}
	if resolveReal(cur) == wantRes {
		return cur != want
	}
	v := versionOf(cur)
	return v == "" || v != wantVer
}

// filterVersionNote describes, for status, a clone whose git filters run another
// build than the running one ("" when they agree or there is no verdict).
func filterVersionNote(cfg config.Config, repo *gitwt.Repo) string {
	b, ok := runningBinaries(cfg)
	if !ok {
		return ""
	}
	cur := configuredExe(repo, "filter.memage.process")
	if cur == "" {
		return ""
	}
	if !fileExists(cur) {
		return fmt.Sprintf("warning: the git filters of %s name %s, which is missing; restart the daemon so it repoints them", cfg.SyncDir, cur)
	}
	if resolveReal(cur) == b.msRes {
		return ""
	}
	cv, ov := versionOf(cur), ownVersion(b.msRes)
	if cv == ov {
		return ""
	}
	if cv == "" {
		cv = "unknown"
	}
	return fmt.Sprintf("warning: the git filters of %s run claude-memsync %s (%s) but this is %s; restart the daemon so it repoints them, or run init", cfg.SyncDir, cv, cur, ov)
}

// logRepoint writes the one line per repo that says the filters moved.
func logRepoint(dir, cur string, b binSet) {
	old := "a missing binary"
	if cur != "" && fileExists(cur) {
		if old = versionOf(cur); old == "" {
			old = "an unrunnable binary"
		}
	}
	log.Printf("encryption: %s: repointed git filters from %s to %s", dir, old, ownVersion(b.msRes))
}
