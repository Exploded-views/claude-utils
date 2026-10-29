package main

import (
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// A hook that loses the hook lock does no git work, but a held-repo alert
// needs no lock: the session still hears it on that prompt, and only once.
func TestHook_LockHeldStillDeliversThePendingHeldAlertOnce(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	in := hookJSON("sess-lk", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook") // baseline
	writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+"check failed: HTTP 404\n")

	lk, held, err := lock.TryHook(cfg.SyncDir)
	if err != nil || held {
		t.Fatalf("take the hook lock: held=%v err=%v", held, err)
	}
	code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	if code != 0 || !strings.Contains(hookContext(t, out), "is HELD") {
		t.Fatalf("exit %d stderr %q stdout %q; the lock-held hook dropped the held alert", code, errs, out)
	}
	if strings.Contains(hookContext(t, out), framingLine) {
		t.Errorf("the lock-held hook printed change notices:\n%s", out)
	}
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); strings.Contains(hookContext(t, out), "is HELD") {
		t.Error("the alert was repeated on the next lock-held prompt")
	}
	lk.Release()
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); strings.Contains(hookContext(t, out), "is HELD") {
		t.Error("the alert was repeated once the lock was free")
	}
}
