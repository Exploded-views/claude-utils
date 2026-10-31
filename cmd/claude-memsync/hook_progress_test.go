package main

import (
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

func TestHookProgress_CloseFreezesAndSorts(t *testing.T) {
	var p hookProgress
	if !p.add(2, repoReport{marker: "c"}) || !p.add(0, repoReport{marker: "a"}) {
		t.Fatal("add refused before close")
	}
	done, lk := p.close()
	if lk != nil || len(done) != 2 || done[0].rep.marker != "a" || done[1].rep.marker != "c" {
		t.Fatalf("close gave %+v, lock %v; want a, c in target order and no lock", done, lk)
	}
	if p.add(1, repoReport{marker: "late"}) {
		t.Error("add accepted a late result after close")
	}
	if got, _ := p.close(); len(got) != 2 {
		t.Errorf("a late result changed what close returns: %+v", got)
	}
	if p.setLock(nil) {
		t.Error("setLock accepted a lock after close")
	}
}

// After close the lock is the caller's: the worker's own release must leave it
// held until the caller releases it.
func TestHookProgress_WorkerReleaseAfterCloseKeepsTheLock(t *testing.T) {
	dir := t.TempDir()
	lk, held, err := lock.TryHook(dir)
	if err != nil || held {
		t.Fatalf("TryHook: %v held=%v", err, held)
	}
	var p hookProgress
	if !p.setLock(lk) {
		t.Fatal("setLock refused")
	}
	_, got := p.close()
	if got != lk {
		t.Fatal("close did not hand over the lock")
	}
	p.releaseIfOwned(lk) // the worker's deferred release
	if l2, held, _ := lock.TryHook(dir); !held {
		l2.Release()
		t.Fatal("the worker released a lock close had handed to the caller")
	}
	got.Release()
	l3, held, err := lock.TryHook(dir)
	if err != nil || held {
		t.Fatalf("after the caller's release: %v held=%v", err, held)
	}
	l3.Release()
}

func TestHookProgress_WorkerReleasesItsOwnLockWithoutClose(t *testing.T) {
	dir := t.TempDir()
	lk, _, _ := lock.TryHook(dir)
	var p hookProgress
	p.setLock(lk)
	p.releaseIfOwned(lk)
	l2, held, err := lock.TryHook(dir)
	if err != nil || held {
		t.Fatalf("the worker's release did not free the lock: %v held=%v", err, held)
	}
	l2.Release()
}
