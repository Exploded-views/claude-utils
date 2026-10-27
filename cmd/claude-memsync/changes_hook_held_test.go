package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
