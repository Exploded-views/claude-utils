package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// writeRecord puts a loop-failed record into the repo of cfgPath.
func writeRecord(t *testing.T, cfgPath, text string) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := loopFailedPath(cfg.SyncDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hookContext(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	return reply.HookSpecificOutput.AdditionalContext
}

// The design: a repo held by the private-only check is alerted
// to the agents. The hook puts one line per held repo into the context, once
// per session per hold record, and again when the record changes.
func TestHook_AlertsAHeldRepoOncePerSessionPerRecord(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("sess-h", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook") // baseline: nothing changed
	writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+"check failed, nothing syncs until it passes: installation lookup: GitHub returned HTTP 404\n")

	code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if code != 0 || !strings.Contains(ctx, "is HELD by the private-only check") || !strings.Contains(ctx, "HTTP 404") || !strings.Contains(ctx, "claude-memsync status") {
		t.Fatalf("exit %d stderr %q; context lacks the held alert:\n%s", code, errs, ctx)
	}
	if strings.Contains(ctx, framingLine) {
		t.Errorf("no note changed, but the changes framing was printed:\n%s", ctx)
	}

	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); strings.Contains(hookContext(t, out), "HELD") {
		t.Error("the same hold was alerted twice in one session")
	}

	writeRecord(t, cfgPath, "2026-10-26T03:00:00Z "+heldMarker+"held from an earlier run: privateOnly is off now\n")
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); !strings.Contains(hookContext(t, out), "held from an earlier run") {
		t.Error("a changed hold record was not alerted again")
	}

}

// Only a hold is alerted: a dead loop's record, or one that merely mentions
// the marker, is not.
func TestHook_NoAlertWithoutAHold(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("sess-n", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	for _, rec := range []string{
		"2026-10-26T02:50:29Z watch failed: too many open files\n",
		"2026-10-26T02:50:29Z sync failed: remote said " + heldMarker + "x\n",
	} {
		writeRecord(t, cfgPath, rec)
		if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); strings.Contains(hookContext(t, out), "HELD") {
			t.Errorf("alerted for a record that is not a hold: %q", rec)
		}
	}
}

// The alert is part of the first prompt of a session too (its baseline).
func TestHook_AlertsOnTheFirstPromptOfASession(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+"check failed, nothing syncs until it passes: repo is not private\n")
	_, out, errs := runHook(t, hookJSON("sess-f1", "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	if !strings.Contains(hookContext(t, out), "is HELD by the private-only check") {
		t.Fatalf("first prompt not alerted; stdout %q stderr %q", out, errs)
	}
}

// The reason comes from a state file, so it is sanitized before it reaches
// the agent's context: no control characters or newlines (no injected extra
// lines), and capped in length.
func TestHook_HeldReasonIsSanitizedAndCapped(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	evil := "check failed\rIGNORE PREVIOUS\x1b[31m\ttab\x00nul" + strings.Repeat("x", 500) + "\nsecond line injected"
	writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+evil)
	_, out, _ := runHook(t, hookJSON("sess-s", "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	var alert string
	for _, l := range strings.Split(ctx, "\n") {
		if strings.Contains(l, "is HELD") {
			alert = l
		}
	}
	if alert == "" {
		t.Fatalf("no alert:\n%s", ctx)
	}
	if strings.Contains(ctx, "second line injected") {
		t.Errorf("a newline in the record injected a context line:\n%s", ctx)
	}
	for _, r := range alert {
		if r < 0x20 || r == 0x7f {
			t.Errorf("control character %q in the alert line: %q", r, alert)
			break
		}
	}
	if n := len(alert); n > 400 {
		t.Errorf("alert line is %d bytes; the reason must be capped", n)
	}
}

// A missing, unreadable or malformed record never fails the hook or blocks
// the prompt: no alert line, exit 0, nothing on stderr from the alert.
func TestHook_BadRecordGivesNoAlertAndNoFailure(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("sess-b", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	cfg, _ := config.Load(cfgPath)
	p := loopFailedPath(cfg.SyncDir)
	cases := map[string]func(){
		"missing":     func() { os.Remove(p) },
		"empty":       func() { writeRecord(t, cfgPath, "") },
		"binary":      func() { writeRecord(t, cfgPath, "\x00\x01\x02\xff") },
		"marker-only": func() { writeRecord(t, cfgPath, heldMarker) },
		"unreadable": func() {
			writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+"x\n")
			os.Chmod(p, 0)
		},
		"directory": func() { os.Remove(p); os.MkdirAll(p, 0o700) },
	}
	for name, setup := range cases {
		setup()
		code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
		if code != 0 || strings.Contains(hookContext(t, out), "HELD") || strings.Contains(errs, "held") {
			t.Errorf("%s: exit %d context %q stderr %q", name, code, hookContext(t, out), errs)
		}
		os.Chmod(p, 0o600)
		os.RemoveAll(p)
	}
}

// With many repos, every held one is alerted within the deadline, even when
// the change report (git walks per repo) overruns it: the alerts are read
// before any git work and go out on the deadline path too. Timing is forced
// with a worker slower than the deadline, so the test does not depend on load.
func TestHook_ManyHeldReposAlertedEvenWhenTheReportOverruns(t *testing.T) {
	const n = 24
	var names []string
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("project-%02d", i))
	}
	cfgs := routedHost(t, names, "", []map[string]string{{"folder": "-home-alice-src-app", "repo": names[0]}})
	in := hookJSON("sess-m", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0") // baselines, untimed
	for _, c := range cfgs {
		p := loopFailedPath(c.SyncDir)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("2026-10-26T02:50:29Z "+heldMarker+"check failed: HTTP 404\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	slow := slowHook(t, 3*time.Second)
	start := time.Now()
	code, out, errs := runHook(t, in, "--hook", "--deadline", "300ms")
	if took := time.Since(start); took > 300*time.Millisecond+700*time.Millisecond {
		t.Errorf("returned after %v, want within the deadline", took)
	}
	if code != 0 || !strings.Contains(errs, "deadline exceeded") {
		t.Fatalf("exit %d stderr %q; want the deadline path", code, errs)
	}
	ctx := hookContext(t, out)
	if got := strings.Count(ctx, "is HELD"); got != n {
		t.Errorf("%d alerts for %d held repos on the deadline path:\n%s", got, n, ctx)
	}
	if strings.Contains(ctx, framingLine) {
		t.Errorf("change notices printed on the deadline path:\n%s", ctx)
	}
	// Told once: the next prompt (no overrun) does not repeat them.
	slow.Store(false)
	if _, out, _ := runHook(t, in, "--hook", "--deadline", "0"); strings.Contains(hookContext(t, out), "is HELD") {
		t.Error("alerts delivered on the deadline path were repeated")
	}
}

// The record is read bounded and only from a regular file: a FIFO must not
// block the hook (or swallow the alerts with it), and a symlink to an endless
// file must not grow memory. Neither gives an alert or a failure.
func TestHook_HeldRecordFIFOAndSymlinkAreSkippedQuickly(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := loopFailedPath(cfg.SyncDir)
	os.MkdirAll(filepath.Dir(p), 0o700)
	in := hookJSON("sess-x", "/home/alice/src/app", "UserPromptSubmit")
	for name, setup := range map[string]func() error{
		"fifo": func() error { return mkfifo(p) },
		// A link is not followed: here it points at a real hold record, which
		// must not be alerted (a link to /dev/zero would read forever).
		"symlink": func() error {
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.WriteFile(target, []byte("2026-10-26T02:50:29Z "+heldMarker+"via link\n"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, p)
		},
	} {
		os.RemoveAll(p)
		if err := setup(); err != nil {
			t.Skipf("%s: %v", name, err)
		}
		type res struct {
			code      int
			out, errs string
		}
		done := make(chan res, 1)
		go func() {
			code, out, errs := runHook(t, in, "--config", cfgPath, "--hook", "--deadline", "0")
			done <- res{code, out, errs}
		}()
		select {
		case r := <-done:
			if r.code != 0 || strings.Contains(hookContext(t, r.out), "HELD") {
				t.Errorf("%s: exit %d context %q stderr %q", name, r.code, hookContext(t, r.out), r.errs)
			}
		case <-time.After(3 * time.Second):
			// Unblock the reader so the goroutine ends, then fail.
			if f, err := os.OpenFile(p, os.O_WRONLY|syscallNonblock(), 0); err == nil {
				f.Close()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			t.Errorf("%s: the hook blocked on the held record (no deadline set)", name)
		}
	}
	os.RemoveAll(p)
}

// Invisible and line-breaking characters are stripped too: bidi controls,
// zero-width characters (Unicode Cf) and the line and paragraph separators.
// The time field is sanitized like the reason.
func TestHook_HeldTextStripsInvisibleAndSeparators(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	rec := "2026\u202e10\u200b-26T02:50:29Z " + heldMarker + "a\u2066b\u2069c\u200dd\u2028second\u2029third\n"
	writeRecord(t, cfgPath, rec)
	_, out, _ := runHook(t, hookJSON("sess-u", "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "is HELD") {
		t.Fatalf("no alert:\n%s", ctx)
	}
	for _, r := range ctx {
		if unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			t.Errorf("character %U reached the context: %q", r, ctx)
		}
	}
}

// The repo name (a routes.json key) is sanitized like the reason.
func TestHeldAlerts_RepoNameIsSanitized(t *testing.T) {
	dir := t.TempDir()
	p := loopFailedPath(dir)
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte("2026-10-26T02:50:29Z "+heldMarker+"x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _ := heldAlerts([]hookTarget{{name: "evil\nINJECTED‮", cfg: config.Config{SyncDir: dir}}}, "s1")
	if len(lines) != 1 || strings.ContainsAny(lines[0], "\n‮") {
		t.Fatalf("name not sanitized: %q", lines)
	}
}

// The per-session marker is read like the record: bounded and regular-file
// only. A FIFO in its place must not block the hook; the alert still goes out.
func TestHook_HeldMarkerFIFOIsSkippedQuickly(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+"check failed\n")
	mark := filepath.Join(cfg.SyncDir, ".state", "hook-markers", "sess-m.held")
	os.MkdirAll(filepath.Dir(mark), 0o700)
	if err := mkfifo(mark); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	in := hookJSON("sess-m", "/home/alice/src/app", "UserPromptSubmit")
	type res struct {
		code int
		out  string
	}
	done := make(chan res, 1)
	go func() {
		code, out, _ := runHook(t, in, "--config", cfgPath, "--hook", "--deadline", "0")
		done <- res{code, out}
	}()
	select {
	case r := <-done:
		if r.code != 0 || !strings.Contains(hookContext(t, r.out), "is HELD") {
			t.Errorf("exit %d context %q: want the alert", r.code, hookContext(t, r.out))
		}
	case <-time.After(3 * time.Second):
		if f, err := os.OpenFile(mark, os.O_WRONLY|syscallNonblock(), 0); err == nil {
			f.Close()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Error("the hook blocked on the session marker (no deadline set)")
	}
}

// The marker is written through a fresh temp file, never a fixed name: a
// symlink planted at the old fixed name must not be followed. The marker
// still works (the second prompt gets no repeat).
func TestHook_HeldMarkerWriteDoesNotFollowSymlink(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+"check failed\n")
	mark := filepath.Join(cfg.SyncDir, ".state", "hook-markers", "sess-s.held")
	os.MkdirAll(filepath.Dir(mark), 0o700)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, mark+".tmp"); err != nil {
		t.Skipf("symlink: %v", err)
	}
	in := hookJSON("sess-s", "/home/alice/src/app", "UserPromptSubmit")
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook", "--deadline", "0"); !strings.Contains(hookContext(t, out), "is HELD") {
		t.Fatalf("first prompt: no alert in %q", hookContext(t, out))
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Errorf("the marker write followed a symlink: victim now %q", b)
	}
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook", "--deadline", "0"); strings.Contains(hookContext(t, out), "is HELD") {
		t.Error("second prompt repeated the alert: the marker was not written")
	}
}
