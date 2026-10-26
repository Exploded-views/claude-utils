//go:build !windows

package main

import (
	"syscall"
	"testing"
)

func TestHardenProcess_DisablesCoreDumps(t *testing.T) {
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &old); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_CORE, &old) })
	hardenProcess()
	var got syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cur != 0 {
		t.Fatalf("RLIMIT_CORE soft limit = %d, want 0", got.Cur)
	}
}

func TestIdentityBackup_HardensTheProcessBeforeReadingKeys(t *testing.T) {
	calls := 0
	old := hardenFn
	hardenFn = func() { calls++ }
	t.Cleanup(func() { hardenFn = old })
	path, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	store, _, _ := fakeStores(t)
	if code, _, errs := runIDBackup("--identity", path, "--store-cmd", store); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if calls != 1 {
		t.Fatalf("hardenFn called %d times, want 1", calls)
	}
	// a usage error or -h never gets as far as a key
	calls = 0
	runIDBackup("--identity", path)
	runIDBackup("-h")
	if calls != 0 {
		t.Fatalf("hardenFn called %d times for a usage error or -h, want 0", calls)
	}
}
