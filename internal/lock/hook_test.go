package lock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHookPathIsInStateAndSeparateFromTheDaemonLock(t *testing.T) {
	d := t.TempDir()
	if got, want := HookPath(d), filepath.Join(d, ".state", "hook.lock"); got != want {
		t.Errorf("HookPath = %q, want %q", got, want)
	}
	if HookPath(d) == Path(d) {
		t.Error("the hook lock shares a file with the daemon lock")
	}
	// A running daemon must not keep the hook from running, and the reverse.
	dl, err := Acquire(d)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Release()
	hl, held, err := TryHook(d)
	if err != nil || held || hl == nil {
		t.Fatalf("TryHook beside a daemon lock: %v %v %v", hl, held, err)
	}
	hl.Release()
}

func TestTryHookReportsUnusableLockFileAsErrorNotHeld(t *testing.T) {
	d := t.TempDir()
	// .state is a file: the lock directory cannot be made.
	if err := os.WriteFile(filepath.Join(d, ".state"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, held, err := TryHook(d); err == nil || held || l != nil {
		t.Errorf("blocked dir: %v %v %v", l, held, err)
	}

	d2 := t.TempDir()
	// hook.lock is a directory: it cannot be opened read-write.
	if err := os.MkdirAll(HookPath(d2), 0o700); err != nil {
		t.Fatal(err)
	}
	if l, held, err := TryHook(d2); err == nil || held || l != nil {
		t.Errorf("unopenable lock file: %v %v %v", l, held, err)
	}
}

func TestReleaseIsIdempotentAndNilSafe(t *testing.T) {
	var nl *Lock
	nl.Release()
	l, _, err := TryHook(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	l.Release()
}
