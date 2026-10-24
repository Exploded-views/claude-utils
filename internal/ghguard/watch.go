package ghguard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// RecheckEvery is the most often a Watch calls the API.
	RecheckEvery = time.Hour
	stateFile    = "visibility"
)

// Watch re-checks a repo's visibility at most once an hour, with an ETag so an
// unchanged repo costs a 304. Allowed reports whether pushing may continue.
// A repo that is no longer private stops pushing (commits stay local); an API
// failure keeps the last known state and never blocks by itself.
type Watch struct {
	C        *Client
	Owner    string
	Name     string
	Now      func() time.Time // defaults to time.Now
	StateDir string           // where the status file goes (e.g. <sync>/.state)
	Logf     func(format string, args ...any)

	mu         sync.Mutex
	last       time.Time
	etag       string
	notPrivate bool
	failLogged bool
}

func (w *Watch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watch) logf(f string, a ...any) {
	if w.Logf != nil {
		w.Logf(f, a...)
	}
}

// Allowed returns true while pushing is permitted.
func (w *Watch) Allowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if !w.last.IsZero() && now.Sub(w.last) < RecheckEvery {
		return !w.notPrivate
	}
	w.last = now

	repo, etag, unchanged, err := w.C.getRepoCond(w.Owner, w.Name, w.etag)
	if err != nil {
		if !w.failLogged {
			w.failLogged = true
			w.logf("private-only recheck failed (keeping last known state): %v", err)
		}
		return !w.notPrivate
	}
	w.failLogged = false
	if unchanged {
		return !w.notPrivate
	}
	w.etag = etag
	bad := repo.verifyPrivate() != nil
	if bad != w.notPrivate {
		w.notPrivate = bad
		if bad {
			w.logf("repo %s/%s is no longer private: pushing stopped, commits stay local", w.Owner, w.Name)
			w.writeState("not private since " + now.UTC().Format(time.RFC3339))
		} else {
			w.logf("repo %s/%s is private again: pushing resumed", w.Owner, w.Name)
			w.writeState("private")
		}
	}
	return !w.notPrivate
}

func (w *Watch) writeState(s string) {
	if w.StateDir == "" {
		return
	}
	_ = os.MkdirAll(w.StateDir, 0700)
	_ = os.WriteFile(filepath.Join(w.StateDir, stateFile), []byte(s+"\n"), 0600)
}

// ReadState returns the status text a Watch wrote in dir, or "".
func ReadState(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// getRepoCond is GetRepo with If-None-Match; unchanged is true on a 304.
func (c *Client) getRepoCond(owner, name, etag string) (Repo, string, bool, error) {
	target := c.base() + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	code, b, h, err := c.doH("GET", target, nil, etag)
	if err != nil {
		return Repo{}, "", false, err
	}
	if code == http.StatusNotModified {
		return Repo{}, etag, true, nil
	}
	if code != http.StatusOK {
		return Repo{}, "", false, fmt.Errorf("GitHub API returned HTTP %d for %s/%s", code, owner, name)
	}
	var r Repo
	if err := json.Unmarshal(b, &r); err != nil {
		return Repo{}, "", false, fmt.Errorf("unreadable API response for %s/%s", owner, name)
	}
	return r, h.Get("ETag"), false, nil
}
