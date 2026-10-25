package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// repoSel is the --repo / --config / --routes trio the per-repo commands share.
type repoSel struct {
	Repo, Config, Routes string
}

// extractRepoFlags pulls --repo, --config and --routes (as "--x v" or
// "--x=v") out of args wherever they stand and returns the rest in order, for
// commands whose own arguments are positional (`recipients add <key>`).
func extractRepoFlags(args []string) (sel repoSel, rest []string, err error) {
	targets := map[string]*string{"--repo": &sel.Repo, "--config": &sel.Config, "--routes": &sel.Routes}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		dst, ok := targets[name]
		if !ok {
			rest = append(rest, a)
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return sel, nil, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		*dst = val
	}
	return sel, rest, nil
}

// repoList names the repos of a routes file for an error message.
func (s *routedSet) repoList() string {
	var out []string
	for _, n := range s.File.RepoNames() {
		if _, have := s.Cfgs[n]; have {
			out = append(out, n)
		} else {
			out = append(out, n+" (not on this host)")
		}
	}
	return strings.Join(out, ", ")
}

// resolveRepo picks the one repo a command acts on.
//
//   - --repo <name> (a routes.json repos key) and --config <path> each name a
//     repo; giving both is an error.
//   - With routes present and neither given: explicit means refuse, listing the
//     repos; otherwise the personal repo (else the first by name).
//   - Without routes: --repo is an error; the config is --config or the default.
//
// name is the routes key of the repo ("" in single-repo mode or when --config
// names a dir routes.json does not list). A missing config.json yields the
// platform defaults, as distill always did.
func resolveRepo(sel repoSel, explicit bool) (cfg config.Config, name string, err error) {
	if sel.Repo != "" && sel.Config != "" {
		return cfg, "", errors.New("--repo and --config both name a repo; give one")
	}
	path, routed, err := resolveRoutesPath(sel.Routes)
	if err != nil {
		return cfg, "", err
	}
	var set *routedSet
	if routed {
		if set, err = loadRouted(path); err != nil {
			return cfg, "", err
		}
	}
	switch {
	case sel.Repo != "":
		if set == nil {
			return cfg, "", errors.New("--repo needs a routes file (see --routes)")
		}
		c, ok := set.Cfgs[sel.Repo]
		if !ok {
			return cfg, "", fmt.Errorf("no repo %q on this host; repos: %s", sel.Repo, set.repoList())
		}
		return c, sel.Repo, nil
	case sel.Config != "":
		// An explicit path is never replaced by defaults: a typo must not
		// act on the default repo.
		cfg, err = loadConfig(sel.Config)
		if err == nil && set != nil {
			name = set.repoForSyncDir(cfg.SyncDir)
		}
		return cfg, name, err
	case set == nil:
		cfg, err = loadDistillConfig("")
		return cfg, "", err
	case explicit:
		return cfg, "", fmt.Errorf("this host has several repos (%s): say which with --repo <name> or --config <path>", set.repoList())
	}
	c, ok := set.Primary()
	if !ok {
		return cfg, "", fmt.Errorf("%s: this host has none of the repos", path)
	}
	for n, x := range set.Cfgs {
		if x.SyncDir == c.SyncDir {
			name = n
		}
	}
	return c, name, nil
}

// isPersonalRepo reports whether cfg is the repo that holds the distilled
// catalog: the personal repo of a routes file, or the only repo in single-repo
// mode.
func isPersonalRepo(cfg config.Config) bool { return isPersonalRepoIn("", cfg) }

func isPersonalRepoIn(routesFlag string, cfg config.Config) bool {
	path, ok, err := resolveRoutesPath(routesFlag)
	if err != nil || !ok {
		return true
	}
	set, err := loadRouted(path)
	if err != nil {
		return true
	}
	name := set.repoForSyncDir(cfg.SyncDir)
	if name == "" {
		return true
	}
	if set.File.Personal != "" {
		return name == set.File.Personal
	}
	return name == set.File.RepoNames()[0] // no personal repo named: the first, as Primary picks
}
