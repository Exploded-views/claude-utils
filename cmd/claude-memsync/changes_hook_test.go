package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

const hookSecret = "SECRET-NOTE-CONTENT-91c2"

// framingLine is spelled out here so a change to the wording is deliberate.
const framingLine = "Memory notes changed on another machine since you last looked (read a file before relying on it):"

// addNotes commits one note per project directory name to the sync repo that
// cfgPath points at.
func addNotes(t *testing.T, cfgPath string, notes map[string]string) {
	t.Helper()
	cfg, err := loadDistillConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range notes {
		p := filepath.Join(cfg.SyncDir, "projects", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	repo := gitwt.New(cfg.SyncDir)
	if err := repo.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("auto: test"); err != nil {
		t.Fatal(err)
	}
}

func hookJSON(session, cwd, event string) string {
	b, _ := json.Marshal(map[string]string{"session_id": session, "cwd": cwd, "hook_event_name": event})
	return string(b)
}

func runHook(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = changesCmd(strings.NewReader(stdin), &out, &errb, args)
	return code, out.String(), errb.String()
}

type hookReply struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func markerPath(t *testing.T, cfgPath, session string) string {
	t.Helper()
	cfg, err := loadDistillConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(cfg.SyncDir, ".state", "hook-markers", session)
}

func TestHook_ReportsOnlyNewChanges(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("sess-1", "/home/alice/src/app", "UserPromptSubmit")

	// First call only sets the baseline.
	if code, out, errs := runHook(t, in, "--config", cfgPath, "--hook"); code != 0 || out != "" || errs != "" {
		t.Fatalf("baseline: exit %d stdout %q stderr %q", code, out, errs)
	}
	if _, err := os.Stat(markerPath(t, cfgPath, "sess-1")); err != nil {
		t.Fatalf("marker not written: %v", err)
	}

	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/new.md": hookSecret})
	code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	h := reply.HookSpecificOutput
	cfg, _ := loadDistillConfig(cfgPath)
	want := framingLine + "\nadded " + filepath.Join(cfg.ClaudeProjectsDir, "-home-alice-src-app", "memory", "new.md")
	if h.HookEventName != "UserPromptSubmit" || h.AdditionalContext != want {
		t.Errorf("got %+v, want context %q", h, want)
	}
	if strings.Contains(out, hookSecret) {
		t.Error("note content leaked into hook output")
	}

	// Reported once: the marker moved.
	if code, out, errs := runHook(t, in, "--config", cfgPath, "--hook"); code != 0 || out != "" || errs != "" {
		t.Errorf("repeat: exit %d stdout %q stderr %q", code, out, errs)
	}
}

func TestHook_EventNameIsEchoed(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	runHook(t, hookJSON("s", "/home/alice/src/app", "SessionStart"), "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/new.md": "n"})
	_, out, _ := runHook(t, hookJSON("s", "/home/alice/src/app", "SessionStart"), "--config", cfgPath, "--hook")
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil || reply.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("got %q (%v)", out, err)
	}
}

func TestHook_ProjectDefaultsToCwd(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	app := hookJSON("s", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, app, "--config", cfgPath, "--hook")
	runHook(t, hookJSON("s2", "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{
		"-home-alice-src-app/memory/in-app.md":     "a",
		"-home-alice-src-other/memory/in-other.md": "o",
	})

	_, out, _ := runHook(t, app, "--config", cfgPath, "--hook")
	if !strings.Contains(out, "in-app.md") || strings.Contains(out, "in-other.md") {
		t.Errorf("cwd should limit to its project: %s", out)
	}
	// An explicit --project wins over the cwd.
	_, out, _ = runHook(t, hookJSON("s2", "/home/alice/src/app", "UserPromptSubmit"),
		"--config", cfgPath, "--hook", "--project", "/home/alice/src/other")
	if !strings.Contains(out, "in-other.md") || strings.Contains(out, "in-app.md") {
		t.Errorf("--project should override cwd: %s", out)
	}
}

// Claude's cwd follows its `cd`, so $CLAUDE_PROJECT_DIR is the better default.
func TestHook_ProjectDefaultsToClaudeProjectDir(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("s", "/home/alice/src/app/sub/dir", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{
		"-home-alice-src-app/memory/in-app.md":     "a",
		"-home-alice-src-other/memory/in-other.md": "o",
	})

	t.Setenv("CLAUDE_PROJECT_DIR", "/home/alice/src/app")
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if !strings.Contains(out, "in-app.md") || strings.Contains(out, "in-other.md") {
		t.Errorf("CLAUDE_PROJECT_DIR should limit to its project: %s", out)
	}

	// An explicit --project still wins.
	runHook(t, hookJSON("s2", "/x", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-other/memory/in-other2.md": "o"})
	_, out, _ = runHook(t, hookJSON("s2", "/x", "UserPromptSubmit"), "--config", cfgPath, "--hook", "--project", "/home/alice/src/other")
	if !strings.Contains(out, "in-other2.md") || strings.Contains(out, "in-app.md") {
		t.Errorf("--project should beat CLAUDE_PROJECT_DIR: %s", out)
	}
}

// A name with a newline must not forge extra lines in the context.
func TestHook_QuotesUnprintableNames(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("s", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	evil := "x.md\nadded  forged.md"
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/" + evil: "e"})
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	cfg, _ := loadDistillConfig(cfgPath)
	lines := strings.Split(reply.HookSpecificOutput.AdditionalContext, "\n")
	want := "added " + strconv.Quote(filepath.Join(cfg.ClaudeProjectsDir, "-home-alice-src-app", "memory", evil))
	if len(lines) != 2 || lines[1] != want {
		t.Errorf("lines = %q, want the second to be %q", lines, want)
	}
}

func TestHook_LimitAndOverflow(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("s", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{
		"-home-alice-src-app/memory/n1.md": "1",
		"-home-alice-src-app/memory/n2.md": "2",
		"-home-alice-src-app/memory/n3.md": "3",
	})
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook", "--limit", "1")
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(reply.HookSpecificOutput.AdditionalContext, "\n")
	if len(lines) != 3 || lines[0] != framingLine || !strings.HasPrefix(lines[1], "added ") || lines[2] != "… and 2 more" {
		t.Errorf("lines = %q", lines)
	}
}

func TestHook_InvalidSessionIDIsSilent(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	for _, sid := range []string{"", "../escape", "a/b", "a b", "a\nb", ".."} {
		code, out, errs := runHook(t, hookJSON(sid, "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
		if code != 0 || out != "" || errs != "" {
			t.Errorf("session %q: exit %d stdout %q stderr %q", sid, code, out, errs)
		}
	}
	cfg, _ := loadDistillConfig(cfgPath)
	if _, err := os.Stat(filepath.Join(cfg.SyncDir, ".state")); err == nil {
		t.Error("an invalid session id created state")
	}
	if _, err := os.Stat(filepath.Join(cfg.SyncDir, "escape")); err == nil {
		t.Error("session id escaped the marker directory")
	}
}

// A hook must never block a prompt: no stdout, exit 0, one line on stderr.
func TestHook_ErrorsExitZeroQuietly(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	good := hookJSON("s", "/home/alice/src/app", "UserPromptSubmit")
	cases := map[string][]string{
		"bad JSON":                     {"not json", "--config", cfgPath, "--hook"},
		"empty stdin":                  {"", "--config", cfgPath, "--hook"},
		"no event":                     {hookJSON("s", "/x", ""), "--config", cfgPath, "--hook"},
		"missing config, no sync repo": {good, "--config", filepath.Join(t.TempDir(), "nope.json"), "--hook"},
		"unknown flag":                 {good, "--config", cfgPath, "--hook", "--bogus"},
		"with --since":                 {good, "--config", cfgPath, "--hook", "--since", "HEAD"},
		"with --marker":                {good, "--config", cfgPath, "--hook", "--marker", "m"},
		"removed flag":                 {good, "--config", cfgPath, "--hook", "--hook-cleanup"},
		"negative --limit":             {good, "--config", cfgPath, "--hook", "--limit", "-1"},
	}
	for name, c := range cases {
		code, out, errs := runHook(t, c[0], c[1:]...)
		if code != 0 || out != "" {
			t.Errorf("%s: exit %d stdout %q", name, code, out)
		}
		if errs == "" {
			t.Errorf("%s: want a line on stderr", name)
		}
	}
}

func TestHook_StaleMarkerIsResetWithAWarning(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	m := markerPath(t, cfgPath, "s")
	if err := os.MkdirAll(filepath.Dir(m), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m, []byte(strings.Repeat("ab", 20)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runHook(t, hookJSON("s", "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	if code != 0 || out != "" || strings.Count(strings.TrimSpace(errs), "\n") != 0 || errs == "" {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errs)
	}
	cfg, _ := loadDistillConfig(cfgPath)
	head, _ := gitwt.New(cfg.SyncDir).Run("rev-parse", "HEAD")
	if b, _ := os.ReadFile(m); strings.TrimSpace(string(b)) != strings.TrimSpace(head) {
		t.Errorf("marker = %q, want HEAD", b)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// If the report cannot be delivered the marker must stay put, so the next
// prompt reports the same changes again.
func TestHook_MarkerMovesOnlyAfterOutputIsWritten(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("s", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	m := markerPath(t, cfgPath, "s")
	before, _ := os.ReadFile(m)
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/new.md": "n"})

	var errb bytes.Buffer
	if code := changesCmd(strings.NewReader(in), failWriter{}, &errb, []string{"--config", cfgPath, "--hook"}); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if errb.Len() == 0 {
		t.Error("want a line on stderr")
	}
	if after, _ := os.ReadFile(m); !bytes.Equal(before, after) {
		t.Errorf("marker moved although the output failed: %q -> %q", before, after)
	}
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); !strings.Contains(out, "new.md") {
		t.Errorf("change should be reported again: %q", out)
	}
}

// A hook marker that is not a marker (something else at that path) is left
// alone: one line on stderr, nothing on stdout, exit 0.
func TestHook_NonMarkerFileIsLeftAlone(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	m := markerPath(t, cfgPath, "s")
	if err := os.MkdirAll(filepath.Dir(m), 0700); err != nil {
		t.Fatal(err)
	}
	const note = "# My notes\nimportant-77ab\n"
	if err := os.WriteFile(m, []byte(note), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runHook(t, hookJSON("s", "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	if code != 0 || out != "" || errs == "" || strings.Contains(errs, "important-77ab") || strings.Count(strings.TrimSpace(errs), "\n") != 0 {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errs)
	}
	if b, _ := os.ReadFile(m); string(b) != note {
		t.Errorf("file was overwritten: %q", b)
	}
}

// A panic must never escape: Claude Code treats exit 2 as "block the prompt".
func TestHook_PanicExitsZero(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	hookTestHook = func() { panic("boom") }
	t.Cleanup(func() { hookTestHook = nil })
	code, out, errs := runHook(t, hookJSON("s", "/home/alice/src/app", "UserPromptSubmit"), "--config", cfgPath, "--hook")
	if code != 0 || out != "" || errs == "" {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errs)
	}
}

// Markers of sessions that never come back are pruned after 30 days; the
// current session's marker is never pruned, since a resumed session needs it.
func TestHook_PrunesOldMarkers(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	in := hookJSON("cur", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	dir := filepath.Dir(markerPath(t, cfgPath, "cur"))
	head, _ := os.ReadFile(markerPath(t, cfgPath, "cur"))
	for _, name := range []string{"old", "fresh"} {
		if err := os.WriteFile(filepath.Join(dir, name), head, 0600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-31 * 24 * time.Hour)
	for _, name := range []string{"old", "cur"} {
		if err := os.Chtimes(filepath.Join(dir, name), past, past); err != nil {
			t.Fatal(err)
		}
	}
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/new.md": "n"})

	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if !strings.Contains(out, "new.md") {
		t.Errorf("the current session's old marker should still work: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old marker not pruned: %v", err)
	}
	for _, name := range []string{"fresh", "cur"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s marker should stay: %v", name, err)
		}
	}
}
