// Package config loads and saves the claude-memsync daemon configuration.
//
// Config lives at ~/.claudesync/config.json. We keep the format JSON to avoid
// a third-party dependency and keep the config trivially editable by hand.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/MarimerLLC/claude-utils/internal/foreign"
)

// Config controls the daemon's behavior. Defaults() returns a Config with
// reasonable per-platform defaults; only RemoteURL is required from the user.
type Config struct {
	// RemoteURL is the git remote to sync against (e.g.
	// git@github.com:MarimerLLC/claude-memories.git).
	RemoteURL string `json:"remoteUrl"`

	// SyncDir is the local sync work-tree (default: ~/.claudesync).
	SyncDir string `json:"syncDir"`

	// ClaudeProjectsDir is the source of truth for memories
	// (default: ~/.claude/projects).
	ClaudeProjectsDir string `json:"claudeProjectsDir"`

	// MergeDriverPath is the absolute path to the claude-memmerge binary.
	// Defaults to a sibling of the running daemon.
	MergeDriverPath string `json:"mergeDriverPath"`

	// DistilledDir holds the shared catalog of environment-level memories.
	// Empty means <SyncDir>/distilled (see DistilledPath). It lives inside the
	// work-tree so the daemon's git sync carries it across workstations.
	DistilledDir string `json:"distilledDir,omitempty"`

	// Scrub holds back, at commit time, any memory file that looks like it
	// holds a secret or matches the denylist (default: true). The Claude copy
	// of a held-back file is never touched; the mirror copy is re-checked on
	// the next cycle (and parked around each pull).
	Scrub bool `json:"scrub"`

	// RequireDenylist makes the daemon commit nothing while the denylist file
	// (see DenylistPath) is missing. Default false. Has no effect when Scrub
	// is false.
	RequireDenylist bool `json:"requireDenylist"`

	// PrivateOnly turns on the private-only guard: the mem-* name allowlist
	// and GitHub visibility checks at init and daemon start. Default false,
	// which leaves behavior exactly as without the guard.
	PrivateOnly bool `json:"privateOnly,omitempty"`

	// GitHubTokenEnv names the env var holding a GitHub API token used only
	// for the private-only visibility check (default MEMSYNC_GITHUB_TOKEN).
	GitHubTokenEnv string `json:"githubTokenEnv,omitempty"`

	// GitHubTokenFile is a file holding that token; it wins over the env
	// var. The token itself is never stored in config.json.
	GitHubTokenFile string `json:"githubTokenFile,omitempty"`

	// GitHubAPIURL overrides the API base URL (GitHub Enterprise, tests).
	GitHubAPIURL string `json:"githubApiUrl,omitempty"`

	// GitHubHost is the git host remotes must use (default github.com, or
	// the host of githubApiUrl). Set it for GitHub Enterprise.
	GitHubHost string `json:"githubHost,omitempty"`

	// RecheckMaxFailures and RecheckMaxAgeHours bound how long the daemon
	// keeps pushing without a successful visibility check (defaults 24 and
	// 24). Past either, pushing stops until a check succeeds.
	RecheckMaxFailures int `json:"recheckMaxFailures,omitempty"`
	RecheckMaxAgeHours int `json:"recheckMaxAgeHours,omitempty"`

	// GitHubOrg is the org `doctor` lists when --org is not given. Empty
	// means the owner parsed from RemoteURL.
	GitHubOrg string `json:"githubOrg,omitempty"`

	// ArchiveDeletes makes the daemon archive instead of delete: a note removed
	// from a Claude memory folder is moved to archive/<date>/<folder>/<file> in
	// the sync repo, and `distill --prune` archives catalog entries. Default
	// false (upstream behaviour); `init --archive-deletes` sets it.
	ArchiveDeletes bool `json:"archiveDeletes"`

	// ReadOnly makes this a read-only member: the daemon pulls and propagates
	// notes into the Claude tree but never commits or pushes, and every command
	// that would commit refuses. Set by `init --read-only`.
	ReadOnly bool `json:"readOnly,omitempty"`

	// HostLabel is the name this host goes by in deploy-key titles, recipient
	// comments ("# host:<label>") and the registry (hosts/<label>/). A name
	// you choose, never the machine's host name. Set by `init --host-label`.
	HostLabel string `json:"hostLabel,omitempty"`

	// CheckKeyWarn is the count of live check-App keys at which `host list`
	// and `doctor` warn (default 20; GitHub allows 25 per app).
	CheckKeyWarn int `json:"checkKeyWarn,omitempty"`

	// DebounceMs is how long to wait after a file change before committing.
	DebounceMs int `json:"debounceMs"`

	// PullIntervalSec is how often to pull from the remote when idle.
	PullIntervalSec int `json:"pullIntervalSec"`

	// ForeignWriters lists other tools that write into Claude memory folders
	// (a predecessor sync tool). While one is active, run and init refuse to
	// start. With a routes file, its own foreignWriters list is used instead.
	ForeignWriters []foreign.Writer `json:"foreignWriters,omitempty"`

	// StartQuietSec is the start-up quiet window in seconds (nil: 15, 0 turns
	// it off): the owned folders must not change during it, or the run refuses.
	StartQuietSec *int `json:"startQuietSec,omitempty"`
}

// DefaultQuietSec is the start-up quiet window when config.json does not say.
const DefaultQuietSec = 15

// QuietSec is the start-up quiet window in seconds.
func (c Config) QuietSec() int {
	if c.StartQuietSec == nil {
		return DefaultQuietSec
	}
	return *c.StartQuietSec
}

// Defaults returns a Config populated with platform-appropriate defaults.
// MergeDriverPath is left empty for the caller to fill via DiscoverMergeDriver().
func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		SyncDir:           filepath.Join(home, ".claudesync"),
		ClaudeProjectsDir: filepath.Join(home, ".claude", "projects"),
		Scrub:             true,
		DebounceMs:        3000,
		PullIntervalSec:   3600,
	}
}

// DiscoverMergeDriver returns the absolute path to claude-memmerge by looking
// for a sibling of the running executable.
func DiscoverMergeDriver() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	dir := filepath.Dir(exe)
	name := "claude-memmerge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("merge driver not found at %s: %w", candidate, err)
	}
	return candidate, nil
}

// Path returns the absolute path to the config file inside SyncDir.
func (c Config) Path() string {
	return filepath.Join(c.SyncDir, "config.json")
}

// DenylistPath returns the user's denylist file: one case-insensitive regular
// expression per line, kept next to config.json and never synced.
func (c Config) DenylistPath() string {
	return filepath.Join(c.SyncDir, "denylist.txt")
}

// DistilledPath returns the distilled-memory catalog directory, defaulting to
// <SyncDir>/distilled when DistilledDir is unset.
func (c Config) DistilledPath() string {
	if c.DistilledDir != "" {
		return c.DistilledDir
	}
	return filepath.Join(c.SyncDir, "distilled")
}

// Load reads the config from path. The path is typically Config.Path() but
// callers can supply their own (e.g. for tests).
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c := Defaults()
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := foreign.Validate(c.ForeignWriters); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.StartQuietSec != nil && *c.StartQuietSec < 0 {
		return Config{}, fmt.Errorf("%s: startQuietSec must not be negative", path)
	}
	return c, nil
}

// Save writes the config as pretty-printed JSON. Caller is responsible for
// having created SyncDir.
func Save(c Config) error {
	if err := os.MkdirAll(c.SyncDir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(c.Path(), b, 0600)
}
