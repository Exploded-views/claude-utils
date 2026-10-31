package ghguard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func privRepo(full, name, owner string) fakeRepo {
	return fakeRepo{Name: name, FullName: full, Private: bp(true), Visibility: "private", Owner: own(owner)}
}

// redirecting serves a 301 from the requested repo to /repos/<to>, then body.
func redirecting(t *testing.T, to string, body any) (*Client, *[]string) {
	t.Helper()
	var warns []string
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/Org/mem-personal" {
			http.Redirect(w, r, "/repos/"+to, http.StatusMovedPermanently)
			return
		}
		writeJSON(w, 200, body)
	})
	c.Warn = func(s string) { warns = append(warns, s) }
	return c, &warns
}

func TestMovedToOtherOwnerIsHeld(t *testing.T) {
	c, _ := redirecting(t, "Other/mem-personal", privRepo("Other/mem-personal", "mem-personal", "Other"))
	err := c.CheckPrivate("Org", "mem-personal")
	if err == nil || !strings.Contains(err.Error(), "repo moved to Other/mem-personal: update routes.json and the remote") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("token in error")
	}
}

func TestRenameWithinOwnerPassesWithWarning(t *testing.T) {
	c, warns := redirecting(t, "Org/mem-renamed", privRepo("Org/mem-renamed", "mem-renamed", "Org"))
	if err := c.CheckPrivate("Org", "mem-personal"); err != nil {
		t.Fatal(err)
	}
	if len(*warns) != 1 || (*warns)[0] != "repo renamed to Org/mem-renamed: update the remote" {
		t.Fatalf("warnings = %q", *warns)
	}
}

func TestRedirectToPublicRepoIsRefused(t *testing.T) {
	r := privRepo("Org/mem-renamed", "mem-renamed", "Org")
	r.Private, r.Visibility = bp(false), "public"
	c, _ := redirecting(t, "Org/mem-renamed", r)
	if err := c.CheckPrivate("Org", "mem-personal"); err == nil {
		t.Fatal("public target must be refused")
	}
}

func TestRedirectToOtherHostDropsAuthorization(t *testing.T) {
	var gotAuth = "unset"
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, 200, privRepo("Org/mem-personal", "mem-personal", "Org"))
	}))
	t.Cleanup(other.Close)
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Go compares hostnames only, so reach the second server as localhost,
		// a different host from the first server's 127.0.0.1.
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/repos/Org/mem-personal", http.StatusMovedPermanently)
	})
	if err := c.CheckPrivate("Org", "mem-personal"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Fatalf("second host received Authorization %q", gotAuth)
	}
}

func TestMissingIdentityFailsClosed(t *testing.T) {
	cases := map[string]fakeRepo{
		"no-login":     {Name: "mem-personal", FullName: "Org/mem-personal", Private: bp(true), Visibility: "private"},
		"no-full-name": {Name: "mem-personal", Private: bp(true), Visibility: "private", Owner: own("Org")},
		"bad-full":     {Name: "mem-personal", FullName: "mem-personal", Private: bp(true), Visibility: "private", Owner: own("Org")},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newServer(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, body) })
			if err := c.CheckPrivate("Org", "mem-personal"); err == nil {
				t.Fatal("must refuse")
			}
		})
	}
}

func TestOwnerCaseDifferencePasses(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, privRepo("ORG/Mem-Personal", "mem-personal", "oRg"))
	})
	var warns []string
	c.Warn = func(s string) { warns = append(warns, s) }
	if err := c.CheckPrivate("org", "mem-personal"); err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("case-only difference must not warn: %q", warns)
	}
}

func TestVerifyIdentity(t *testing.T) {
	mk := func(full, login string) Repo {
		r := Repo{FullName: full}
		r.Owner.Login = login
		return r
	}
	cases := []struct {
		name       string
		r          Repo
		warn, errS string
	}{
		{"same", mk("Org/a", "Org"), "", ""},
		{"rename", mk("Org/b", "Org"), "repo renamed to Org/b: update the remote", ""},
		{"full-name-owner-differs", mk("X/a", "Org"), "", "repo moved to X/a"},
		{"login-differs", mk("Org/a", "X"), "", "repo moved to Org/a"},
		{"no-login", mk("Org/a", ""), "", "lacks"},
		{"no-full", mk("", "Org"), "", "lacks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := VerifyIdentity("Org", "a", tc.r)
			if w != tc.warn {
				t.Errorf("warn = %q, want %q", w, tc.warn)
			}
			if (err == nil) != (tc.errS == "") || (err != nil && !strings.Contains(err.Error(), tc.errS)) {
				t.Errorf("err = %v, want containing %q", err, tc.errS)
			}
		})
	}
}

func TestMalformedFullNameFailsClosed(t *testing.T) {
	for _, full := range []string{"me/", "me/repo/x", "/repo", "me", "me//repo"} {
		t.Run(full, func(t *testing.T) {
			r := Repo{FullName: full}
			r.Owner.Login = "me"
			w, err := VerifyIdentity("me", "repo", r)
			if err == nil {
				t.Fatal("must refuse a malformed full_name")
			}
			if w != "" {
				t.Errorf("warn = %q, want empty", w)
			}
		})
	}
}
