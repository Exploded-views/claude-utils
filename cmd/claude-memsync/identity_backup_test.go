//go:build !windows

package main

import (
	"bytes"
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
`), 0o700)
	os.WriteFile(verify, []byte("#!/bin/sh\ncat \"$1/stored\"\n"), 0o700)
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

func runIDBackup(args ...string) (int, string, string) {
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
	if strings.Contains(out, "verified") || strings.Contains(out+errs, secret) || strings.Contains(out+errs, "not-the-key") {
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

func TestIdentityBackup_AllDeduplicatesSharedIdentities(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects", "client"}, "personal", nil)
	dir := t.TempDir()
	a, _, _ := idFile(t, dir, "a.txt", false)
	b, _, _ := idFile(t, dir, "b.txt", true)
	for name, id := range map[string]string{"personal": a, "projects": a, "client": b} {
		if out, err := exec.Command("git", "-C", cfgs[name].SyncDir, "config", "memage.identity", id).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	store, verify, rec := fakeStores(t)
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
	if !strings.Contains(out+errs, "--store-cmd") {
		t.Errorf("-h does not describe --store-cmd: %q %q", out, errs)
	}
}
