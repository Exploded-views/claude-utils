package ghguard

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestListMemRepos_OnlyValidMemNames(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []fakeRepo{{Name: "mem-personal"}, {Name: "website"}, {Name: "mem-project-x"}, {Name: "mem-bogus"}, {Name: "Mem-personal"}})
	})
	got, err := c.ListMemRepos("Org")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "mem-personal,mem-project-x" {
		t.Errorf("got %v", got)
	}
}

func TestListDeployKeys_PaginatesAndParses(t *testing.T) {
	var srvURL string
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/Org/mem-personal/keys" || r.Method != "GET" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/Org/mem-personal/keys?page=2>; rel="next"`, srvURL))
			writeJSON(w, 200, []map[string]any{
				{"id": 1, "title": "vm-a", "read_only": false, "created_at": "2026-01-02T03:04:05Z", "last_used": "2026-02-01T00:00:00Z", "key": "ssh-ed25519 AAAA"},
			})
		default:
			writeJSON(w, 200, []map[string]any{
				{"id": 2, "title": "vm-b", "read_only": true, "created_at": "2026-01-03T00:00:00Z", "last_used": nil},
			})
		}
	})
	srvURL = c.BaseURL
	keys, err := c.ListDeployKeys("Org", "mem-personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].ID != 1 || keys[0].Title != "vm-a" || keys[0].ReadOnly || keys[0].LastUsed.IsZero() {
		t.Errorf("first key: %+v", keys)
	}
	if keys[1].ID != 2 || !keys[1].ReadOnly || !keys[1].LastUsed.IsZero() || keys[1].CreatedAt.IsZero() {
		t.Errorf("second key: %+v", keys[1])
	}
}

func TestDeployKeys_RefuseNonMemRepos(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request may be made for a non-mem repo: %s %s", r.Method, r.URL.Path)
	})
	if _, err := c.ListDeployKeys("Org", "website"); err == nil {
		t.Error("list must refuse a non-mem repo")
	}
	if err := c.DeleteDeployKey("Org", "website", 5); err == nil {
		t.Error("delete must refuse a non-mem repo")
	}
}

func TestDeleteDeployKey(t *testing.T) {
	var gotMethod, gotPath string
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(204)
	})
	if err := c.DeleteDeployKey("Org", "mem-personal", 42); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" || gotPath != "/repos/Org/mem-personal/keys/42" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
}

func TestDeleteDeployKey_ForbiddenIsDistinct(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]string{"message": "Resource not accessible by personal access token"})
	})
	err := c.DeleteDeployKey("Org", "mem-personal", 42)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("token leaked")
	}
	c2 := newServer(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 404, map[string]string{}) })
	if err := c2.DeleteDeployKey("Org", "mem-personal", 42); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
