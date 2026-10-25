package main

import (
	"fmt"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/ghguard"
	"github.com/MarimerLLC/claude-utils/internal/related"
)

// ghVisibility is the visibility check of a relation's endpoints. The name must
// be a mem-* name; where a GitHub token is configured (the primary repo's
// config, as for doctor) the repo must also read back as private. A token that
// is configured but unusable fails closed; with no token the check is names
// only, as the private-only mode itself is without one.
type ghVisibility struct {
	names   related.NameOnly
	client  *ghguard.Client
	cfg     config.Config
	loadErr error
	remote  func(repo string) string // the configured remote URL of a repo this host has, or ""
}

func newGHVisibility(set *routedSet) related.Visibility {
	v := ghVisibility{}
	cfg, ok := set.Primary()
	if !ok {
		return v
	}
	v.cfg = cfg
	v.client, v.loadErr = guardClient(cfg)
	v.remote = func(repo string) string {
		if k, ok := set.localKey(repo); ok {
			return set.Cfgs[k].RemoteURL
		}
		return ""
	}
	return v
}

func (v ghVisibility) CheckPrivate(repo string) error {
	if err := v.names.CheckPrivate(repo); err != nil {
		return err
	}
	if v.loadErr != nil {
		return fmt.Errorf("cannot check visibility: %w", v.loadErr)
	}
	if v.client == nil {
		return nil // no token: names only
	}
	if v.remote != nil {
		if url := v.remote(repo); url != "" {
			return v.client.CheckRemote(url)
		}
	}
	org, err := orgFor(v.cfg, "")
	if err != nil {
		return fmt.Errorf("cannot check visibility: %w", err)
	}
	return v.client.CheckPrivate(org, repo)
}
