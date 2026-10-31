package gitwt

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// pinCanary makes a script per key; each appends its key to one file when git
// runs it. ran lists the keys that ran, sorted.
type pinCanary struct {
	t   *testing.T
	dir string
	log string
}

func newPinCanary(t *testing.T) *pinCanary {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the canaries are shell scripts")
	}
	d := t.TempDir()
	return &pinCanary{t: t, dir: d, log: filepath.Join(d, "ran")}
}

// script is an executable that records key, prints nothing and succeeds.
func (c *pinCanary) script(key string) string {
	p := filepath.Join(c.dir, "canary-"+key+".sh")
	body := "#!/bin/sh\necho " + key + " >> " + c.log + "\n"
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		c.t.Fatal(err)
	}
	return p
}

func (c *pinCanary) ran() []string {
	b, _ := os.ReadFile(c.log)
	seen := map[string]bool{}
	for _, l := range strings.Fields(string(b)) {
		seen[l] = true
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func rawGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// Every command-capable key a repo's own .git/config can set is set to a
// canary, after the commits, and the calls memsync makes (status, add, commit,
// diff, log, show, checkout, fetch, ls-remote, push, an ext:: remote) must run
// none of them. The pins live in one helper that every call goes
// through.
func TestRepoConfigCannotRunPrograms(t *testing.T) {
	gitIdent(t)
	c := newPinCanary(t)
	r := testRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	rawGit(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", origin)
	write(t, r, ".gitattributes", []byte("* diff=c\n"))
	write(t, r, "a.md", []byte("one\n"))
	rawGit(t, r.Dir, "add", "-A")
	rawGit(t, r.Dir, "commit", "-q", "-m", "first")
	rawGit(t, r.Dir, "remote", "add", "origin", origin)
	rawGit(t, r.Dir, "push", "-q", "origin", "main")

	hooks := t.TempDir()
	for _, h := range []string{"pre-commit", "post-commit", "post-checkout", "reference-transaction", "post-index-change", "pre-push", "fsmonitor-watchman"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\necho hooksPath >> "+c.log+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alt := t.TempDir()
	if err := os.WriteFile(filepath.Join(r.Dir, ".git", "objects", "info", "alternates"), []byte(alt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"core.fsmonitor": c.script("fsmonitor"), "core.hooksPath": hooks, "core.pager": c.script("pager"),
		"core.askpass": c.script("askpass"), "core.alternateRefsCommand": c.script("alternateRefs"),
		"core.sshCommand": c.script("sshCommand"), "diff.external": c.script("diffExternal"),
		"diff.c.textconv": c.script("textconv"), "diff.c.command": c.script("diffCommand"),
		"commit.gpgSign": "true", "gpg.program": c.script("gpg"), "tag.gpgSign": "true",
		"remote.origin.uploadpack": c.script("uploadpack"), "remote.origin.receivepack": c.script("receivepack"),
		"credential.helper": "!" + c.script("credential"),
		"remote.ext.url":    "ext::" + c.script("protocolExt"),
	} {
		rawGit(t, r.Dir, "config", k, v)
	}

	step := func(args ...string) {
		t.Helper()
		if _, err := r.Run(args...); err != nil {
			t.Errorf("%v failed: %v", args, err)
		}
	}
	step("status", "--porcelain")
	write(t, r, "a.md", []byte("two\n"))
	step("add", "-A")
	step("diff", "--cached")
	step("commit", "-q", "-m", "second")
	step("diff", "HEAD~1", "HEAD") // diff.external
	rawGit(t, r.Dir, "config", "--unset", "diff.external")
	step("diff", "HEAD~1", "HEAD") // textconv
	step("log", "-p", "-1")
	step("show", "HEAD")
	step("checkout", "HEAD", "--", "a.md")
	step("fetch", "origin")
	step("ls-remote", "origin")
	step("push", "origin", "main")
	if _, err := r.Run("fetch", "ext"); err == nil {
		t.Error("an ext:: remote was fetched")
	}
	if got := c.ran(); len(got) != 0 {
		t.Errorf("repo-local config ran programs: %v", got)
	}
}

// A program set through an included config file is no safer than one set in
// .git/config: the pin, applied after includes, wins.
func TestIncludedConfigCannotRunFsmonitor(t *testing.T) {
	gitIdent(t)
	c := newPinCanary(t)
	r := testRepo(t)
	write(t, r, "a.md", []byte("one\n"))
	inc := filepath.Join(t.TempDir(), "included.cfg")
	if err := os.WriteFile(inc, []byte("[core]\n\tfsmonitor = "+c.script("include")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, r.Dir, "config", "include.path", inc)
	if _, err := r.Run("status", "--porcelain"); err != nil {
		t.Error(err)
	}
	if _, err := r.Run("add", "-A"); err != nil {
		t.Error(err)
	}
	if got := c.ran(); len(got) != 0 {
		t.Errorf("an included config ran programs: %v", got)
	}
}

// ssh and credential helpers come from the user's global and system config,
// never from the repo: a repo-local core.sshCommand and credential.helper lose
// to the global ones, which still run (the pin must not drop the user's own).
func TestNetworkUsesGlobalSSHAndCredentialsOnly(t *testing.T) {
	gitIdent(t)
	c := newPinCanary(t)
	r := testRepo(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	cfg := "[core]\n\tsshCommand = " + c.script("globalSSH") + "\n[credential]\n\thelper = !" + c.script("globalCred") + "\n"
	if err := os.WriteFile(global, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_SSH_COMMAND", "")
	os.Unsetenv("GIT_SSH_COMMAND")
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	srv.TLS = &tls.Config{}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	rawGit(t, r.Dir, "config", "core.sshCommand", c.script("localSSH"))
	rawGit(t, r.Dir, "config", "credential.helper", "!"+c.script("localCred"))
	rawGit(t, r.Dir, "remote", "add", "sshr", "ssh://u@127.0.0.1/none.git")
	rawGit(t, r.Dir, "remote", "add", "web", srv.URL+"/none.git")
	r.Run("ls-remote", "sshr")
	r.Run("ls-remote", "web")
	got := strings.Join(c.ran(), " ")
	for _, bad := range []string{"localSSH", "localCred"} {
		if strings.Contains(got, bad) {
			t.Errorf("the repo's own %s ran (ran: %s)", bad, got)
		}
	}
	for _, want := range []string{"globalSSH", "globalCred"} {
		if !strings.Contains(got, want) {
			t.Errorf("the user's global %s did not run (ran: %s)", want, got)
		}
	}
}
