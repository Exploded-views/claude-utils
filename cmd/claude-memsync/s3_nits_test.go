package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// With the automatic stop, a held lock that survives "service not running"
// belongs to something else; the advice must not be stop/start.
func TestDistillPrune_HeldLockAdviceDoesNotSayStopStart(t *testing.T) {
	cfgs := routedHost(t, []string{"personal"}, "personal", nil)
	f := &fakeSvc{status: "stopped"}
	installFakeSvc(t, f)
	held, err := lock.Acquire(cfgs["personal"].SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var code int
	errs := captureStderr(t, func() { code = runDistill([]string{"--prune"}) })
	if code == 0 {
		t.Fatal("prune under a held lock must be refused")
	}
	if strings.Contains(errs, "claude-memsync stop") || strings.Contains(errs, "claude-memsync start") {
		t.Errorf("stale stop/start advice:\n%s", errs)
	}
	if !strings.Contains(errs, "something other than the claude-memsync service") {
		t.Errorf("the message must say the lock is held by something else:\n%s", errs)
	}
}

// After the automatic restart the service is checked: a daemon that exited (for
// example refused by the foreign-writer guard) is a warning, not "restarted".
func TestSyncLockStopping_RestartedServiceThatExitedIsAWarning(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	oldSettle := restartSettle
	restartSettle = 0
	t.Cleanup(func() { restartSettle = oldSettle })
	started := false
	startFn = func() error { f.calls = append(f.calls, "start"); started = true; return nil }
	statusFn = func() (string, error) {
		if started {
			return "stopped", nil
		}
		return "running", nil
	}
	dir := heldLock(t, f)
	var errb bytes.Buffer
	if err := withSyncLockStopping(&errb, dir, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	out := errb.String()
	if strings.Contains(out, "service restarted") {
		t.Errorf("said restarted for a service that is not running:\n%s", out)
	}
	if !strings.Contains(out, "not running") || !strings.Contains(out, "claude-memsync status") {
		t.Errorf("want a warning that the service is not running, pointing at status:\n%s", out)
	}
}

func TestSyncLockStopping_RestartedServiceThatRunsIsReported(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	oldSettle := restartSettle
	restartSettle = 0
	t.Cleanup(func() { restartSettle = oldSettle })
	dir := heldLock(t, f)
	var errb bytes.Buffer
	if err := withSyncLockStopping(&errb, dir, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "claude-memsync service restarted") {
		t.Errorf("want the restarted line:\n%s", errb.String())
	}
}

// label ... --all: one stop and one start for the whole loop, not per repo.
func TestLabelSelfAll_StopsAndRestartsTheServiceOnce(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	a := encryptRouted(t, cfgs["personal"])
	b := encryptRouted(t, cfgs["projects"])
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	// The fake daemon holds both locks, drops them when stopped and takes them
	// again when started, as the real service does.
	var held []*lock.Lock
	take := func() {
		for _, d := range []string{a.SyncDir, b.SyncDir} {
			l, err := lock.Acquire(d)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, l)
		}
	}
	drop := func() {
		for _, l := range held {
			l.Release()
		}
		held = nil
	}
	take()
	t.Cleanup(drop)
	f.onStop = drop
	startFn = func() error { f.calls = append(f.calls, "start"); take(); return nil }
	code, out, errs := runRcpt("label", "self", "--host", "vm-1", "--all")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if f.did() != "stop,start" {
		t.Errorf("service calls = %q, want exactly one stop and one start", f.did())
	}
	if n := strings.Count(errs, "stopping the claude-memsync service"); n != 1 {
		t.Errorf("stop announced %d times:\n%s", n, errs)
	}
}

// join status on a host with an unreviewed change also prints the
// .gitattributes notice that recipients accept shows.
func TestJoinStatus_UnreviewedPrintsTheGitattributesNotice(t *testing.T) {
	h := twoHosts(t)
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	fp := fpOf(out)
	runJoinCmd("list", "--config", h.a.Path())
	if _, err := h.ra.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--allow-unlabelled", "--fingerprint", fp); code != 0 {
		t.Fatalf("accept: %s", errs)
	}
	if _, err := h.rb.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	_ = joinCmd(&o, &e, []string{"status", "--config", h.b.Path()})
	if !strings.Contains(o.String()+e.String(), ".gitattributes") {
		t.Errorf("join status lacks the .gitattributes notice:\nstdout: %s\nstderr: %s", o.String(), e.String())
	}
}
