package hosts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/ghguard"
)

var now = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func days(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }

func key(id int64, title string, ro bool, created, used time.Time) ghguard.DeployKey {
	return ghguard.DeployKey{ID: id, Title: title, ReadOnly: ro, CreatedAt: created, LastUsed: used}
}

func find(t *testing.T, r Report, label string) Host {
	t.Helper()
	for _, h := range r.Hosts {
		if h.Label == label {
			return h
		}
	}
	t.Fatalf("no host %q in %+v", label, r.Hosts)
	return Host{}
}

func TestBuild_GroupsByLabel(t *testing.T) {
	r := Build(Input{
		Now: now, StaleDays: 30,
		Keys: map[string][]ghguard.DeployKey{
			"mem-personal":  {key(1, "vm-a", false, days(90), days(2)), key(2, "vm-b", true, days(90), days(1))},
			"mem-project-x": {key(3, "vm-a", true, days(80), days(10))},
		},
		Local: map[string][]string{
			"mem-personal":  {"vm-a", "vm-c"},
			"mem-project-x": {},
		},
	})
	a := find(t, r, "vm-a")
	if len(a.Keys) != 2 || !a.LastUsed.Equal(days(2)) {
		t.Errorf("vm-a: %+v", a)
	}
	rw := map[string]bool{}
	for _, k := range a.Keys {
		rw[k.Repo] = !k.ReadOnly
	}
	if !rw["mem-personal"] || rw["mem-project-x"] {
		t.Errorf("rw/ro per repo wrong: %v", rw)
	}
	if !a.Recipient["mem-personal"] || a.Recipient["mem-project-x"] {
		t.Errorf("recipient presence: %v", a.Recipient)
	}
	if _, ok := find(t, r, "vm-b").Recipient["mem-personal"]; !ok || find(t, r, "vm-b").Recipient["mem-personal"] {
		t.Errorf("vm-b has a key but no recipient line here: %v", find(t, r, "vm-b").Recipient)
	}
	c := find(t, r, "vm-c") // recipient only, no deploy key
	if len(c.Keys) != 0 || !c.Recipient["mem-personal"] || c.Stale {
		t.Errorf("vm-c: %+v", c)
	}
	if strings.Join(r.LocalRepos, ",") != "mem-personal,mem-project-x" {
		t.Errorf("local repos: %v", r.LocalRepos)
	}
}

func TestBuild_StaleFlag(t *testing.T) {
	r := Build(Input{
		Now: now, StaleDays: 30,
		Keys: map[string][]ghguard.DeployKey{
			"mem-personal": {
				key(1, "fresh", false, days(200), days(3)),
				key(2, "old", false, days(200), days(45)),
				key(3, "never-new", false, days(5), time.Time{}),
				key(4, "never-old", false, days(60), time.Time{}),
			},
			"mem-project-x": {
				key(5, "mixed", false, days(200), days(100)),
				key(6, "mixed", true, days(200), days(4)),
			},
		},
	})
	want := map[string]bool{"fresh": false, "old": true, "never-new": false, "never-old": true, "mixed": false}
	for l, w := range want {
		if got := find(t, r, l).Stale; got != w {
			t.Errorf("%s stale = %v, want %v", l, got, w)
		}
	}
	// the threshold is configurable
	r = Build(Input{Now: now, StaleDays: 1, Keys: map[string][]ghguard.DeployKey{"mem-personal": {key(1, "fresh", false, days(200), days(3))}}})
	if !find(t, r, "fresh").Stale {
		t.Error("stale-days 1 should flag a key used 3 days ago")
	}
}

func TestBuild_ExpiredFlagAndRegistry(t *testing.T) {
	d := func(s string) time.Time { x, _ := time.Parse("2006-01-02", s); return x }
	r := Build(Input{
		Now: now, StaleDays: 30, HaveRegistry: true,
		Registry: map[string]RegHost{
			"past":   {Label: "past", Expiry: d("2026-05-31")},
			"today":  {Label: "today", Expiry: d("2026-06-01")},
			"future": {Label: "future", Expiry: d("2026-07-01")},
			"inreg":  {Label: "inreg"},
		},
		Keys: map[string][]ghguard.DeployKey{"mem-personal": {key(1, "past", false, days(5), days(1)), key(2, "nokey-in-registry-no", false, days(5), days(1))}},
	})
	if !find(t, r, "past").Expired || find(t, r, "today").Expired || find(t, r, "future").Expired {
		t.Errorf("expiry flags wrong: past=%v today=%v future=%v", find(t, r, "past").Expired, find(t, r, "today").Expired, find(t, r, "future").Expired)
	}
	if !find(t, r, "inreg").InRegistry || find(t, r, "nokey-in-registry-no").InRegistry {
		t.Error("registry presence wrong")
	}
	if r.RegistryKnown != true {
		t.Error("report should say a registry was consulted")
	}
}

func TestBuild_CheckKeyCountAndWarning(t *testing.T) {
	reg := map[string]RegHost{}
	for i := 0; i < 20; i++ {
		l := fmt.Sprintf("h%02d", i)
		reg[l] = RegHost{Label: l, CheckKey: &CheckKey{Fingerprint: "SHA256:" + strings.Repeat("A", 43), App: "chk"}}
	}
	reg["nofp"] = RegHost{Label: "nofp"}
	reg["other"] = RegHost{Label: "other", CheckKey: &CheckKey{Fingerprint: "SHA256:" + strings.Repeat("B", 43), App: "second"}}
	r := Build(Input{Now: now, StaleDays: 30, HaveRegistry: true, Registry: reg, CheckKeyWarn: 20})
	if len(r.CheckKeys) != 2 {
		t.Fatalf("want 2 apps, got %+v", r.CheckKeys)
	}
	var chk, second AppKeys
	for _, a := range r.CheckKeys {
		if a.App == "chk" {
			chk = a
		} else {
			second = a
		}
	}
	if chk.Live != 20 || !chk.Warn || second.Live != 1 || second.Warn {
		t.Errorf("chk=%+v second=%+v", chk, second)
	}
	if w := r.Warnings(); len(w) != 1 || !strings.Contains(w[0], "chk") || !strings.Contains(w[0], "25") {
		t.Errorf("warnings: %v", w)
	}
	reg2 := map[string]RegHost{}
	for k, v := range reg {
		reg2[k] = v
	}
	delete(reg2, "h00") // a retired host leaves the registry
	r = Build(Input{Now: now, StaleDays: 30, HaveRegistry: true, Registry: reg2, CheckKeyWarn: 20})
	if len(r.Warnings()) != 0 {
		t.Errorf("19 live keys must not warn: %v", r.Warnings())
	}
	r = Build(Input{Now: now, StaleDays: 30, HaveRegistry: true, Registry: reg2, CheckKeyWarn: 5})
	if len(r.Warnings()) != 1 {
		t.Error("the threshold is configurable")
	}
}

func TestSummaryLine(t *testing.T) {
	r := Build(Input{Now: now, StaleDays: 30, Keys: map[string][]ghguard.DeployKey{
		"mem-personal": {key(1, "old1", false, days(90), days(60)), key(2, "ok", false, days(90), days(1))},
	}})
	if got := r.Summary(); got != "hosts: 1 stale (old1), 0 expired" {
		t.Errorf("got %q", got)
	}
	r = Build(Input{Now: now, StaleDays: 30})
	if r.Summary() != "" {
		t.Errorf("nothing to say must print nothing: %q", r.Summary())
	}
}

func TestReadRegistry(t *testing.T) {
	dir := t.TempDir()
	w := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(body), 0o600)
	}
	w("hosts/vm-a/routes.json", "{}")
	w("hosts/vm-b/expiry", "2026-03-04\n")
	fp := "SHA256:" + strings.Repeat("a", 43)
	w("hosts/vm-c/check-key", `{"fingerprint":"`+fp+`","app":"my-check-app"}`)
	w("hosts/Bad_Label/routes.json", "{}")
	w("hosts/vm-d/expiry", "garbage")
	w("hosts/vm-e/check-key", `{"fingerprint":"-----BEGIN PRIVATE KEY-----","app":"x"}`)
	reg, err := ReadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg["vm-a"]; !ok {
		t.Error("vm-a missing")
	}
	if reg["vm-b"].Expiry.Format("2006-01-02") != "2026-03-04" {
		t.Errorf("vm-b: %+v", reg["vm-b"])
	}
	if ck := reg["vm-c"].CheckKey; ck == nil || ck.Fingerprint != fp || ck.App != "my-check-app" {
		t.Errorf("vm-c: %+v", reg["vm-c"])
	}
	if _, ok := reg["Bad_Label"]; ok {
		t.Error("invalid labels are ignored")
	}
	if !reg["vm-d"].Expiry.IsZero() || reg["vm-d"].Problem == "" {
		t.Errorf("vm-d: a bad expiry is unset and reported: %+v", reg["vm-d"])
	}
	if reg["vm-e"].CheckKey != nil || reg["vm-e"].Problem == "" {
		t.Errorf("vm-e: only SHA256 fingerprints are accepted, never key text: %+v", reg["vm-e"])
	}
}

func TestReadRegistry_MissingDirIsEmpty(t *testing.T) {
	reg, err := ReadRegistry(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(reg) != 0 {
		t.Errorf("%v %v", reg, err)
	}
}

func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func newRegistryRepo(t *testing.T) string {
	t.Helper()
	gitEnv(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return dir
}

func headCount(t *testing.T, dir string) string {
	t.Helper()
	out, _ := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").CombinedOutput()
	return strings.TrimSpace(string(out))
}

func TestWriteCheckKey_CommitsAndReadsBack(t *testing.T) {
	dir := newRegistryRepo(t)
	fp := "SHA256:" + strings.Repeat("c", 43)
	changed, err := WriteCheckKey(dir, "vm-a", CheckKey{Fingerprint: fp, App: "chk"})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	reg, _ := ReadRegistry(dir)
	if reg["vm-a"].CheckKey == nil || reg["vm-a"].CheckKey.Fingerprint != fp {
		t.Errorf("%+v", reg)
	}
	before := headCount(t, dir)
	if changed, err := WriteCheckKey(dir, "vm-a", CheckKey{Fingerprint: fp, App: "chk"}); err != nil || changed {
		t.Errorf("rewriting the same record must not commit: %v %v", changed, err)
	}
	if headCount(t, dir) != before {
		t.Error("a commit was made for an unchanged record")
	}
	if _, err := WriteCheckKey(dir, "vm-a", CheckKey{Fingerprint: "-----BEGIN OPENSSH PRIVATE KEY-----", App: "chk"}); err == nil {
		t.Error("key text must be refused")
	}
	if _, err := WriteCheckKey(dir, "../x", CheckKey{Fingerprint: fp, App: "chk"}); err == nil {
		t.Error("bad label must be refused")
	}
	if err := ClearCheckKey(dir, "vm-a"); err != nil {
		t.Fatal(err)
	}
	if reg, _ := ReadRegistry(dir); reg["vm-a"].CheckKey != nil {
		t.Error("check-key should be cleared")
	}
}
