package ghguard

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A repo transferred while the daemon runs: the hourly recheck follows the
// redirect and sees private:true under another owner.
func TestWatchHoldsTransferredRepo(t *testing.T) {
	moved := false
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if moved && r.URL.Path == "/repos/Org/mem-personal" {
			http.Redirect(w, r, "/repos/Other/mem-personal", http.StatusMovedPermanently)
			return
		}
		if moved {
			writeJSON(w, 200, fakeRepo{Name: "mem-personal", FullName: "Other/mem-personal", Owner: own("Other"), Private: bp(true), Visibility: "private"})
			return
		}
		writeJSON(w, 200, fakeRepo{Name: "mem-personal", FullName: "Org/mem-personal", Owner: own("Org"), Private: bp(true), Visibility: "private"})
	})
	clk := &clock{time.Unix(1000000, 0)}
	w := &Watch{C: c, Owner: "Org", Name: "mem-personal", Now: clk.now, StateDir: t.TempDir()}
	if !w.Allowed() {
		t.Fatal("first")
	}
	moved = true
	clk.t = clk.t.Add(61 * time.Minute)
	if w.Allowed() {
		t.Fatal("push still allowed after transfer to another owner")
	}
}

func TestWatchKeepsHoldOn304(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"m"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"m"`)
		writeJSON(w, 200, privRepo("Other/mem-personal", "mem-personal", "Other"))
	})
	clk := &clock{time.Unix(1000000, 0)}
	w := &Watch{C: c, Owner: "Org", Name: "mem-personal", Now: clk.now, StateDir: t.TempDir()}
	for i := 0; i < 3; i++ {
		if w.Allowed() {
			t.Fatalf("recheck %d (200 then 304s): push allowed for a repo owned by Other", i)
		}
		clk.t = clk.t.Add(61 * time.Minute)
	}
	if st := ReadState(w.StateDir); !strings.Contains(st, "repo moved to Other/mem-personal") {
		t.Fatalf("state = %q", st)
	}
}

func TestWatchRenameWithinOwnerPassesWithWarning(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, privRepo("Org/mem-new", "mem-new", "Org"))
	})
	var warns []string
	c.Warn = func(s string) { warns = append(warns, s) }
	clk := &clock{time.Unix(1000000, 0)}
	w := &Watch{C: c, Owner: "Org", Name: "mem-personal", Now: clk.now, StateDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		if !w.Allowed() {
			t.Fatal("a same-owner rename must pass")
		}
		clk.t = clk.t.Add(61 * time.Minute)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "repo renamed to Org/mem-new") {
		t.Fatalf("warns = %v", warns)
	}
}

func TestWatchResumesWhenIdentityMatchesAgain(t *testing.T) {
	owner := "Other"
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, privRepo(owner+"/mem-personal", "mem-personal", owner))
	})
	clk := &clock{time.Unix(1000000, 0)}
	w := &Watch{C: c, Owner: "Org", Name: "mem-personal", Now: clk.now, StateDir: t.TempDir()}
	if w.Allowed() {
		t.Fatal("moved repo allowed")
	}
	owner = "Org"
	clk.t = clk.t.Add(61 * time.Minute)
	if !w.Allowed() {
		t.Fatal("not resumed after the owner matches again")
	}
}
