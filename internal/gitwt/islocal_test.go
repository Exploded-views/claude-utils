package gitwt

import "testing"

func TestIsLocalURLIsTotal(t *testing.T) {
	cases := map[string]bool{
		"": true, "a": true, "a:": false, "C:": false, "c:": false, ":": false, "::": false,
		`C:\x`: true, "C:/x": true, "c:/x": true, "/srv/r.git": true, "../r.git": true,
		"file:///r.git": true, "ssh://h/r": false, "https://h/r": false,
		"git@host:org/r.git": false, "host:r.git": false, "./a:b": true,
	}
	for u, want := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("isLocalURL(%q) panicked: %v", u, r)
				}
			}()
			if got := isLocalURL(u); got != want {
				t.Errorf("isLocalURL(%q) = %v, want %v", u, got, want)
			}
		}()
	}
	// No string of up to 3 characters from a small alphabet may panic.
	alpha := []string{"a", "C", ":", "/", `\`, "."}
	var walk func(s string, n int)
	walk = func(s string, n int) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("isLocalURL(%q) panicked: %v", s, r)
				}
			}()
			isLocalURL(s)
		}()
		if n == 0 {
			return
		}
		for _, a := range alpha {
			walk(s+a, n-1)
		}
	}
	walk("", 3)
}
