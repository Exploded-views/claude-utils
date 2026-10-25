package route

import (
	"strings"
	"testing"
)

func suggestFile(kinds map[string]string) File {
	f := File{Repos: map[string]Repo{}}
	for n, k := range kinds {
		f.Repos[n] = Repo{Config: "/x/" + n, Kind: k}
		if k == KindPersonal {
			f.Personal = n
		}
	}
	return f
}

func TestSuggestHome(t *testing.T) {
	all := func(string) bool { return true }
	cases := []struct {
		name  string
		goos  string
		kinds map[string]string
		have  func(string) bool
		pin   string
		want  string
	}{
		{"mac with personal", "darwin", map[string]string{"p": KindPersonal, "o": KindOrch}, all, "", "p"},
		{"mac without personal, orch", "darwin", map[string]string{"o": KindOrch}, all, "", "o"},
		{"linux with orch", "linux", map[string]string{"p": KindPersonal, "o": KindOrch}, all, "", "o"},
		{"linux with infra", "linux", map[string]string{"p": KindPersonal, "i": KindInfra}, all, "", "i"},
		{"orch beats infra", "linux", map[string]string{"i": KindInfra, "o": KindOrch}, all, "", "o"},
		{"nothing fits", "linux", map[string]string{"p": KindPersonal, "c": KindClient, "x": KindProject}, all, "", ""},
		{"orch not on this host", "linux", map[string]string{"o": KindOrch}, func(string) bool { return false }, "", ""},
		{"mac personal not on host", "darwin", map[string]string{"p": KindPersonal}, func(string) bool { return false }, "", ""},
		{"already pinned to personal", "linux", map[string]string{"p": KindPersonal, "o": KindOrch}, all, "p", "p"},
	}
	for _, c := range cases {
		f := suggestFile(c.kinds)
		if c.pin != "" {
			f.Routes = []Rule{{Folder: "-h", Repo: c.pin}}
		}
		got, why := SuggestHome(f, c.goos, c.have, "-h")
		if got != c.want || why == "" {
			t.Errorf("%s: got %q (%s), want %q", c.name, got, why, c.want)
		}
	}
	_, why := SuggestHome(suggestFile(nil), "linux", all, "-h")
	if !strings.Contains(why, "deny") {
		t.Errorf("deny reason: %s", why)
	}
}
