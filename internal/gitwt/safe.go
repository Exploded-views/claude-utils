package gitwt

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// This file is the one place memsync starts git. A repo's own .git/config (and
// anything it includes) can name programs git will run: an fsmonitor, a hook
// directory, a pager, an external diff or textconv, a clean/smudge filter, a
// credential helper, an ssh command, a gpg program, an upload-pack. Only the
// local user can write that file, but the daemon and the hook run git in
// whatever repo the config names, on every prompt, so every call pins each key
// it could hit. A syntax test (TestNoGitExecOutsideTheHelper) fails on any
// other file that spawns git, so a new call site cannot miss the pins.
//
// A `-c` pin beats the repo's config and an included file's. It does not beat
// remote.<n>.uploadpack or receivepack (the first value wins there), so those
// are pinned with the command-line flags. Keys that name an arbitrary driver
// (filter.<name>.*) cannot be listed; memsync's own drivers are pinned by name
// (SetConfigPins) and info/attributes binds every file to them.

// basePins are the pins for every command. Order matters only in that all of
// them sit after the caller's own leading options, so a caller cannot unpin.
var basePins = []string{
	"--no-pager",
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.pager=cat",
	"-c", "core.alternateRefsCommand=",
	"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "push.gpgSign=false", "-c", "log.showSignature=false",
	"-c", "diff.external=",
	"-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false", "-c", "push.recurseSubmodules=no",
	"-c", "protocol.allow=never",
	"-c", "protocol.ssh.allow=always", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always",
	"-c", "protocol.ext.allow=never", "-c", "protocol.fd.allow=never",
}

// Opts says how Command starts git.
type Opts struct {
	Dir             string          // run as `git -C Dir`; "" runs in the process's directory
	Env             []string        // extra KEY=VALUE, applied last
	Ctx             context.Context // if set, cancelling it kills git's whole process group
	KeepMaintenance bool            // leave gc.auto and maintenance.auto alone (the explicit `gc --auto`)
	DropGitEnv      bool            // drop every inherited GIT_* variable but GIT_EXEC_PATH
}

var configPins atomic.Pointer[[]string]

// SetConfigPins sets "key=value" settings that every later git command gets as
// `-c key=value`: memsync's own filter, textconv and merge drivers, from the
// running build, so a repo's config cannot swap them for another program.
func SetConfigPins(kv ...string) {
	c := append([]string(nil), kv...)
	configPins.Store(&c)
}

// Command builds a git command with every pin applied. It is the only way
// memsync starts git. Callers run it with proc.Run, or as an *exec.Cmd.
func Command(o Opts, args ...string) *exec.Cmd {
	sub := subcommandAt(args)
	name := ""
	if sub < len(args) {
		name = args[sub]
	}
	var argv []string
	if o.Dir != "" {
		argv = append(argv, "-C", o.Dir)
	}
	argv = append(argv, args[:sub]...)
	// `git config` reads and writes the settings themselves and starts no
	// program: a -c pin there would answer for the file (a read of a pinned key
	// would return the pin, so the upgrade check would never see the repo's
	// stale driver path). It gets no -c pins.
	if name == "config" {
		argv = append(argv, "--no-pager")
	} else {
		argv = append(argv, basePins...)
		if !o.KeepMaintenance {
			argv = append(argv, NoAutoMaintenance...)
		}
		if p := configPins.Load(); p != nil {
			for _, kv := range *p {
				argv = append(argv, "-c", kv)
			}
		}
	}
	env := gitEnv(o.DropGitEnv)
	if isNetwork(name) && localConfigMayShadow(o.Dir, args[:sub], name) {
		u := userNetConfig()
		argv = append(argv, "-c", "credential.helper=")
		for _, h := range u.helpers {
			argv = append(argv, "-c", "credential.helper="+h)
		}
		argv = append(argv, "-c", "core.askpass="+u.askpass)
		env = append(env, "GIT_SSH_COMMAND="+u.sshCommand())
	} else if name != "config" {
		argv = append(argv, "-c", "core.askpass=")
	}
	if sub < len(args) {
		argv = append(argv, name)
		argv = append(argv, subcommandFlags(name, args[sub+1:])...)
		argv = append(argv, args[sub+1:]...)
	}
	env = append(env, "GIT_PAGER=cat", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
	env = append(env, o.Env...)
	var cmd *exec.Cmd
	if o.Ctx != nil {
		cmd = proc.GroupedCommandContext(o.Ctx, "git", argv...)
	} else {
		cmd = proc.Command("git", argv...)
	}
	cmd.Env = env
	return cmd
}

// subcommandAt is the index of the subcommand in args: the first word that is
// not a global option or an option's value. len(args) when there is none.
func subcommandAt(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-c", "-C", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--exec-path":
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return i
	}
	return len(args)
}

func isNetwork(sub string) bool {
	switch sub {
	case "clone", "fetch", "pull", "push", "ls-remote":
		return true
	}
	return false
}

// subcommandFlags are the flags put right after the subcommand: the ones that
// stop a diff from running an external diff or textconv, and the ones that
// name the upload-pack and receive-pack (a `-c` does not beat the repo's
// remote.<n>.uploadpack or receivepack). A flag the caller already gave stays.
func subcommandFlags(sub string, rest []string) []string {
	has := func(prefix string) bool {
		for _, a := range rest {
			if a == "--" {
				return false
			}
			if strings.HasPrefix(a, prefix) {
				return true
			}
		}
		return false
	}
	switch sub {
	case "diff", "log", "show":
		return []string{"--no-ext-diff", "--no-textconv"}
	case "clone", "fetch", "pull", "ls-remote":
		if has("--upload-pack") {
			return nil
		}
		return []string{"--upload-pack=git-upload-pack"}
	case "push":
		if has("--receive-pack") || has("--exec") {
			return nil
		}
		return []string{"--receive-pack=git-receive-pack"}
	}
	return nil
}

// gitEnv is the environment for git: the caller's, minus what would redirect
// git to another repository than the one asked for, inject config
// (GIT_CONFIG_*), or name a program to run for a diff, a pager or an editor.
func gitEnv(dropAll bool) []string {
	var env []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		uk := strings.ToUpper(k)
		if dropAll && strings.HasPrefix(uk, "GIT_") && uk != "GIT_EXEC_PATH" {
			continue
		}
		switch uk {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX",
			"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
			"GIT_EXTERNAL_DIFF", "GIT_PAGER", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR":
			continue
		}
		if strings.HasPrefix(uk, "GIT_CONFIG_KEY_") || strings.HasPrefix(uk, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, e)
	}
	return env
}

// userNet is what a network command takes from the user's own (global and
// system) config instead of the repo's.
type userNet struct {
	helpers []string
	askpass string
	ssh     string
}

// sshCommand is the ssh command git should use: GIT_SSH_COMMAND if set, else
// the user's core.sshCommand, else GIT_SSH, else ssh. Never the repo's.
func (u userNet) sshCommand() string {
	if v := os.Getenv("GIT_SSH_COMMAND"); v != "" {
		return v
	}
	if u.ssh != "" {
		return u.ssh
	}
	if v := os.Getenv("GIT_SSH"); v != "" {
		return "'" + strings.ReplaceAll(filepath.ToSlash(v), "'", `'\''`) + "'"
	}
	return "ssh"
}

// SSHCommand is the ssh command a network git command will use, from the
// environment and the user's own git config, never a repo's.
func SSHCommand() string {
	if v := os.Getenv("GIT_SSH_COMMAND"); v != "" {
		return v // no git process to ask
	}
	return userNetConfig().sshCommand()
}

// localConfigMayShadow reports whether the repo a network command runs in could
// name its own ssh command, credential helper or askpass, so that the user's
// own must be put back by the pins. It reads the repo's config file (no git
// process); anything it cannot place or that includes another file counts as
// "may". A clone has no repo yet.
func localConfigMayShadow(dir string, lead []string, sub string) bool {
	if sub == "clone" {
		return false
	}
	gitDir := ""
	for i, a := range lead {
		if strings.HasPrefix(a, "--git-dir=") {
			gitDir = strings.TrimPrefix(a, "--git-dir=")
		} else if a == "--git-dir" && i+1 < len(lead) {
			gitDir = lead[i+1]
		}
	}
	if gitDir == "" {
		if dir == "" {
			return true
		}
		gitDir = filepath.Join(dir, ".git")
	}
	if st, err := os.Stat(gitDir); err != nil || !st.IsDir() {
		return true // a gitfile (worktree, submodule), or not placed
	}
	// Fail closed: git takes the config of another git dir when commondir
	// names one (any git dir, not only a worktree's), and a directory without
	// HEAD or without a config is not one this scan understands.
	if _, err := os.Stat(filepath.Join(gitDir, "commondir")); err == nil || !os.IsNotExist(err) {
		return true
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		return true
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return true
	}
	t := strings.ToLower(string(b))
	// Keys that can run a program, or redirect or re-route a remote, whether
	// or not the -c pins already win over them (the pins are unconditional;
	// this only decides whether the user's own helpers and ssh command are put
	// back): ssh command, credential, askpass, include and includeIf, url
	// rewrites (insteadOf, pushInsteadOf), core.gitProxy, protocol.*, the
	// remote's uploadpack and receivepack, ssh.variant, worktree config.
	for _, k := range []string{"sshcommand", "credential", "askpass", "include", "worktreeconfig", "ssh.variant",
		"insteadof", "gitproxy", "[protocol", "uploadpack", "receivepack"} {
		if strings.Contains(t, k) {
			return true
		}
	}
	return false
}

var (
	userNetMu    sync.Mutex
	userNetCache = map[string]userNet{}
)

// userNetConfig reads core.sshCommand, core.askpass and credential.helper from
// the user's global then system config, once per distinct config location.
func userNetConfig() userNet {
	key := strings.Join([]string{os.Getenv("HOME"), os.Getenv("XDG_CONFIG_HOME"), os.Getenv("GIT_CONFIG_GLOBAL"),
		os.Getenv("GIT_CONFIG_SYSTEM"), os.Getenv("GIT_CONFIG_NOSYSTEM"), os.Getenv("USERPROFILE")}, "\x00")
	userNetMu.Lock()
	defer userNetMu.Unlock()
	if u, ok := userNetCache[key]; ok {
		return u
	}
	var u userNet
	// System first, then global: git reads them in that order and the last
	// single value wins; helpers accumulate in that order.
	for _, scope := range []string{"--system", "--global"} {
		cmd := proc.Command("git", "--no-pager", "-c", "core.fsmonitor=false", "config", scope, "--null",
			"--get-regexp", `^(core\.sshcommand|core\.askpass|credential\.helper)$`)
		cmd.Env = append(gitEnv(false), "GIT_PAGER=cat")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := proc.Run(cmd); err != nil && out.Len() == 0 {
			continue // no such file, or no such key
		}
		for _, rec := range strings.Split(out.String(), "\x00") {
			k, v, _ := strings.Cut(rec, "\n")
			switch strings.ToLower(k) {
			case "core.sshcommand":
				u.ssh = v
			case "core.askpass":
				u.askpass = v
			case "credential.helper":
				if v == "" {
					u.helpers = nil // an empty value resets the list
				} else {
					u.helpers = append(u.helpers, v)
				}
			}
		}
	}
	userNetCache[key] = u
	return u
}
