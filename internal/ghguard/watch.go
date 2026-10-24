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
// A repo that is no longer private stops pushing (commits stay local). A single
// API failure keeps the last known state, but pushing also stops (fail closed)
// after MaxFailures consecutive failures or MaxAge without a successful check.
type Watch struct {
	C        *Client
	Owner    string
	Name     string
	Now      func() time.Time // defaults to time.Now
	StateDir string           // where the status file goes (e.g. <sync>/.state)
	Logf     func(format string, args ...any)

	// Precheck, when set, runs on every Allowed call (not rate limited); an
	// error blocks pushing until it passes again (e.g. the push URL changed).
	Precheck func() error

	MaxFailures int           // default 24
	MaxAge      time.Duration // default 24h

	mu         sync.Mutex
	last       time.Time
	lastOK     time.Time
	etag       string
	notPrivate bool
	stale      bool
	fails      int
	failLogged bool
	wrote      bool
	preBad     bool
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

func (w *Watch) ok() bool { return !w.notPrivate && !w.stale && !w.preBad }

// Allowed returns true while pushing is permitted.
func (w *Watch) Allowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if w.Precheck != nil {
		if err := w.Precheck(); err != nil {
			if !w.preBad {
				w.preBad = true
				w.logf("pushing stopped: %v", err)
				w.writeState("unverifiable since " + now.UTC().Format(time.RFC3339) + " (push target changed)")
				w.wrote = true
			}
			return false
		}
		if w.preBad {
			w.preBad = false
			w.logf("push target check passes again")
			w.writeState(w.stateText(now))
		}
	}
	if w.lastOK.IsZero() {
		w.lastOK = now // the daemon-start check is the baseline
	}
	if !w.last.IsZero() && now.Sub(w.last) < RecheckEvery {
		return w.ok()
	}
	w.last = now
	maxF, maxAge := w.MaxFailures, w.MaxAge
	if maxF <= 0 {
		maxF = 24
	}
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}

	repo, etag, unchanged, err := w.C.getRepoCond(w.Owner, w.Name, w.etag)
	if err != nil {
		w.fails++
		if !w.failLogged {
			w.failLogged = true
			w.logf("private-only recheck failed (keeping last known state): %v", err)
		}
		if !w.stale && (w.fails >= maxF || now.Sub(w.lastOK) >= maxAge) {
			w.stale = true
			w.logf("repo %s/%s could not be verified private for too long: pushing stopped, commits stay local", w.Owner, w.Name)
			w.writeState("unverifiable since " + now.UTC().Format(time.RFC3339))
			w.wrote = true
		}
		return w.ok()
	}
	w.failLogged = false
	w.fails = 0
	w.lastOK = now
	wasStale := w.stale
	w.stale = false
	changed := false
	if !unchanged {
		w.etag = etag
		bad := repo.verifyPrivate() != nil
		if bad != w.notPrivate {
			changed = true
			w.notPrivate = bad
			if bad {
				w.logf("repo %s/%s is no longer private: pushing stopped, commits stay local", w.Owner, w.Name)
			} else {
				w.logf("repo %s/%s is private again: pushing resumed", w.Owner, w.Name)
			}
		}
	}
	if changed || wasStale || !w.wrote {
		w.writeState(w.stateText(now))
		w.wrote = true
	}
	return w.ok()
}

func (w *Watch) stateText(now time.Time) string {
	switch {
	case w.notPrivate:
		return "not private since " + now.UTC().Format(time.RFC3339)
	case w.stale:
		return "unverifiable since " + now.UTC().Format(time.RFC3339)
	}
	return "private"
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
