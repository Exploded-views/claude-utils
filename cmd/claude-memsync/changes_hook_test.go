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
const framingLine = "Memory notes changed in the sync repo (this or another machine) since you last looked (read a file before relying on it):"

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

// HEAD moved outside a cycle (a hand `git pull`/rebase, or a propagate that
// failed): .state/synced-head still names the older commit, so ~/.claude has
// not got the new file yet. The hook reports it at once and advances its
// marker; once catch-up has copied the file, nothing is reported again. The
// session was told to read a file that was not there yet, and never told again.
func TestHook_WaitsForSyncedHead(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	repo := gitwt.New(cfg.SyncDir)
	in := hookJSON("sess-sh", "/home/alice/src/app", "UserPromptSubmit")
	if code, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); code != 0 || out != "" {
		t.Fatalf("baseline: %d %q", code, out)
	}
	rec, _ := repo.Run("rev-parse", "HEAD")
	state := filepath.Join(cfg.SyncDir, ".state")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "synced-head"), []byte(rec), 0600); err != nil {
		t.Fatal(err)
	}

	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/late.md": "late"})
	local := filepath.Join(cfg.ClaudeProjectsDir, "-home-alice-src-app", "memory", "late.md")

	reportedWhileCurrent := false
	_, out1, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if strings.Contains(out1, "late.md") {
		if _, err := os.Stat(local); err == nil {
			reportedWhileCurrent = true
		}
	}

	// Catch-up runs: the file reaches ~/.claude and the record moves to HEAD.
	if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("late"), 0600); err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(state, "synced-head"), []byte(head), 0600); err != nil {
		t.Fatal(err)
	}
	_, out2, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if strings.Contains(out2, "late.md") {
		reportedWhileCurrent = true
	}
	if !reportedWhileCurrent {
		t.Errorf("late.md was never reported while ~/.claude had it (first report %q came before catch-up, second %q)", out1, out2)
	}
}

// HEAD moves back to an ancestor of the marker (git reset by hand, or the
// fork-point rebase onto an upstream that was force-pushed back to drop a
// commit). The difference (purged.md going away) is reported once, the
// marker follows HEAD back, and what comes next is reported once.
func TestHook_HeadMovedBackIsReportedOnce(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	repo := gitwt.New(cfg.SyncDir)
	in := hookJSON("sess-back", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	older, _ := repo.Run("rev-parse", "HEAD")
	older = strings.TrimSpace(older)
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/purged.md": "p"})
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); !strings.Contains(out, "purged.md") {
		t.Fatalf("setup: purged.md not reported: %q", out)
	}
	if _, err := repo.Run("reset", "--hard", older); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"deleted", ""} {
		code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
		if code != 0 || errs != "" || (want == "" && out != "") || (want != "" && !strings.Contains(out, want+" ")) {
			t.Fatalf("after reset, run %d: exit %d stdout %q stderr %q", i, code, out, errs)
		}
	}
	if mb, _ := os.ReadFile(markerPath(t, cfgPath, "sess-back")); strings.TrimSpace(string(mb)) != older {
		t.Errorf("marker = %q, want it moved back to %s", mb, older)
	}
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/next.md": "n"})
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if !strings.Contains(out, "next.md") || strings.Contains(out, "purged.md") {
		t.Errorf("after the next commit: %q", out)
	}
}

// A detached HEAD (the user is mid-way through a hand resolution) is not
// recorded in synced-head. The hook reports up to the record, not up to the
// detached commit.
func TestHook_DetachedHead(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	repo := gitwt.New(cfg.SyncDir)
	in := hookJSON("sess-det", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	rec, _ := repo.Run("rev-parse", "HEAD")
	writeSyncedHead(t, cfg.SyncDir, rec)
	if _, err := repo.Run("checkout", "--detach"); err != nil {
		t.Fatal(err)
	}
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/detached.md": "d"})
	if code, out, errs := runHook(t, in, "--config", cfgPath, "--hook"); code != 0 || out != "" || errs != "" {
		t.Fatalf("detached, ahead of the record: exit %d stdout %q stderr %q", code, out, errs)
	}

	// Without a record a detached HEAD is still a commit: report up to it.
	if err := os.Remove(filepath.Join(cfg.SyncDir, ".state", "synced-head")); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); !strings.Contains(out, "detached.md") {
		t.Errorf("no record, detached HEAD: %q", out)
	}
}

// A record that names no commit is no bound at all.
func TestHook_UnresolvableSyncedHeadFallsBackToHead(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	in := hookJSON("sess-bad", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeSyncedHead(t, cfg.SyncDir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n")
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/x.md": "x"})
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); !strings.Contains(out, "x.md") {
		t.Errorf("got %q", out)
	}
}

func writeSyncedHead(t *testing.T, syncDir, rec string) {
	t.Helper()
	state := filepath.Join(syncDir, ".state")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "synced-head"), []byte(strings.TrimSpace(rec)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// ignoreState gives the test repo the .gitignore init writes, so .state and
// the denylist are never committed by addNotes.
func ignoreState(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".state/\ndenylist.txt\nconfig.json\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// A memory file whose name matches this PC's denylist can reach HEAD from a
// peer (the denylist is per-PC) or an older client. The daemon keeps such a
// name out of its logs; the hook must keep it out of Claude's context too, and
// say how many were left out.
func TestHook_DenylistedNameIsCountedNotListed(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	if err := os.WriteFile(cfg.DenylistPath(), []byte("# clients\nacmecorp\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in := hookJSON("sess-deny", "/home/alice/src/app", "UserPromptSubmit")
	if code, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); code != 0 || out != "" {
		t.Fatalf("baseline: %d %q", code, out)
	}
	addNotes(t, cfgPath, map[string]string{
		"-home-alice-src-app/memory/acmecorp_contract.md": "x",
		"-home-alice-src-app/memory/ACMECORP_two.md":      "x",
		"-home-alice-src-app/memory/fine.md":              "x",
	})
	code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	if strings.Contains(strings.ToLower(out), "acmecorp") {
		t.Errorf("hook output names a denylisted file: %s", out)
	}
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	lines := strings.Split(reply.HookSpecificOutput.AdditionalContext, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "added ") || !strings.HasSuffix(lines[1], "fine.md") ||
		lines[2] != "2 changed file(s) whose name matches your denylist are not listed" {
		t.Errorf("context lines: %q", lines)
	}
}

// Only denylisted names changed: the count is the whole report.
func TestHook_OnlyDenylistedNamesChanged(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	os.WriteFile(cfg.DenylistPath(), []byte("acmecorp\n"), 0600)
	in := hookJSON("sess-deny1", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/acmecorp.md": "x"})
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	want := framingLine + "\n1 changed file(s) whose name matches your denylist are not listed"
	if got := reply.HookSpecificOutput.AdditionalContext; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A denylist that exists but cannot be used: list no names, counts only, one
// line on stderr, and still exit 0.
func TestHook_UnparseableDenylistFailsClosed(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	os.WriteFile(cfg.DenylistPath(), []byte("acmecorp\n(unclosed\n"), 0600)
	in := hookJSON("sess-badden", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{
		"-home-alice-src-app/memory/acmecorp_contract.md": "x",
		"-home-alice-src-app/memory/fine.md":              "x",
	})
	code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "acmecorp") || strings.Contains(out, "fine.md") {
		t.Errorf("names listed despite an unusable denylist: %s", out)
	}
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got := reply.HookSpecificOutput.AdditionalContext; !strings.HasPrefix(got, framingLine+"\n2 changed file(s)") || strings.Count(got, "\n") != 1 {
		t.Errorf("context: %q", got)
	}
	if strings.Count(errs, "\n") != 1 || !strings.HasPrefix(errs, "changes: ") || strings.Contains(errs, "(unclosed") {
		t.Errorf("stderr should be one line without the pattern: %q", errs)
	}
}

// A denylist that is a directory (unreadable as a file) is the same.
func TestHook_UnreadableDenylistFailsClosed(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	if err := os.Mkdir(cfg.DenylistPath(), 0700); err != nil {
		t.Fatal(err)
	}
	in := hookJSON("sess-dirden", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/fine.md": "x"})
	_, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	if strings.Contains(out, "fine.md") || !strings.Contains(out, "1 changed file(s)") || strings.Count(errs, "\n") != 1 {
		t.Errorf("stdout %q stderr %q", out, errs)
	}
}

// No denylist file: nothing is hidden.
func TestHook_NoDenylistListsEverything(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	in := hookJSON("sess-noden", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/acmecorp.md": "x"})
	code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	if code != 0 || errs != "" || !strings.Contains(out, "acmecorp.md") || strings.Contains(out, "denylist") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errs)
	}
}

// A held-back path that a peer changed: the daemon kept the local copy and
// saved the incoming version beside the mirror copy. The report says so
// instead of pointing at a file that does not hold the peer's text.
func TestHook_LocalCopyKeptIsAnnotated(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	in := hookJSON("sess-kept", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	addNotes(t, cfgPath, map[string]string{
		"-home-alice-src-app/memory/held.md":   "from the peer",
		"-home-alice-src-app/memory/copied.md": "copied",
	})
	mem := filepath.Join(cfg.ClaudeProjectsDir, "-home-alice-src-app", "memory")
	if err := os.MkdirAll(mem, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(mem, "held.md"), []byte("my local edit"), 0600)
	os.WriteFile(filepath.Join(mem, "copied.md"), []byte("copied"), 0600)

	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	want := "(the copy on this PC differs from the synced version: a local edit not yet synced, or an incoming change that was held back)"
	for _, l := range strings.Split(reply.HookSpecificOutput.AdditionalContext, "\n")[1:] {
		if strings.HasSuffix(l, "copied.md") == false && strings.Contains(l, "copied.md") {
			t.Errorf("copied.md carries a note: %q", l)
		}
	}
	if !strings.Contains(reply.HookSpecificOutput.AdditionalContext, "held.md "+want) {
		t.Errorf("held.md not annotated: %q", reply.HookSpecificOutput.AdditionalContext)
	}
}
