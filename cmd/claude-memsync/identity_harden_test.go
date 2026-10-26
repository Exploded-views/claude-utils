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
	// start from a nonzero soft limit, or the test proves nothing
	up := old
	if up.Max == 0 {
		t.Skip("hard core limit is 0 here")
	}
	up.Cur = 1 << 20
	if up.Max < up.Cur {
		up.Cur = up.Max
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &up); err != nil {
		t.Skip(err)
	}
	hardenProcess()
	var got syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cur != 0 {
		t.Fatalf("RLIMIT_CORE soft limit = %d, want 0", got.Cur)
	}
}

func TestIdentityBackup_HardensTheProcessBeforeAnyKeyIsReadOrStored(t *testing.T) {
	read := false
	oldRead := readIdentityFn
	readIdentityFn = func(p string) (*idFileInfo, error) { read = true; return oldRead(p) }
	t.Cleanup(func() { readIdentityFn = oldRead })
	path, _, _ := idFile(t, t.TempDir(), "identity.txt", false)
	store, _, rec := fakeStores(t)
	calls := 0
	harden := func() {
		calls++
		if read {
			t.Error("an identity file was read before the process was hardened")
		}
		if got := recorded(t, rec, "stdin"); len(got) != 0 {
			t.Error("a store command ran before the process was hardened")
		}
	}
	if code, _, errs := runIDBackupHarden(harden, "--identity", path, "--store-cmd", store); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if calls != 1 {
		t.Fatalf("hardenFn called %d times, want 1", calls)
	}
	if !read {
		t.Fatal("the read seam was never used, so the order was not checked")
	}
	// a usage error or -h never gets as far as a key
	calls = 0
	runIDBackupHarden(harden, "--identity", path)
	runIDBackupHarden(harden, "-h")
	if calls != 0 {
		t.Fatalf("hardenFn called %d times for a usage error or -h, want 0", calls)
	}
}
