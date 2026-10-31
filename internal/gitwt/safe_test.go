package gitwt

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

func TestSubcommandAt(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"status"}, 0},
		{[]string{"-c", "core.quotePath=false", "ls-files"}, 2},
		{[]string{"--git-dir=/x", "config", "k"}, 1},
		{[]string{"--git-dir", "/x", "fetch"}, 2},
		{[]string{"-C", "/x", "--no-pager", "log"}, 3},
		{[]string{"-c", "a=b"}, 2},
		{nil, 0},
	} {
		if got := subcommandAt(tc.args); got != tc.want {
			t.Errorf("subcommandAt(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

// A caller's leading -c cannot unpin: the pins come after the caller's options.
func TestCommand_PinsFollowTheCallersOptions(t *testing.T) {
	args := Command(Opts{Dir: "/r"}, "-c", "core.fsmonitor=/evil", "status").Args[1:]
	last := -1
	for i, a := range args {
		if a == "core.fsmonitor=/evil" {
			last = i
		}
	}
	pin := -1
	for i, a := range args {
		if a == "core.fsmonitor=false" {
			pin = i
		}
	}
	if last < 0 || pin < last {
		t.Errorf("the pin must come after the caller's -c: %q", args)
	}
	if args[0] != "-C" || args[1] != "/r" || args[len(args)-1] != "status" {
		t.Errorf("unexpected shape: %q", args)
	}
}

func TestCommand_FlagsForTheSubcommand(t *testing.T) {
	has := func(args []string, want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for sub, want := range map[string]string{
		"diff": "--no-ext-diff", "log": "--no-textconv", "show": "--no-textconv",
		"fetch": "--upload-pack=git-upload-pack", "pull": "--upload-pack=git-upload-pack",
		"ls-remote": "--upload-pack=git-upload-pack", "clone": "--upload-pack=git-upload-pack",
		"push": "--receive-pack=git-receive-pack",
	} {
		if a := Command(Opts{}, sub, "x").Args[1:]; !has(a, want) {
			t.Errorf("%s: no %s in %q", sub, want, a)
		}
	}
	if a := Command(Opts{}, "status").Args[1:]; has(a, "--no-ext-diff") || has(a, "--upload-pack=git-upload-pack") {
		t.Errorf("status got flags it has no use for: %q", a)
	}
	// A pack program the caller chose stays.
	if a := Command(Opts{}, "push", quietReceivePack, "origin").Args[1:]; has(a, "--receive-pack=git-receive-pack") {
		t.Errorf("the caller's --receive-pack was doubled: %q", a)
	}
	// Only network commands read the user's config.
	if a := Command(Opts{}, "status").Args[1:]; has(a, "credential.helper=") {
		t.Errorf("status was given credential pins: %q", a)
	}
}

// The inherited environment cannot inject config, an external diff, a pager or
// an editor, and the caller's own variables come last.
func TestCommand_EnvDropsInjectionVariables(t *testing.T) {
	for k, v := range map[string]string{
		"GIT_CONFIG_PARAMETERS": "'core.fsmonitor=/evil'", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.fsmonitor",
		"GIT_CONFIG_VALUE_0": "/evil", "GIT_EXTERNAL_DIFF": "/evil", "GIT_PAGER": "/evil", "GIT_EDITOR": "/evil", "GIT_DIR": "/nowhere",
	} {
		t.Setenv(k, v)
	}
	env := Command(Opts{Env: []string{"KEEP=1"}}, "status").Env
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, bad := range []string{"GIT_CONFIG_PARAMETERS=", "GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=", "GIT_EXTERNAL_DIFF=", "GIT_DIR=", "GIT_PAGER=/evil", "GIT_EDITOR=/evil"} {
		if strings.Contains(joined, "\n"+bad) {
			t.Errorf("%s was passed on", bad)
		}
	}
	for _, want := range []string{"GIT_PAGER=cat", "GIT_EDITOR=true", "KEEP=1"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("%s missing from the environment", want)
		}
	}
	if env[len(env)-1] != "KEEP=1" {
		t.Error("the caller's variables must come last")
	}
}

// memsync's own filter and merge driver are pinned by name: a repo whose config
// names another program for them still runs the one memsync pinned.
func TestSetConfigPins_DriversWinByName(t *testing.T) {
	gitIdent(t)
	c := newPinCanary(t)
	t.Cleanup(func() { SetConfigPins() })
	r := testRepo(t)
	cat := filepath.Join(c.dir, "pinned-clean.sh")
	if err := os.WriteFile(cat, []byte("#!/bin/sh\necho filterPinned >> "+c.log+"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	mdrv := filepath.Join(c.dir, "pinned-merge.sh")
	if err := os.WriteFile(mdrv, []byte("#!/bin/sh\necho mergePinned >> "+c.log+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	write(t, r, "a.md", []byte("base\n"))
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("commit", "-q", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	rawGit(t, r.Dir, "checkout", "-q", "-b", "side")
	write(t, r, "a.md", []byte("side\n"))
	rawGit(t, r.Dir, "commit", "-qam", "side")
	rawGit(t, r.Dir, "checkout", "-q", "main")
	write(t, r, "a.md", []byte("main\n"))
	rawGit(t, r.Dir, "commit", "-qam", "main")
	// From here the repo binds both drivers to canaries; memsync pins its own.
	if err := os.MkdirAll(filepath.Join(r.Dir, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Dir, ".git", "info", "attributes"), []byte("* filter=memage merge=memtext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, r.Dir, "config", "filter.memage.clean", c.script("filterRepo"))
	rawGit(t, r.Dir, "config", "merge.memtext.driver", c.script("mergeRepo")+" %O %A %B")
	SetConfigPins("filter.memage.clean="+cat, "merge.memtext.driver="+mdrv+" %O %A %B")
	r.Run("merge", "side") // conflicts: the pinned driver says so
	rawGit(t, r.Dir, "merge", "--abort")
	write(t, r, "a.md", []byte("again\n"))
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.ran(), " ")
	for _, want := range []string{"filterPinned", "mergePinned"} {
		if !strings.Contains(got, want) {
			t.Errorf("the pinned %s did not run (ran: %s)", want, got)
		}
	}
	if strings.Contains(got, "Repo") {
		t.Errorf("the repo's own driver ran: %s", got)
	}
}

// The reviewed pin set. Changing it is a security decision: update this list
// with the reason.
func TestBasePins_ArePinned(t *testing.T) {
	want := []string{
		"core.fsmonitor=false", "core.hooksPath=" + os.DevNull, "core.pager=cat", "core.alternateRefsCommand=",
		"commit.gpgSign=false", "tag.gpgSign=false", "push.gpgSign=false", "log.showSignature=false", "diff.external=",
		"submodule.recurse=false", "fetch.recurseSubmodules=false", "push.recurseSubmodules=no",
		"protocol.allow=never", "protocol.ssh.allow=always", "protocol.https.allow=always", "protocol.file.allow=always",
		"protocol.ext.allow=never", "protocol.fd.allow=never",
	}
	args := Command(Opts{}, "status").Args[1:]
	if args[0] != "--no-pager" {
		t.Errorf("--no-pager missing: %q", args)
	}
	for _, w := range want {
		found := false
		for i, a := range args {
			if a == w && i > 0 && args[i-1] == "-c" {
				found = true
			}
		}
		if !found {
			t.Errorf("-c %s is not pinned: %q", w, args)
		}
	}
	found := false
	for i, a := range args {
		if a == "core.askpass=" && args[i-1] == "-c" {
			found = true
		}
	}
	if !found {
		t.Error("core.askpass= is not pinned on a local command")
	}
}

func TestIsNetwork(t *testing.T) {
	for _, s := range []string{"clone", "fetch", "pull", "push", "ls-remote"} {
		if !isNetwork(s) {
			t.Errorf("%s is a network command", s)
		}
	}
	for _, s := range []string{"status", "commit", "diff", "config", "rebase", ""} {
		if isNetwork(s) {
			t.Errorf("%s is not a network command", s)
		}
	}
}

// `git config` shows the repo's own values: a pin must not answer for a key it
// pins (the upgrade check reads filter.memage.process this way).
func TestCommand_ConfigReadsSeeTheRepoNotThePins(t *testing.T) {
	t.Cleanup(func() { SetConfigPins() })
	SetConfigPins("filter.memage.process=/pinned")
	r := testRepo(t)
	for k, v := range map[string]string{"core.hooksPath": "/x/hooks", "filter.memage.process": "/repo-value", "core.fsmonitor": "/mon"} {
		if _, err := r.Run("config", k, v); err != nil {
			t.Fatal(err)
		}
		if got, err := r.Run("config", "--get", k); err != nil || strings.TrimSpace(got) != v {
			t.Errorf("config --get %s = %q, %v; want %q", k, got, err, v)
		}
	}
}

// The system config's credential helper is the user's too, and runs.
func TestNetworkUsesSystemCredentialHelper(t *testing.T) {
	gitIdent(t)
	c := newPinCanary(t)
	r := testRepo(t)
	system := filepath.Join(t.TempDir(), "gitconfig-system")
	if err := os.WriteFile(system, []byte("[credential]\n\thelper = !"+c.script("systemCred")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	rawGit(t, r.Dir, "remote", "add", "web", srv.URL+"/none.git")
	// A repo helper makes the repo able to shadow the user's, so the pins put
	// the user's global and system helpers back.
	rawGit(t, r.Dir, "config", "credential.helper", "!"+c.script("repoCred"))
	r.Run("ls-remote", "web")
	if got := strings.Join(c.ran(), " "); strings.Contains(got, "repoCred") {
		t.Errorf("the repo's helper ran: %s", got)
	}
	if got := strings.Join(c.ran(), " "); !strings.Contains(got, "systemCred") {
		t.Errorf("the system credential helper did not run (ran: %s)", got)
	}
}

// A repo whose config names no ssh command, credential helper or askpass cannot
// shadow the user's, so a network command reads the user's config not at all
// (no extra git process); one that does gets the user's put back.
func TestCommand_NetworkPinsOnlyWhenTheRepoConfigCouldShadow(t *testing.T) {
	gitIdent(t)
	r := testRepo(t)
	var spawns int
	restore := proc.SetSpawnHook(func(string, []string) { spawns++ })
	defer restore()
	plain := strings.Join(Command(Opts{Dir: r.Dir}, "fetch", "origin").Args[1:], " ")
	if spawns != 0 || strings.Contains(plain, "credential.helper=") {
		t.Errorf("a plain repo cost %d spawns or got credential pins: %s", spawns, plain)
	}
	for _, key := range []string{"core.sshCommand", "credential.helper", "core.askpass", "include.path", "includeIf.gitdir:/x/.path"} {
		rawGit(t, r.Dir, "config", key, "x")
		if got := strings.Join(Command(Opts{Dir: r.Dir}, "fetch", "origin").Args[1:], " "); !strings.Contains(got, "credential.helper=") {
			t.Errorf("%s set in the repo but the user's config was not put back: %s", key, got)
		}
		rawGit(t, r.Dir, "config", "--unset", key)
	}
	if got := strings.Join(Command(Opts{}, "fetch", "origin").Args[1:], " "); !strings.Contains(got, "credential.helper=") {
		t.Error("with no repo placed, the user's config must be put back")
	}
}

// The scan fails closed: a git dir it cannot fully place, or whose config git
// would take from elsewhere (commondir), or that holds any key which can
// redirect or run something on the way to a remote, gets the user's config put
// back by the pins.
func TestLocalConfigMayShadow_FailsClosed(t *testing.T) {
	mk := func(t *testing.T, files map[string]string) string {
		t.Helper()
		d := t.TempDir()
		g := filepath.Join(d, ".git")
		if err := os.MkdirAll(g, 0o700); err != nil {
			t.Fatal(err)
		}
		for n, c := range files {
			if err := os.WriteFile(filepath.Join(g, n), []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	plain := map[string]string{"HEAD": "ref: refs/heads/main\n", "config": "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = /x/y.git\n"}
	if d := mk(t, plain); localConfigMayShadow(d, nil, "fetch") {
		t.Error("a plain repo config cannot shadow")
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range plain {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, files := range map[string]map[string]string{
		"no config file":          {"HEAD": "ref: refs/heads/main\n"},
		"commondir, no config":    {"HEAD": "x\n", "commondir": "../.."},
		"commondir with a config": with(map[string]string{"commondir": "../.."}),
		"no HEAD":                 {"config": plain["config"]},
		"includeIf":               with(map[string]string{"config": plain["config"] + "[includeIf \"gitdir:/x/\"]\n\tpath = /evil\n"}),
		"insteadOf":               with(map[string]string{"config": plain["config"] + "[url \"ext::sh -c x\"]\n\tinsteadOf = /x\n"}),
		"pushInsteadOf":           with(map[string]string{"config": plain["config"] + "[url \"ssh://h/\"]\n\tpushInsteadOf = /x\n"}),
		"gitProxy":                with(map[string]string{"config": plain["config"] + "[core]\n\tgitProxy = /evil\n"}),
		"protocol":                with(map[string]string{"config": plain["config"] + "[protocol \"ext\"]\n\tallow = always\n"}),
		"uploadpack":              with(map[string]string{"config": plain["config"] + "[remote \"origin\"]\n\tuploadpack = /evil\n"}),
		"receivepack":             with(map[string]string{"config": plain["config"] + "[remote \"origin\"]\n\treceivepack = /evil\n"}),
	} {
		if !localConfigMayShadow(mk(t, files), nil, "fetch") {
			t.Errorf("%s: the scan must say may-shadow", name)
		}
	}
	// A bare repo given as --git-dir is read the same way.
	bare := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	if !localConfigMayShadow("", []string{"--git-dir=" + bare}, "fetch") {
		t.Error("an empty git dir must fail closed")
	}
}

// A .git that is a file (a worktree or submodule) is not placed: fail closed.
func TestLocalConfigMayShadow_GitFileFailsClosed(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !localConfigMayShadow(d, nil, "fetch") {
		t.Error("a gitfile must fail closed")
	}
	if !localConfigMayShadow(filepath.Join(d, "missing"), nil, "fetch") {
		t.Error("a directory without a .git must fail closed")
	}
}
