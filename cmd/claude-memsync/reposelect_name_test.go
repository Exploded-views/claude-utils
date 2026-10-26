package main

import (
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// setRemotes points each repo's saved config at a remote URL, as a real host
// has after `init`.
func setRemotes(t *testing.T, cfgs map[string]config.Config, remotes map[string]string) {
	t.Helper()
	for name, url := range remotes {
		cfg := cfgs[name]
		cfg.RemoteURL = url
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func repoSelTest(t *testing.T, repo string) (string, error) {
	t.Helper()
	cfg, name, err := resolveRepo(repoSel{Repo: repo}, true)
	if err == nil && cfg.SyncDir == "" {
		t.Fatal("no config returned")
	}
	return name, err
}

// --repo takes the local route key, and also the repo's name on GitHub, with or
// without the owner, matched against each routed repo's remote URL.
func TestResolveRepo_AcceptsKeyNameAndOwnerName(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "bitc", "other", "mem-project-trap"}, "personal", nil)
	setRemotes(t, cfgs, map[string]string{
		"personal":         "git@github.com:example-org/mem-personal.git",
		"bitc":             "git@gh-mem-bitc:example-org/mem-project-bitc.git",
		"other":            "https://github.com/example-org/mem-project-other",
		"mem-project-trap": "git@github.com:example-org/mem-trap-remote.git",
	})
	cases := []struct{ in, want string }{
		{"bitc", "bitc"},                                // the key
		{"mem-project-bitc", "bitc"},                    // the GitHub repo name
		{"example-org/mem-project-bitc", "bitc"},        // owner/name
		{"Example-Org/Mem-Project-Bitc", "bitc"},        // GitHub names are case-insensitive
		{"mem-project-other", "other"},                  // https remote without .git
		{"mem-project-trap", "mem-project-trap"},        // an exact key always wins
		{"example-org/mem-trap-remote", "mem-project-trap"}, // ...and the owner/name still finds it
	}
	for _, c := range cases {
		got, err := repoSelTest(t, c.in)
		if err != nil || got != c.want {
			t.Errorf("--repo %q: got %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestResolveRepo_AmbiguousNameListsTheKeys(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "twin-a", "twin-b"}, "personal", nil)
	setRemotes(t, cfgs, map[string]string{
		"personal": "git@github.com:example-org/mem-personal.git",
		"twin-a":   "git@github.com:example-org/mem-shared.git",
		"twin-b":   "git@github.com:other-org/mem-shared.git",
	})
	_, err := repoSelTest(t, "mem-shared")
	if err == nil {
		t.Fatal("an ambiguous name was accepted")
	}
	for _, k := range []string{"twin-a", "twin-b", "ambiguous"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("message must mention %q: %v", k, err)
		}
	}
	if got, err := repoSelTest(t, "other-org/mem-shared"); err != nil || got != "twin-b" {
		t.Errorf("owner/name disambiguates: got %q %v", got, err)
	}
}

func TestResolveRepo_UnknownNameStillListsTheRepos(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "bitc"}, "personal", nil)
	setRemotes(t, cfgs, map[string]string{"bitc": "git@github.com:example-org/mem-project-bitc.git"})
	_, err := repoSelTest(t, "mem-nope")
	if err == nil || !strings.Contains(err.Error(), "bitc") || !strings.Contains(err.Error(), "mem-nope") {
		t.Fatalf("unknown: %v", err)
	}
}
