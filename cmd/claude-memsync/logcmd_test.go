package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

func runLogT(t *testing.T, stdin string, args ...string) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	code = logCmd(&o, &e, strings.NewReader(stdin), args)
	return code, o.String(), e.String()
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "log.md")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLogCheck_ExitStatus(t *testing.T) {
	clean := writeLog(t, "# t\n- 2026-11-02 18:30Z ok\n")
	if code, out, errs := runLogT(t, "", "check", clean); code != 0 || out != "" || errs != "" {
		t.Fatalf("clean: %d %q %q", code, out, errs)
	}
	bad := writeLog(t, "- 2026-11-02 18:30Z ok\n- 2026-11-02 ev-785: x\n")
	code, out, _ := runLogT(t, "", "check", bad)
	if code != 1 {
		t.Fatalf("problems: code %d, want 1", code)
	}
	if !strings.HasPrefix(out, bad+":2: ") || strings.Count(out, "\n") != 1 {
		t.Fatalf("output = %q, want one %s:2: line", out, bad)
	}
	missing := filepath.Join(t.TempDir(), "nope.md")
	if code, _, errs := runLogT(t, "", "check", missing); code != 2 || errs == "" {
		t.Fatalf("missing: %d %q", code, errs)
	}
	if code, _, _ := runLogT(t, "", "check"); code != 2 {
		t.Fatalf("no args: %d, want 2", code)
	}
	// A read error outranks problems elsewhere.
	if code, _, _ := runLogT(t, "", "check", bad, missing); code != 2 {
		t.Fatalf("problems plus read error: %d, want 2", code)
	}
}

func TestLogAppend_ExitStatus(t *testing.T) {
	nowFn = func() time.Time { return time.Date(2026, 11, 2, 11, 30, 59, 0, time.FixedZone("PDT", -7*3600)) }
	defer func() { nowFn = time.Now }()

	p := writeLog(t, "")
	if code, _, errs := runLogT(t, "", "append", p, "hello", "world"); code != 0 {
		t.Fatalf("success: %d %q", code, errs)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "- 2026-11-02 18:30Z hello world\n" {
		t.Fatalf("file = %q", b)
	}

	before, _ := os.ReadFile(p)
	if code, _, errs := runLogT(t, "", "append", p, "a\nb"); code != 2 || errs == "" {
		t.Fatalf("newline text: %d %q", code, errs)
	}
	if code, _, _ := runLogT(t, "", "append", p); code != 2 { // no text, empty stdin
		t.Fatalf("no text: %d", code)
	}
	if code, _, _ := runLogT(t, "", "append"); code != 2 {
		t.Fatalf("no file: %d", code)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, before) {
		t.Fatalf("file changed: %q", after)
	}

	broken := writeLog(t, "- 2026-11-02 18:29Z no newline")
	if code, _, errs := runLogT(t, "", "append", broken, "x"); code != 1 || !strings.Contains(errs, "log check") {
		t.Fatalf("no final newline: %d %q", code, errs)
	}

	code, out, errs := runLogT(t, "", "append", "-h")
	if code != 0 || !strings.Contains(out+errs, "usage: claude-memsync log append") {
		t.Fatalf("-h: %d %q %q", code, out, errs)
	}
	if code, _, _ := runLogT(t, "", "append", "--bogus", p, "x"); code != 2 {
		t.Fatalf("bad flag: %d, want 2", code)
	}
}

func TestLogAppend_WaitFlag(t *testing.T) {
	p := writeLog(t, "")
	if code, _, errs := runLogT(t, "", "append", "--wait", "1s", p, "x"); code != 0 {
		t.Fatalf("--wait: %d %q", code, errs)
	}
	if code, _, _ := runLogT(t, "", "append", "--wait", "nonsense", p, "x"); code != 2 {
		t.Fatalf("bad --wait: %d, want 2", code)
	}
}

func TestLogAppend_StdinOneLine(t *testing.T) {
	p := writeLog(t, "")
	if code, _, errs := runLogT(t, "from stdin\n", "append", p); code != 0 {
		t.Fatalf("stdin: %d %q", code, errs)
	}
	if b, _ := os.ReadFile(p); !strings.HasSuffix(string(b), "Z from stdin\n") {
		t.Fatalf("file = %q", b)
	}
	before, _ := os.ReadFile(p)
	if code, _, _ := runLogT(t, "x\ny\n", "append", p); code != 2 {
		t.Fatalf("two stdin lines: %d, want 2", code)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, before) {
		t.Fatal("file changed after a refused stdin")
	}
}

func TestLogAppend_ClockSeam(t *testing.T) {
	nowFn = func() time.Time { return time.Date(2030, 1, 2, 3, 4, 59, 0, time.UTC) }
	defer func() { nowFn = time.Now }()
	p := writeLog(t, "")
	runLogT(t, "", "append", p, "x")
	if b, _ := os.ReadFile(p); string(b) != "- 2030-01-02 03:04Z x\n" {
		t.Fatalf("file = %q", b)
	}
}

func TestLog_UsageAndUnknown(t *testing.T) {
	if code, _, errs := runLogT(t, ""); code != 2 || !strings.Contains(errs, "claude-memsync log append") || !strings.Contains(errs, "log check") {
		t.Fatalf("no args: %d %q", code, errs)
	}
	if code, _, errs := runLogT(t, "", "nope"); code != 2 || !strings.Contains(errs, "nope") {
		t.Fatalf("unknown: %d %q", code, errs)
	}
	if code, out, errs := runLogT(t, "", "-h"); code != 0 || !strings.Contains(out+errs, "log append") {
		t.Fatalf("-h: %d %q %q", code, out, errs)
	}
	if code, out, errs := runLogT(t, "", "check", "-h"); code != 0 || !strings.Contains(out+errs, "usage: claude-memsync log check") {
		t.Fatalf("check -h: %d %q %q", code, out, errs)
	}
}

// Wiring: main dispatches "log" and usage() lists it. Built binary, so os.Exit
// statuses are the real ones.
func TestLog_MainWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "claude-memsync")
	_, thisFile, _, _ := runtime.Caller(0)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Dir(thisFile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	p := filepath.Join(t.TempDir(), "log.md")
	home := t.TempDir()
	run := func(args ...string) (int, string) {
		c := exec.Command(bin, args...)
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
		out, err := c.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return 0, string(out)
	}
	if code, out := run("log", "append", p, "hi"); code != 0 {
		t.Fatalf("append: %d %s", code, out)
	}
	if code, out := run("log", "check", p); code != 0 || out != "" {
		t.Fatalf("check: %d %s", code, out)
	}
	os.WriteFile(p, []byte("junk\n"), 0600)
	if code, out := run("log", "check", p); code != 1 || !strings.HasPrefix(out, p+":1: ") {
		t.Fatalf("check bad: %d %s", code, out)
	}
	if _, out := run("help"); !strings.Contains(out, "  log ") {
		t.Fatalf("usage lacks the log line:\n%s", out)
	}
}

func TestLogAppend_WaitReachesTheLockTimeout(t *testing.T) {
	p := writeLog(t, "")
	f, _ := os.OpenFile(p, os.O_RDWR, 0)
	defer f.Close()
	rel, err := lock.WaitFile(f, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, _, errs := runLogT(t, "", "append", "--wait", "100ms", p, "x")
	if code != 1 || time.Since(start) > 2*time.Second || !strings.Contains(errs, "timed out") {
		t.Fatalf("code %d after %v: %q", code, time.Since(start), errs)
	}
	go func() { time.Sleep(200 * time.Millisecond); rel() }()
	if code, _, errs := runLogT(t, "", "append", "--wait", "3s", p, "x"); code != 0 {
		t.Fatalf("--wait 3s: %d %q", code, errs)
	}
}

func TestLogAppend_StdinOverTheCapIsRefusedNotTruncated(t *testing.T) {
	p := writeLog(t, "")
	if code, _, _ := runLogT(t, strings.Repeat("a", 5000)+"\n", "append", p); code != 2 {
		t.Fatalf("code %d, want 2", code)
	}
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatalf("wrote %d bytes", len(b))
	}
}

func TestLogCheck_DoubleDash(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile("-dash.md", []byte("- 2026-11-02 18:30Z ok\n"), 0600)
	if code, _, errs := runLogT(t, "", "check", "--", "-dash.md"); code != 0 {
		t.Fatalf("check -- file: %d %q", code, errs)
	}
	if code, _, _ := runLogT(t, "", "check", "-dash.md"); code != 2 {
		t.Fatalf("a bare -dash.md should be a usage error")
	}
}
