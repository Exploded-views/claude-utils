//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
)

// idFile writes an age identity file (mode 0600) and returns its path, its
// secret line, and its public recipient.
func idFile(t *testing.T, dir, name string, pq bool) (path, secret, recipient string) {
	t.Helper()
	if pq {
		id, err := age.GenerateHybridIdentity()
		if err != nil {
			t.Fatal(err)
		}
		secret, recipient = id.String(), id.Recipient().String()
	} else {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		secret, recipient = id.String(), id.Recipient().String()
	}
	path = filepath.Join(dir, name)
	body := "# created: test\n# public key: " + recipient + "\n" + secret + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, secret, recipient
}

// fakeStores writes a store script and a verify script. The store script
// records its argv, its environment and its stdin under rec, one set of files
// per run; the verify script prints the bytes the last run stored.
func fakeStores(t *testing.T) (storeCmd, verifyCmd, rec string) {
	t.Helper()
	d := t.TempDir()
	rec = filepath.Join(d, "rec")
	if err := os.Mkdir(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(d, "store.sh")
	verify := filepath.Join(d, "verify.sh")
	os.WriteFile(store, []byte(`#!/bin/sh
rec="$1"; shift
printf '%s\n' "$0" "$@" > "$rec/argv.$$"
env > "$rec/env.$$"
cat > "$rec/stdin.$$"
cp "$rec/stdin.$$" "$rec/stored"
if [ -n "$1" ]; then cp "$rec/stdin.$$" "$rec/item.$1"; fi
`), 0o700)
	os.WriteFile(verify, []byte("#!/bin/sh\nif [ -n \"$2\" ]; then cat \"$1/item.$2\"; else cat \"$1/stored\"; fi\n"), 0o700)
	return store + " " + rec, verify + " " + rec, rec
}

func recorded(t *testing.T, rec, prefix string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(rec, prefix+".*"))
	var out []string
	for _, f := range m {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// runIDBackup runs the command with the process hardening stubbed out, so the
// test binary is not left undumpable. runIDBackupHarden names the stub.
func runIDBackup(args ...string) (int, string, string) {
	return runIDBackupHarden(func() {}, args...)
}

func runIDBackupHarden(harden func(), args ...string) (int, string, string) {
	old := hardenFn
	hardenFn = harden
	defer func() { hardenFn = old }()
	var o, e bytes.Buffer
	code := identityBackupCmd(&o, &e, args)
	return code, o.String(), e.String()
}

func TestIdentityBackup_SecretOnlyInStdin(t *testing.T) {
	for _, pq := range []bool{false, true} {
		name := "classic"
		if pq {
			name = "pq"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path, secret, rcpt := idFile(t, dir, "identity.txt", pq)
			want, _ := os.ReadFile(path)
			store, verify, rec := fakeStores(t)
			code, out, errs := runIDBackup("--identity", path, "--store-cmd", store, "--verify-cmd", verify)
			if code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
			}
			if strings.Contains(out, secret) || strings.Contains(errs, secret) {
				t.Fatal("the secret was printed")
			}
			if strings.Contains(out, "AGE-SECRET-KEY") || strings.Contains(errs, "AGE-SECRET-KEY") {
				t.Fatal("key material was printed")
			}
			for _, k := range []string{"argv", "env"} {
				got := recorded(t, rec, k)
				if len(got) != 1 {
					t.Fatalf("%s recorded %d times", k, len(got))
				}
				if strings.Contains(got[0], secret) || strings.Contains(got[0], "AGE-SECRET-KEY") {
					t.Fatalf("the secret is in the child's %s", k)
				}
			}
			in := recorded(t, rec, "stdin")
			if len(in) != 1 || in[0] != string(want) {
				t.Fatalf("stdin = %q, want the identity file's bytes", in)
			}
			fp := agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(rcpt))
			for _, s := range []string{path, fp, "verified (sha256 matches)"} {
				if !strings.Contains(out, s) {
					t.Errorf("stdout lacks %q:\n%s", s, out)
				}
			}
			if !strings.Contains(errs, "waiting for your password manager") {
				t.Errorf("no wait line on stderr: %q", errs)
			}
		})
	}
}

func TestIdentityBackup_StoreFailureIsExit1(t *testing.T) {
	path, secret, _ := idFile(t, t.TempDir(), "identity.txt", false)
	fail := filepath.Join(t.TempDir(), "fail.sh")
	os.WriteFile(fail, []byte("#!/bin/sh\ncat >/dev/null\necho \"$AGE_X\" >&2\nexit 3\n"), 0o700)
	code, out, errs := runIDBackup("--identity", path, "--store-cmd", fail)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out+errs, "exit status 3") {
		t.Errorf("message does not say the store failed: %q %q", out, errs)
	}
	if strings.Contains(out+errs, secret) {
		t.Fatal("secret printed")
	}
}

func TestIdentityBackup_StoreTimeoutIsExit1(t *testing.T) {
	path, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	slow := filepath.Join(t.TempDir(), "slow.sh")
	os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 10\n"), 0o700)
	t.Setenv("CLAUDE_MEMSYNC_SECRET_TIMEOUT", "300ms")
	start := time.Now()
	code, out, errs := runIDBackup("--identity", path, "--store-cmd", slow)
	if code != 1 || !strings.Contains(out+errs, "timed out") {
		t.Fatalf("exit %d, out %q err %q: want 1 and a timeout message", code, out, errs)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestIdentityBackup_VerifyMismatchIsExit1(t *testing.T) {
	path, secret, _ := idFile(t, t.TempDir(), "identity.txt", false)
	store, _, _ := fakeStores(t)
	wrong := filepath.Join(t.TempDir(), "wrong.sh")
	os.WriteFile(wrong, []byte("#!/bin/sh\necho not-the-key\n"), 0o700)
	code, out, errs := runIDBackup("--identity", path, "--store-cmd", store, "--verify-cmd", wrong)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "MISMATCH") {
		t.Errorf("stdout lacks MISMATCH: %q", out)
	}
	if strings.Contains(out, "verified (sha256") || strings.Contains(out+errs, secret) || strings.Contains(out+errs, "not-the-key") {
		t.Errorf("unexpected output: %q %q", out, errs)
	}
}

func TestIdentityBackup_VerifyCommandFailureIsExit1(t *testing.T) {
	path, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	store, _, _ := fakeStores(t)
	code, out, errs := runIDBackup("--identity", path, "--store-cmd", store, "--verify-cmd", "false")
	if code != 1 || strings.Contains(out, "verified (sha256") {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
}

func TestIdentityBackup_WithoutVerifyWarns(t *testing.T) {
	path, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	store, _, _ := fakeStores(t)
	code, out, errs := runIDBackup("--identity", path, "--store-cmd", store)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if !strings.Contains(errs, "UNVERIFIED") || strings.Contains(out, "verified (sha256") {
		t.Errorf("no unverified warning: out %q err %q", out, errs)
	}
}

func TestIdentityBackup_RefusesUnsafeFiles(t *testing.T) {
	mk := map[string]func(t *testing.T, dir string) string{
		"group-readable": func(t *testing.T, dir string) string {
			p, _, _ := idFile(t, dir, "id", false)
			os.Chmod(p, 0o640)
			return p
		},
		"world-readable": func(t *testing.T, dir string) string {
			p, _, _ := idFile(t, dir, "id", false)
			os.Chmod(p, 0o644)
			return p
		},
		"executable": func(t *testing.T, dir string) string {
			p, _, _ := idFile(t, dir, "id", false)
			os.Chmod(p, 0o700)
			return p
		},
		"symlink": func(t *testing.T, dir string) string {
			p, _, _ := idFile(t, dir, "real", false)
			l := filepath.Join(dir, "link")
			if err := os.Symlink(p, l); err != nil {
				t.Fatal(err)
			}
			return l
		},
		"hard-linked": func(t *testing.T, dir string) string {
			p, _, _ := idFile(t, dir, "id", false)
			if err := os.Link(p, p+".2"); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"not-an-identity": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "id")
			os.WriteFile(p, []byte("hunter2 is not an age key\n"), 0o600)
			return p
		},
		"a-directory": func(t *testing.T, dir string) string { return dir },
		"missing":     func(t *testing.T, dir string) string { return filepath.Join(dir, "nope") },
	}
	for name, f := range mk {
		t.Run(name, func(t *testing.T) {
			p := f(t, t.TempDir())
			store, _, rec := fakeStores(t)
			code, out, errs := runIDBackup("--identity", p, "--store-cmd", store)
			if code != 1 {
				t.Fatalf("exit %d, want 1: %s %s", code, out, errs)
			}
			if got := recorded(t, rec, "stdin"); len(got) != 0 {
				t.Fatal("the store command ran on an unsafe file")
			}
			if strings.Contains(out+errs, "hunter2") {
				t.Fatal("file contents printed")
			}
		})
	}
}

func TestIdentityBackup_DryRunRunsNothing(t *testing.T) {
	path, secret, rcpt := idFile(t, t.TempDir(), "identity.txt", true)
	store, verify, rec := fakeStores(t)
	code, out, errs := runIDBackup("--dry-run", "--identity", path, "--store-cmd", store, "--verify-cmd", verify)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	for _, k := range []string{"argv", "env", "stdin", "stored"} {
		if got, _ := filepath.Glob(filepath.Join(rec, k+"*")); len(got) != 0 {
			t.Fatalf("dry run ran the store command (%s)", k)
		}
	}
	fp := agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(rcpt))
	for _, s := range []string{path, fp, "dry run", filepath.Join(filepath.Dir(rec), "store.sh")} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out+errs, secret) {
		t.Fatal("secret printed")
	}
	// an unsafe file is still reported by a dry run
	os.Chmod(path, 0o644)
	if code, _, _ := runIDBackup("--dry-run", "--identity", path, "--store-cmd", store); code != 1 {
		t.Fatalf("dry run accepted a 0644 file (exit %d)", code)
	}
}

func safeFP(rcpt string) string {
	return strings.ReplaceAll(agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(rcpt)), " ", "-")
}

func setCloneIdentity(t *testing.T, syncDir, id string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", syncDir, "config", "memage.identity", id).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestIdentityBackup_AllDeduplicatesSharedIdentities(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects", "client"}, "personal", nil)
	dir := t.TempDir()
	a, _, _ := idFile(t, dir, "a.txt", false)
	b, _, _ := idFile(t, dir, "b.txt", true)
	for name, id := range map[string]string{"personal": a, "projects": a, "client": b} {
		setCloneIdentity(t, cfgs[name].SyncDir, id)
	}
	store, verify, rec := fakeStores(t)
	store += " {fingerprint}"
	verify += " {fingerprint}"
	code, out, errs := runIDBackup("--all", "--store-cmd", store, "--verify-cmd", verify)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if n := len(recorded(t, rec, "stdin")); n != 2 {
		t.Fatalf("store ran %d times, want 2 (a shared identity is stored once)", n)
	}
	if strings.Count(out, a) != 1 || strings.Count(out, b) != 1 {
		t.Errorf("each identity must be listed once:\n%s", out)
	}
	// the default is every routed clone too
	if code, _, _ := runIDBackup("--store-cmd", store); code != 0 {
		t.Fatalf("default run exit %d", code)
	}
	if n := len(recorded(t, rec, "stdin")); n != 4 {
		t.Fatalf("default run stored %d in total, want 4", n)
	}
	// --repo names one clone
	if code, _, errs := runIDBackup("--repo", "client", "--store-cmd", store); code != 0 {
		t.Fatalf("--repo exit %d: %s", code, errs)
	}
	if n := len(recorded(t, rec, "stdin")); n != 5 {
		t.Fatalf("--repo run stored %d in total, want 5", n)
	}
}

// Several identities through one fixed store command would overwrite each
// other in a manager that replaces by name, and every one would still verify.
func TestIdentityBackup_SeveralIdentitiesNeedAPerFilePlaceholder(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	dir := t.TempDir()
	a, _, _ := idFile(t, dir, "a.txt", false)
	b, _, _ := idFile(t, dir, "b.txt", true)
	setCloneIdentity(t, cfgs["personal"].SyncDir, a)
	setCloneIdentity(t, cfgs["client"].SyncDir, b)
	store, verify, rec := fakeStores(t)
	code, out, errs := runIDBackup("--all", "--store-cmd", store, "--verify-cmd", verify)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (a fixed store command for two identities)\nstdout: %s\nstderr: %s", code, out, errs)
	}
	for _, s := range []string{"{fingerprint}", "--identity", a, b} {
		if !strings.Contains(errs, s) {
			t.Errorf("the refusal does not mention %q:\n%s", s, errs)
		}
	}
	if len(recorded(t, rec, "stdin")) != 0 {
		t.Fatal("something was stored before the refusal")
	}
}

func TestIdentityBackup_PlaceholdersAreSubstitutedPerFile(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	dir := t.TempDir()
	a, secA, rcptA := idFile(t, dir, "a.txt", false)
	b, secB, rcptB := idFile(t, dir, "b.txt", true)
	setCloneIdentity(t, cfgs["personal"].SyncDir, a)
	setCloneIdentity(t, cfgs["client"].SyncDir, b)
	store, verify, rec := fakeStores(t)
	code, out, errs := runIDBackup("--all", "--store-cmd", store+" {fingerprint} {name}", "--verify-cmd", verify+" {fingerprint}")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	argv := strings.Join(recorded(t, rec, "argv"), "\n")
	for _, s := range []string{safeFP(rcptA), safeFP(rcptB), "a.txt", "b.txt"} {
		if !strings.Contains(argv, s) {
			t.Errorf("argv lacks %q:\n%s", s, argv)
		}
	}
	if strings.Contains(argv, "{") {
		t.Errorf("a placeholder was left in argv:\n%s", argv)
	}
	// two distinct items, each the right key
	for fp, sec := range map[string]string{safeFP(rcptA): secA, safeFP(rcptB): secB} {
		got, err := os.ReadFile(filepath.Join(rec, "item."+fp))
		if err != nil || !strings.Contains(string(got), sec) {
			t.Errorf("item %s missing or wrong: %v", fp, err)
		}
	}
	if strings.Count(out, "verified (sha256 matches)") != 2 {
		t.Errorf("both must be verified:\n%s", out)
	}
	if strings.Contains(out+errs, secA) || strings.Contains(out+errs, secB) {
		t.Fatal("secret printed")
	}
}

// {name} alone can collide (two files called identity.txt): refuse before
// storing anything rather than overwrite.
func TestIdentityBackup_CollidingItemNamesAreRefused(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	a, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	b, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	setCloneIdentity(t, cfgs["personal"].SyncDir, a)
	setCloneIdentity(t, cfgs["client"].SyncDir, b)
	store, _, rec := fakeStores(t)
	code, _, errs := runIDBackup("--all", "--store-cmd", store+" {name}")
	if code != 2 || !strings.Contains(errs, "same") {
		t.Fatalf("exit %d, stderr %q: want 2 and a collision message", code, errs)
	}
	if len(recorded(t, rec, "stdin")) != 0 {
		t.Fatal("something was stored before the refusal")
	}
}

func TestIdentityBackup_UsageErrors(t *testing.T) {
	path, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	store, _, rec := fakeStores(t)
	for name, args := range map[string][]string{
		"no store-cmd":      {"--identity", path},
		"empty store-cmd":   {"--identity", path, "--store-cmd", "  "},
		"unterminated":      {"--identity", path, "--store-cmd", "op 'x"},
		"all and identity":  {"--all", "--identity", path, "--store-cmd", store},
		"repo and identity": {"--repo", "x", "--identity", path, "--store-cmd", store},
		"stray argument":    {"--identity", path, "--store-cmd", store, "extra"},
	} {
		if code, _, _ := runIDBackup(args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	if got := recorded(t, rec, "stdin"); len(got) != 0 {
		t.Fatal("a usage error ran the store command")
	}
}

func TestIdentityBackup_NothingToBackUpIsExit1(t *testing.T) {
	routedHost(t, []string{"personal"}, "personal", nil) // no memage.identity set
	store, _, rec := fakeStores(t)
	code, _, errs := runIDBackup("--all", "--store-cmd", store)
	if code != 1 || !strings.Contains(errs, "--identity") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if len(recorded(t, rec, "stdin")) != 0 {
		t.Fatal("ran")
	}
}

func TestRunIdentity_Dispatch(t *testing.T) {
	if code := runIdentity(nil); code != 2 {
		t.Errorf("no args: %d", code)
	}
	if code := runIdentity([]string{"frobnicate"}); code != 2 {
		t.Errorf("unknown: %d", code)
	}
}

func TestIdentityBackup_HelpExitsZeroAndPrintsUsage(t *testing.T) {
	code, out, errs := runIDBackup("-h")
	if code != 0 {
		t.Fatalf("-h exit %d, want 0", code)
	}
	if !strings.Contains(out+errs, "-store-cmd") {
		t.Errorf("-h does not describe -store-cmd: %q %q", out, errs)
	}
}

func idLineWith(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

// The host default identity (read by related clones) and the classic files
// rekey-pq keeps are still needed; each is listed with why.
func TestIdentityBackup_IncludesHostDefaultsAndClassicWithReasons(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	c, _, _ := idFile(t, t.TempDir(), "c.txt", true)
	setCloneIdentity(t, cfgs["personal"].SyncDir, c)
	base := defaultIdentityPath()
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	classic, _, _ := idFile(t, filepath.Dir(base), filepath.Base(base), false)
	pq, _, _ := idFile(t, filepath.Dir(base), filepath.Base(newIdentityPath(base)), true)
	retired, _, _ := idFile(t, filepath.Dir(base), filepath.Base(base)+".classic-retired", false)
	store, _, rec := fakeStores(t)
	code, out, errs := runIDBackup("--all", "--dry-run", "--store-cmd", store+" {fingerprint}")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	for path, why := range map[string]string{
		c:       "used by: personal",
		pq:      "host default",
		classic: "classic history and rollback",
		retired: "classic history and rollback",
	} {
		l := idLineWith(out, path)
		if l == "" || !strings.Contains(l, why) {
			t.Errorf("%s: want a line saying %q, got %q\n%s", path, why, l, out)
		}
	}
	if !strings.Contains(idLineWith(out, classic), "host default") {
		t.Errorf("the classic default is also a host default: %q", idLineWith(out, classic))
	}
	if strings.Contains(out, "(related clones)") {
		t.Errorf("no related repos are configured, so the label must be plain \"host default\":\n%s", out)
	}
	if l := idLineWith(out, "skipped:"); !strings.Contains(l, cfgs["client"].SyncDir) {
		t.Errorf("a clone without an identity must be reported as skipped:\n%s", out)
	}
	if len(recorded(t, rec, "stdin")) != 0 {
		t.Fatal("dry run stored")
	}
	// one clone's scope does not drag in the host defaults
	_, out, _ = runIDBackup("--repo", "personal", "--dry-run", "--store-cmd", store)
	if strings.Contains(out, base) || !strings.Contains(out, c) {
		t.Errorf("--repo personal must list only that clone's identity:\n%s", out)
	}
}

func TestIdentityBackup_SkippedClonesAreNeverSilent(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects", "client"}, "personal", nil)
	c, _, _ := idFile(t, t.TempDir(), "c.txt", false)
	setCloneIdentity(t, cfgs["personal"].SyncDir, c)
	if err := os.RemoveAll(filepath.Join(cfgs["projects"].SyncDir, ".git")); err != nil {
		t.Fatal(err)
	}
	store, _, _ := fakeStores(t)
	code, out, errs := runIDBackup("--all", "--dry-run", "--store-cmd", store)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	for dir, reason := range map[string]string{cfgs["projects"].SyncDir: "not a git repo", cfgs["client"].SyncDir: "no memage.identity"} {
		want := "skipped: " + dir + " (" + reason
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func TestIdentityBackup_RefusesASymlinkedConfiguredIdentity(t *testing.T) {
	cfgs := routedHost(t, []string{"personal"}, "personal", nil)
	dir := t.TempDir()
	real, _, _ := idFile(t, dir, "real.txt", false)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	setCloneIdentity(t, cfgs["personal"].SyncDir, link)
	store, _, rec := fakeStores(t)
	code, out, errs := runIDBackup("--repo", "personal", "--store-cmd", store)
	if code != 1 || !strings.Contains(errs, "symlink") {
		t.Fatalf("exit %d, stderr %q: want 1 and a symlink refusal", code, errs)
	}
	if len(recorded(t, rec, "stdin")) != 0 || strings.Contains(out, "verified (sha256") {
		t.Fatal("a symlinked identity was stored")
	}
}

func TestIdentityBackup_SummaryLine(t *testing.T) {
	path, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	store, verify, _ := fakeStores(t)
	fail := filepath.Join(t.TempDir(), "fail.sh")
	os.WriteFile(fail, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	wrong := filepath.Join(t.TempDir(), "wrong.sh")
	os.WriteFile(wrong, []byte("#!/bin/sh\necho x\n"), 0o700)
	for name, c := range map[string]struct {
		args []string
		want string
		code int
	}{
		"verified":         {[]string{"--store-cmd", store, "--verify-cmd", verify}, "1 stored (1 verified, 0 UNVERIFIED, 0 verify-failed), 0 store-failed", 0},
		"unverified":       {[]string{"--store-cmd", store}, "1 stored (0 verified, 1 UNVERIFIED, 0 verify-failed), 0 store-failed", 0},
		"store fail":       {[]string{"--store-cmd", fail}, "0 stored (0 verified, 0 UNVERIFIED, 0 verify-failed), 1 store-failed", 1},
		"mismatch":         {[]string{"--store-cmd", store, "--verify-cmd", wrong}, "1 stored (0 verified, 0 UNVERIFIED, 1 verify-failed), 0 store-failed", 1},
		"verify cmd fails": {[]string{"--store-cmd", store, "--verify-cmd", "false"}, "1 stored (0 verified, 0 UNVERIFIED, 1 verify-failed), 0 store-failed", 1},
	} {
		code, out, errs := runIDBackup(append([]string{"--identity", path}, c.args...)...)
		if code != c.code || !strings.Contains(out, c.want+"\n") {
			t.Errorf("%s: exit %d, want %d with %q:\n%s%s", name, code, c.code, c.want, out, errs)
		}
	}
	// a dry run stores nothing and has no such line
	if _, out, _ := runIDBackup("--identity", path, "--store-cmd", store, "--dry-run"); strings.Contains(out, " stored (") {
		t.Errorf("dry run printed a summary:\n%s", out)
	}
}

// overwriteStores are a manager that keeps ONE item whatever is asked: the
// store replaces it, the verify prints it. Extra arguments are ignored.
func overwriteStores(t *testing.T) (store, verify, rec string) {
	t.Helper()
	d := t.TempDir()
	rec = filepath.Join(d, "rec")
	if err := os.Mkdir(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	s, v := filepath.Join(d, "ow-store.sh"), filepath.Join(d, "ow-verify.sh")
	os.WriteFile(s, []byte("#!/bin/sh\ncat > \"$1/only\"\necho run >> \"$1/runs\"\n"), 0o700)
	os.WriteFile(v, []byte("#!/bin/sh\ncat \"$1/only\"\n"), 0o700)
	return s + " " + rec, v + " " + rec, rec
}

func twoIdentityClones(t *testing.T) (a, b string) {
	t.Helper()
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	dir := t.TempDir()
	a, _, _ = idFile(t, dir, "a.txt", false)
	b, _, _ = idFile(t, dir, "b.txt", true)
	setCloneIdentity(t, cfgs["personal"].SyncDir, a)
	setCloneIdentity(t, cfgs["client"].SyncDir, b)
	return a, b
}

// A placeholder in an argument that does not name the item, with a fixed
// verify command: each verify ran right after its own store and never saw the
// overwrite. It must never end in exit 0.
func TestIdentityBackup_PlaceholderOutsideTheItemNameNeverExits0(t *testing.T) {
	twoIdentityClones(t)
	store, verify, rec := overwriteStores(t)
	code, out, errs := runIDBackup("--all", "--store-cmd", store+" --tags {fingerprint}", "--verify-cmd", verify)
	if code == 0 {
		t.Fatalf("exit 0 with a fixed verify command and one overwritten item:\n%s%s", out, errs)
	}
	if code != 2 || !strings.Contains(errs, "--verify-cmd") {
		t.Errorf("exit %d, stderr %q: want 2 and a --verify-cmd message", code, errs)
	}
	if _, err := os.Stat(filepath.Join(rec, "runs")); err == nil {
		t.Error("the store ran before the refusal")
	}
}

// Whatever the argument layout, a final read-back of every item catches a
// later store that replaced an earlier one.
func TestIdentityBackup_FinalVerifyCatchesAnOverwrite(t *testing.T) {
	twoIdentityClones(t)
	store, verify, _ := overwriteStores(t)
	code, out, errs := runIDBackup("--all", "--store-cmd", store+" --tags {fingerprint}", "--verify-cmd", verify+" --tags {fingerprint}")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errs)
	}
	if n := strings.Count(out, "OVERWRITTEN by a later store:"); n != 1 {
		t.Errorf("OVERWRITTEN reported %d times, want 1 (the earlier item):\n%s%s", n, out, errs)
	}
	if strings.Contains(out, "2 stored (2 verified") {
		t.Errorf("the summary still claims two verified:\n%s", out)
	}
}

// The same key at two paths is one item, not a collision.
func TestIdentityBackup_SameKeyAtTwoPathsIsStoredOnce(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	a, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	b := filepath.Join(t.TempDir(), "identity.txt")
	data, _ := os.ReadFile(a)
	if err := os.WriteFile(b, data, 0o600); err != nil {
		t.Fatal(err)
	}
	setCloneIdentity(t, cfgs["personal"].SyncDir, a)
	setCloneIdentity(t, cfgs["client"].SyncDir, b)
	store, verify, rec := fakeStores(t)
	for _, extra := range []string{" {name}", " {fingerprint}", ""} {
		for _, f := range []string{"stdin.*", "argv.*"} {
			m, _ := filepath.Glob(filepath.Join(rec, f))
			for _, x := range m {
				os.Remove(x)
			}
		}
		code, out, errs := runIDBackup("--all", "--store-cmd", store+extra, "--verify-cmd", verify+extra)
		if code != 0 {
			t.Fatalf("%q: exit %d: %s%s", extra, code, out, errs)
		}
		if n := len(recorded(t, rec, "stdin")); n != 1 {
			t.Errorf("%q: stored %d times, want once", extra, n)
		}
		if !strings.Contains(out, a) || !strings.Contains(out, b) {
			t.Errorf("%q: both paths must be listed:\n%s", extra, out)
		}
	}
}

func TestIdentityBackup_HostDefaultLabelNamesRelatedClonesWhenConfigured(t *testing.T) {
	cfgs := routedHost(t, []string{"personal"}, "personal", nil)
	c, _, _ := idFile(t, t.TempDir(), "c.txt", true)
	setCloneIdentity(t, cfgs["personal"].SyncDir, c)
	base := defaultIdentityPath()
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	pq, _, _ := idFile(t, filepath.Dir(base), filepath.Base(newIdentityPath(base)), true)
	rp := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(rp)
	if err := json.Unmarshal(b, &rj); err != nil {
		t.Fatal(err)
	}
	rj["registry"] = filepath.Join(t.TempDir(), "registry")
	b, _ = json.Marshal(rj)
	os.WriteFile(rp, b, 0o600)
	store, _, _ := fakeStores(t)
	code, out, errs := runIDBackup("--all", "--dry-run", "--store-cmd", store+" {fingerprint}")
	if code != 0 || !strings.Contains(idLineWith(out, pq), "host default (related clones)") {
		t.Fatalf("exit %d: want the label \"host default (related clones)\":\n%s%s", code, out, errs)
	}
}

func TestClassicOf_OnlyTheExactRekeyNaming(t *testing.T) {
	for in, want := range map[string]string{
		"/k/identity-pq.txt":    "/k/identity.txt",
		"/k/identity-pq":        "/k/identity",
		"/k/identity.txt":       "/k/identity.txt",
		"/k/work-pq.txt":        "/k/work-pq.txt",
		"/k/my-identity-pq.txt": "/k/my-identity-pq.txt",
		"/k/identity-pq.txt.x":  "/k/identity-pq.txt.x",
	} {
		if got := classicOf(in); got != want {
			t.Errorf("classicOf(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][]string{
		"/k/identity-pq.txt": {"/k/identity-pq.txt.classic-retired", "/k/identity.txt.classic-retired"},
		"/k/work-pq.txt":     {"/k/work-pq.txt.classic-retired", "/k/work.txt.classic-retired"},
		"/k/work.txt":        {"/k/work.txt.classic-retired"},
	} {
		if got := retiredCandidates(in); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("retiredCandidates(%q) = %v, want %v", in, got, want)
		}
	}
}

// A file that merely sits beside a "-pq" one is not that host's classic.
func TestIdentityBackup_DoesNotAdoptANeighbourOfACustomPQName(t *testing.T) {
	cfgs := routedHost(t, []string{"personal"}, "personal", nil)
	dir := t.TempDir()
	pq, _, _ := idFile(t, dir, "work-pq.txt", true)
	neighbour, _, _ := idFile(t, dir, "work.txt", false)
	retired, _, _ := idFile(t, dir, "work.txt.classic-retired", false)
	setCloneIdentity(t, cfgs["personal"].SyncDir, pq)
	store, _, _ := fakeStores(t)
	code, out, errs := runIDBackup("--repo", "personal", "--dry-run", "--store-cmd", store+" {fingerprint}")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errs)
	}
	if strings.Contains(out, neighbour+"\t") {
		t.Errorf("an unrelated neighbour was adopted:\n%s", out)
	}
	if !strings.Contains(out, retired) {
		t.Errorf("the retired classic of a custom-named identity is missing:\n%s", out)
	}
}

// laterStoreStores keep one item. The second store replaces it and then either
// exits 1 ("exit1") or writes junk and exits 0 ("junk").
func laterStoreStores(t *testing.T, mode string) (store, verify, rec string) {
	t.Helper()
	d := t.TempDir()
	rec = filepath.Join(d, "rec")
	if err := os.Mkdir(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	second := `cp "$rec/in" "$rec/only"; exit 1`
	if mode == "junk" {
		second = `echo junk > "$rec/only"`
	}
	s, v := filepath.Join(d, "later-store.sh"), filepath.Join(d, "later-verify.sh")
	os.WriteFile(s, []byte(`#!/bin/sh
rec="$1"; cat > "$rec/in"; echo run >> "$rec/runs"
n=$(wc -l < "$rec/runs" | tr -d ' ')
if [ "$n" -ge 2 ]; then `+second+`; fi
cp "$rec/in" "$rec/only"
`), 0o700)
	os.WriteFile(v, []byte("#!/bin/sh\ncat \"$1/only\"\n"), 0o700)
	return s + " " + rec, v + " " + rec, rec
}

// Item 1 verifies; a later store replaces it and then fails (or its own
// verify fails). Item 1 must not stay "verified": the key is gone.
func TestIdentityBackup_FinalPassRunsWhenALaterStoreOrVerifyFails(t *testing.T) {
	for _, mode := range []string{"exit1", "junk"} {
		t.Run(mode, func(t *testing.T) {
			twoIdentityClones(t)
			store, verify, _ := laterStoreStores(t, mode)
			code, out, errs := runIDBackup("--all", "--store-cmd", store+" {fingerprint}", "--verify-cmd", verify+" {fingerprint}")
			if code != 1 {
				t.Fatalf("exit %d, want 1\n%s%s", code, out, errs)
			}
			var first string
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "verified (sha256 matches)") && !strings.Contains(l, " stored (") {
					first, _, _ = strings.Cut(l, "\t")
				}
			}
			if first == "" {
				t.Fatalf("the first item never verified, so the scenario did not happen:\n%s", out)
			}
			if !strings.Contains(errs, "OVERWRITTEN by a later store: "+first) {
				t.Errorf("item 1 (%s) must end OVERWRITTEN:\n%s%s", first, out, errs)
			}
			if strings.Contains(out, "(1 verified") {
				t.Errorf("item 1 is still counted as verified:\n%s", out)
			}
			if l := idLineWith(out, "OVERWRITTEN"); strings.Count(l, first) != 1 {
				t.Errorf("the OVERWRITTEN line names the path %d times, want once: %q", strings.Count(l, first), l)
			}
		})
	}
}

// The same key with a different comment line is the same key.
func TestIdentityBackup_SameKeyWithAnotherCommentIsStoredOnce(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	a, secret, rcpt := idFile(t, t.TempDir(), "a.txt", false)
	b := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(b, []byte("# another comment, same key\n"+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setCloneIdentity(t, cfgs["personal"].SyncDir, a)
	setCloneIdentity(t, cfgs["client"].SyncDir, b)
	store, verify, rec := fakeStores(t)
	code, out, errs := runIDBackup("--all", "--store-cmd", store+" {fingerprint}", "--verify-cmd", verify+" {fingerprint}")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errs)
	}
	if n := len(recorded(t, rec, "stdin")); n != 1 {
		t.Errorf("stored %d times, want once", n)
	}
	if !strings.Contains(out, a) || !strings.Contains(out, b) || !strings.Contains(out, agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(rcpt))) {
		t.Errorf("both files must be listed:\n%s", out)
	}
}

// "stored once" is only true when the first copy was stored.
func TestIdentityBackup_SameKeyLineDoesNotClaimAFailedStore(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", nil)
	a, _, _ := idFile(t, t.TempDir(), "a.txt", false)
	b := filepath.Join(t.TempDir(), "b.txt")
	data, _ := os.ReadFile(a)
	os.WriteFile(b, data, 0o600)
	setCloneIdentity(t, cfgs["personal"].SyncDir, a)
	setCloneIdentity(t, cfgs["client"].SyncDir, b)
	fail := filepath.Join(t.TempDir(), "fail.sh")
	os.WriteFile(fail, []byte("#!/bin/sh\ncat >/dev/null\nexit 1\n"), 0o700)
	code, out, errs := runIDBackup("--all", "--store-cmd", fail)
	if code != 1 {
		t.Fatalf("exit %d: %s%s", code, out, errs)
	}
	if strings.Contains(out, "stored once") {
		t.Errorf("claims a copy was stored although the store failed:\n%s", out)
	}
}
