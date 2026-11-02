package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/proc"
	"github.com/MarimerLLC/claude-utils/internal/stoprec"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// stopHome points the config dir at a temp HOME and returns the records dir.
func stopHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	d, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(d, "stops")
}

// fakeJournal replaces journalctlFn for one test and restores it.
func fakeJournal(t *testing.T, f func(ctx context.Context, n int) (string, error)) {
	t.Helper()
	old := journalctlFn
	journalctlFn = f
	t.Cleanup(func() { journalctlFn = old })
}

// countSpawns counts every child proc starts, and records the names.
func countSpawns(t *testing.T) (n *atomic.Int64, names func() []string) {
	t.Helper()
	n = new(atomic.Int64)
	var got atomic.Value
	got.Store([]string{})
	restore := proc.SetSpawnHook(func(name string, args []string) {
		n.Add(1)
		got.Store(append(append([]string{}, got.Load().([]string)...), filepath.Base(name)))
	})
	t.Cleanup(restore)
	return n, func() []string { return got.Load().([]string) }
}

var uncleanEnv = map[string]string{"SERVICE_RESULT": "exit-code", "EXIT_CODE": "exited", "EXIT_STATUS": "1"}

func runRS(t *testing.T, args []string, env map[string]string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = runRecordStop(args, envOf(env), &o, &e)
	return code, o.String(), e.String()
}

func TestRecordStop_CleanStopWritesNothingAndSpawnsNothing(t *testing.T) {
	dir := stopHome(t)
	n, _ := countSpawns(t)
	code, out, errOut := runRS(t, nil, map[string]string{"SERVICE_RESULT": "success", "EXIT_CODE": "exited", "EXIT_STATUS": "0"})
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if n.Load() != 0 {
		t.Fatalf("a clean stop spawned %d processes", n.Load())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a clean stop created the records dir")
	}
}

func TestRecordStop_UncleanWritesRecord(t *testing.T) {
	for _, res := range []string{"exit-code", "killed", "signal", "oom-kill", "timeout", "core-dump"} {
		t.Run(res, func(t *testing.T) {
			dir := stopHome(t)
			var gotN int
			fakeJournal(t, func(ctx context.Context, n int) (string, error) {
				gotN = n
				return "2026-11-02T12:00:00+0000 h claude-memsync[1]: first\n2026-11-02T12:00:01+0000 h claude-memsync[1]: second\n", nil
			})
			code, _, errOut := runRS(t, nil, map[string]string{"SERVICE_RESULT": res, "EXIT_CODE": "exited", "EXIT_STATUS": "3"})
			if code != 0 || errOut != "" {
				t.Fatalf("code=%d err=%q", code, errOut)
			}
			if gotN != 80 {
				t.Fatalf("journal asked for %d lines, want 80", gotN)
			}
			recs, err := stoprec.List(dir)
			if err != nil || len(recs) != 1 {
				t.Fatalf("records = %d, %v", len(recs), err)
			}
			r := recs[0]
			if r.Result != res || r.ExitCode != "exited" || r.ExitStatus != "3" || r.Source != "systemd" || r.Repeats != 1 {
				t.Fatalf("record = %+v", r)
			}
			if len(r.Tail) != 2 || !strings.HasSuffix(r.Tail[1], "second") {
				t.Fatalf("tail = %v", r.Tail)
			}
		})
	}
}

func TestRecordStop_FlagsOverrideEnv(t *testing.T) {
	dir := stopHome(t)
	fakeJournal(t, func(context.Context, int) (string, error) { return "", nil })
	code, _, _ := runRS(t, []string{"--result", "timeout", "--exit-code", "killed", "--exit-status", "KILL"}, map[string]string{"SERVICE_RESULT": "success"})
	if code != 0 {
		t.Fatal(code)
	}
	r, ok, _ := stoprec.Latest(dir)
	if !ok || r.Result != "timeout" || r.ExitCode != "killed" || r.ExitStatus != "KILL" || r.Source != "manual" {
		t.Fatalf("record = %+v", r)
	}
}

func TestRecordStop_SecretsNeverStoredOrPrinted(t *testing.T) {
	dir := stopHome(t)
	secret := "ghp_" + strings.Repeat("b", 36)
	fakeJournal(t, func(context.Context, int) (string, error) {
		return "ok line\nleak " + secret + " here\n", nil
	})
	code, out, errOut := runRS(t, nil, uncleanEnv)
	if code != 0 || strings.Contains(out+errOut, secret) {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	r, _, _ := stoprec.Latest(dir)
	if r.Withheld != 1 {
		t.Fatalf("Withheld = %d", r.Withheld)
	}
	b, _ := os.ReadFile(filepath.Join(dir, mustOne(t, dir)))
	if bytes.Contains(b, []byte("ghp_")) {
		t.Fatal("the record holds the token")
	}
}

func mustOne(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("dir %s: %v %v", dir, ents, err)
	}
	return ents[0].Name()
}

// writeDenylist puts a denylist in a repo's sync dir.
func writeDenylist(t *testing.T, syncDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(syncDir, "denylist.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, mustOne(t, dir)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecordStop_DenylistedPathsAreRedacted(t *testing.T) {
	dir := stopHome(t)
	sync := filepath.Join(os.Getenv("HOME"), ".claudesync")
	os.MkdirAll(sync, 0o700)
	writeDenylist(t, sync, "acme-secret-plans\n")
	fakeJournal(t, func(context.Context, int) (string, error) {
		return "bad ref projects/acme-secret-plans/x.md in tree\n", nil
	})
	runRS(t, nil, uncleanEnv)
	r, _, _ := stoprec.Latest(dir)
	if len(r.Tail) != 1 || strings.Contains(r.Tail[0], "acme-secret-plans") || !strings.Contains(r.Tail[0], "<path redacted>") {
		t.Fatalf("tail = %v", r.Tail)
	}
}

func TestRecordStop_UnreadableDenylistRedactsPathLikeTokens(t *testing.T) {
	dir := stopHome(t)
	sync := filepath.Join(os.Getenv("HOME"), ".claudesync")
	os.MkdirAll(sync, 0o700)
	writeDenylist(t, sync, "(unclosed\n") // does not parse
	fakeJournal(t, func(context.Context, int) (string, error) {
		return "bad ref projects/whatever/x.md in tree\n", nil
	})
	_, _, errOut := runRS(t, nil, uncleanEnv)
	r, _, _ := stoprec.Latest(dir)
	if len(r.Tail) != 1 || strings.Contains(r.Tail[0], "whatever") {
		t.Fatalf("tail = %v", r.Tail)
	}
	if !strings.HasPrefix(errOut, "record-stop:") || strings.Contains(errOut, "(unclosed") {
		t.Fatalf("stderr = %q, want a record-stop: line that names no pattern", errOut)
	}
}

func TestRecordStop_UnloadableConfigRedactsPathLikeTokens(t *testing.T) {
	dir := stopHome(t)
	sync := filepath.Join(os.Getenv("HOME"), ".claudesync")
	os.MkdirAll(sync, 0o700)
	os.WriteFile(filepath.Join(sync, "config.json"), []byte("{broken"), 0o600)
	fakeJournal(t, func(context.Context, int) (string, error) {
		return "bad ref projects/whatever/x.md in tree\n", nil
	})
	_, _, errOut := runRS(t, nil, uncleanEnv)
	r, _, _ := stoprec.Latest(dir)
	if len(r.Tail) != 1 || strings.Contains(r.Tail[0], "whatever") || !strings.HasPrefix(errOut, "record-stop:") {
		t.Fatalf("tail = %v stderr = %q", r.Tail, errOut)
	}
}

// Routed mode: the daemon logs for every repo into one journal, so every
// repo's denylist applies, not just the primary's.
func TestRecordStop_EveryRoutedRepoDenylistApplies(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", []map[string]string{{"folder": "-a", "repo": "client"}})
	writeDenylist(t, cfgs["client"].SyncDir, "acme-merger\n") // only the secondary lists it
	d, _ := config.Dir()
	dir := filepath.Join(d, "stops")
	fakeJournal(t, func(context.Context, int) (string, error) {
		return "scrub: holding back projects/-Users-b-code-acme/memory/acme-merger.md\n", nil
	})
	if code, _, errOut := runRS(t, nil, uncleanEnv); code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if b := recordBytes(t, dir); bytes.Contains(b, []byte("acme-merger")) || !bytes.Contains(b, []byte("path redacted")) {
		t.Fatalf("the secondary repo's denylisted term is in the record:\n%s", b)
	}
}

func TestRecordStop_RoutedUnreadableSecondaryDenylistRedactsAllPaths(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", []map[string]string{{"folder": "-a", "repo": "client"}})
	writeDenylist(t, cfgs["personal"].SyncDir, "other-term\n")
	writeDenylist(t, cfgs["client"].SyncDir, "(unclosed\n")
	d, _ := config.Dir()
	fakeJournal(t, func(context.Context, int) (string, error) { return "see projects/innocent/x.md now\n", nil })
	runRS(t, nil, uncleanEnv)
	if b := recordBytes(t, filepath.Join(d, "stops")); bytes.Contains(b, []byte("innocent")) {
		t.Fatalf("closed mode let a path through:\n%s", b)
	}
}

func TestRecordStop_RoutedUnloadableSecondaryConfigRedactsAllPaths(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "client"}, "personal", []map[string]string{{"folder": "-a", "repo": "client"}})
	os.WriteFile(cfgs["client"].Path(), []byte("{broken"), 0o600)
	d, _ := config.Dir()
	fakeJournal(t, func(context.Context, int) (string, error) { return "see projects/innocent/x.md now\n", nil })
	runRS(t, nil, uncleanEnv)
	if b := recordBytes(t, filepath.Join(d, "stops")); bytes.Contains(b, []byte("innocent")) {
		t.Fatalf("closed mode let a path through:\n%s", b)
	}
}

func TestRecordStop_ByHandWithNoFlagsRecordsUnknown(t *testing.T) {
	dir := stopHome(t)
	fakeJournal(t, func(context.Context, int) (string, error) { return "", nil })
	runRS(t, nil, map[string]string{})
	r, ok, _ := stoprec.Latest(dir)
	if !ok || r.Result != "unknown" || r.Source != "manual" {
		t.Fatalf("record = %+v", r)
	}
}

func TestRecordStop_TokenInAResultFlagIsNotStoredOrPrinted(t *testing.T) {
	dir := stopHome(t)
	fakeJournal(t, func(context.Context, int) (string, error) { return "", nil })
	secret := "ghp_" + strings.Repeat("d", 36)
	code, out, errOut := runRS(t, []string{"--result", "boom " + secret}, map[string]string{})
	if code != 0 || strings.Contains(out+errOut, secret) {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if b := recordBytes(t, dir); bytes.Contains(b, []byte("ghp_")) {
		t.Fatalf("the token reached the record:\n%s", b)
	}
}

func TestRecordStop_NeverReturnsNonZero(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		env    map[string]string
		args   []string
		stderr bool // a failing case must say why on stderr
	}{
		{name: "records dir is a file", stderr: true, setup: func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Dir(dir), 0o700)
			os.WriteFile(dir, []byte("x"), 0o600)
		}},
		{name: "dir 0500", setup: func(t *testing.T, dir string) {
			os.MkdirAll(dir, 0o500)
		}},
		{name: "dir is a symlink", stderr: true, setup: func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Dir(dir), 0o700)
			if err := os.Symlink(t.TempDir(), dir); err != nil {
				t.Skip(err)
			}
		}},
		{name: "config dir parent is a file", stderr: true, setup: func(t *testing.T, dir string) {
			home := os.Getenv("HOME")
			os.WriteFile(filepath.Join(home, ".config"), []byte("x"), 0o600)
		}},
		{name: "journalctl fails", stderr: true, setup: func(t *testing.T, dir string) {
			fakeJournal(t, func(context.Context, int) (string, error) { return "", errors.New("no journal") })
		}},
		{name: "journalctl 50 MB", setup: func(t *testing.T, dir string) {
			big := strings.Repeat(strings.Repeat("z", 1000)+"\n", 50_000)
			fakeJournal(t, func(context.Context, int) (string, error) { return big, nil })
		}},
		{name: "garbage status", env: map[string]string{"SERVICE_RESULT": "exit-code", "EXIT_CODE": "exited", "EXIT_STATUS": "abc"}},
		{name: "control bytes in env", env: map[string]string{"SERVICE_RESULT": "exit-code\x1b[31m", "EXIT_CODE": "\x00", "EXIT_STATUS": strings.Repeat("9", 5000)}},
		{name: "unknown flag", stderr: true, args: []string{"--no-such-flag"}},
		{name: "positional argument", stderr: true, args: []string{"surprise"}},
		{name: "panic in the body", stderr: true, setup: func(t *testing.T, dir string) {
			old := recordStopHook
			recordStopHook = func() { panic("injected") }
			t.Cleanup(func() { recordStopHook = old })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := stopHome(t)
			fakeJournal(t, func(context.Context, int) (string, error) { return "a line\n", nil })
			if c.setup != nil {
				c.setup(t, dir)
			}
			env := c.env
			if env == nil {
				env = uncleanEnv
			}
			code, _, errOut := runRS(t, c.args, env)
			if code != 0 {
				t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut)
			}
			if c.stderr && !strings.HasPrefix(errOut, "record-stop:") {
				t.Fatalf("stderr = %q, want a line starting record-stop:", errOut)
			}
			if c.name == "panic in the body" && !strings.Contains(errOut, "panic") {
				t.Fatalf("a panic left no stderr line: %q", errOut)
			}
			if c.name == "unknown flag" && errOut == "" {
				t.Fatal("an unknown flag left no stderr line")
			}
		})
	}
}

func TestRecordStop_NoHomeStillExitsZero(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	fakeJournal(t, func(context.Context, int) (string, error) { return "", nil })
	if code, _, _ := runRS(t, nil, uncleanEnv); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestRecordStop_JournalDeadline(t *testing.T) {
	if journalTimeout != 5*time.Second {
		t.Fatalf("journalTimeout = %v, want 5s", journalTimeout)
	}
	dir := stopHome(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fakeJournal(t, func(ctx context.Context, n int) (string, error) {
		<-release // a seam that ignores its context
		return "late\n", nil
	})
	old := journalTimeout
	journalTimeout = 150 * time.Millisecond
	defer func() { journalTimeout = old }()
	start := time.Now()
	code, _, _ := runRS(t, nil, uncleanEnv)
	if code != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("exit %d after %v", code, time.Since(start))
	}
	// The stop is still recorded, with no tail.
	r, ok, _ := stoprec.Latest(dir)
	if !ok || len(r.Tail) != 0 {
		t.Fatalf("record = %+v ok=%v", r, ok)
	}
}

func TestRecordStop_NoServiceManagerCalls(t *testing.T) {
	stopHome(t)
	n, names := countSpawns(t)
	fakeJournal(t, func(context.Context, int) (string, error) { return "x\n", nil })
	runRS(t, nil, uncleanEnv)
	for _, s := range names() {
		if s == "systemctl" || s == "launchctl" {
			t.Fatalf("record-stop ran %s", s)
		}
	}
	_ = n
}

func TestRecordStop_Help(t *testing.T) {
	code, out, _ := runRS(t, []string{"-h"}, nil)
	if code != 0 || !strings.Contains(out, "usage: claude-memsync record-stop") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// fakeBin puts a script named name on a PATH that holds nothing else, and
// returns the file its arguments are logged to.
func fakeBin(t *testing.T, name, body string) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fakes")
	}
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return log
}

func TestRunJournalctl_UsesTheUserUnitWithOneSpawn(t *testing.T) {
	log := fakeBin(t, "journalctl", "echo one; echo two")
	n, names := countSpawns(t)
	out, err := runJournalctl(context.Background(), 80)
	if err != nil || out != "one\ntwo\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if n.Load() != 1 || names()[0] != "journalctl" {
		t.Fatalf("spawns = %d %v, want exactly one journalctl", n.Load(), names())
	}
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); got != "--user -u claude-memsync.service -n 80 --no-pager -o short-iso" {
		t.Fatalf("args = %q", got)
	}
}

func TestRunJournalctl_MissingBinaryIsAnError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := runJournalctl(context.Background(), 80); err == nil {
		t.Fatal("no error for a missing journalctl")
	}
}

func TestRunJournalctl_KilledAtTheDeadline(t *testing.T) {
	fakeBin(t, "journalctl", "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runJournalctl(ctx, 80)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

// The real record-stop, run as the program, exits 0 even when its body panics.
func TestRecordStop_ProcessExitZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process semantics")
	}
	for _, mode := range []string{"record-stop-panic", "record-stop"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(os.Args[0])
			cmd.Env = []string{
				"MEMSYNC_TEST_MAIN=" + mode, "HOME=" + home, "PATH=" + t.TempDir(),
				"SERVICE_RESULT=timeout", "EXIT_CODE=killed", "EXIT_STATUS=KILL",
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("record-stop exited non-zero: %v\n%s", err, out)
			}
			if mode == "record-stop-panic" && !strings.Contains(string(out), "panic") {
				t.Fatalf("no panic line on stderr:\n%s", out)
			}
			if mode == "record-stop" {
				ents, _ := os.ReadDir(filepath.Join(home, ".config", "claude-memsync", "stops"))
				if len(ents) != 1 {
					t.Fatalf("records = %v", ents)
				}
			}
		})
	}
}

// A repo named in routes.json whose config is absent has a denylist we cannot
// read, so nothing can be vouched for; its name (maybe a client term) is not
// printed.
func TestRecordStop_RoutedMissingRepoRedactsAllPathsAndNamesNothing(t *testing.T) {
	routedHost(t, []string{"personal"}, "personal", nil, "client-acme-missing")
	d, _ := config.Dir()
	fakeJournal(t, func(context.Context, int) (string, error) { return "see projects/innocent/x.md now\n", nil })
	_, _, errOut := runRS(t, nil, uncleanEnv)
	if b := recordBytes(t, filepath.Join(d, "stops")); bytes.Contains(b, []byte("innocent")) {
		t.Fatalf("closed mode let a path through:\n%s", b)
	}
	if !strings.HasPrefix(errOut, "record-stop:") || strings.Contains(errOut, "acme") || !strings.Contains(errOut, "1 ") {
		t.Fatalf("stderr = %q, want a record-stop: line with the count and no repo name", errOut)
	}
}
