package ghguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// ErrForbidden is a 403 from a write: the token lacks the permission (deploy
// keys need Administration: write on the repo).
var ErrForbidden = errors.New("GitHub refused: the token lacks the permission (deploy keys need Administration: write)")

// ErrNotFound is a 404: the key is already gone, or the token cannot see it.
var ErrNotFound = errors.New("GitHub reports not found")

// DeployKey is a deploy key of a repo, without the key material.
type DeployKey struct {
	ID        int64
	Title     string
	ReadOnly  bool
	CreatedAt time.Time
	LastUsed  time.Time // zero when GitHub has never seen it used
}

// ListMemRepos returns the sorted names of the org's repos this tool may
// touch: those ValidName accepts. Everything else is ignored.
func (c *Client) ListMemRepos(org string) ([]string, error) {
	repos, err := c.ListOrgRepos(org)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		if ValidName(r.Name) == nil {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListDeployKeys lists the deploy keys of a mem-* repo (title, access and
// timestamps; the key text is not read).
func (c *Client) ListDeployKeys(owner, name string) ([]DeployKey, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	var out []DeployKey
	err := c.getPages(c.base()+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/keys?per_page=100", owner+"/"+name+" deploy keys", func(b []byte) error {
		var page []struct {
			ID        int64      `json:"id"`
			Title     string     `json:"title"`
			ReadOnly  bool       `json:"read_only"`
			CreatedAt *time.Time `json:"created_at"`
			LastUsed  *time.Time `json:"last_used"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		for _, k := range page {
			d := DeployKey{ID: k.ID, Title: k.Title, ReadOnly: k.ReadOnly}
			if k.CreatedAt != nil {
				d.CreatedAt = *k.CreatedAt
			}
			if k.LastUsed != nil {
				d.LastUsed = *k.LastUsed
			}
			out = append(out, d)
		}
		return nil
	})
	return out, err
}

// DeleteDeployKey deletes one deploy key of a mem-* repo.
func (c *Client) DeleteDeployKey(owner, name string, id int64) error {
	if err := ValidName(name); err != nil {
		return err
	}
	code, _, _, err := c.do("DELETE", c.base()+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/keys/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusForbidden, http.StatusUnauthorized:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	}
	return fmt.Errorf("GitHub API returned HTTP %d deleting a deploy key of %s/%s", code, owner, name)
}
