package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// fastGuardRetry makes the private-only retry delay tiny for one test.
func fastGuardRetry(t *testing.T) {
	t.Helper()
	oldMin, oldMax := guardBackoffMin, guardBackoffMax
	guardBackoffMin, guardBackoffMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { guardBackoffMin, guardBackoffMax = oldMin, oldMax })
}

// startedLoops stubs runLoop: it records which repos' loops started (and each
// loop's PushAllowed at that moment) and blocks until the daemon stops.
type startedLoops struct {
	mu   sync.Mutex
	runs map[string]int
	push map[string]bool
	at   map[string]int32 // a counter's value when the loop first started
	ctr  *atomic.Int32
}

func stubLoops(t *testing.T, ctr *atomic.Int32) *startedLoops {
	t.Helper()
	s := &startedLoops{runs: map[string]int{}, push: map[string]bool{}, at: map[string]int32{}, ctr: ctr}
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error {
		s.mu.Lock()
		s.runs[l.Name]++
		s.push[l.Name] = l.PushAllowed != nil
		if s.ctr != nil && s.runs[l.Name] == 1 {
			s.at[l.Name] = s.ctr.Load()
		}
		s.mu.Unlock()
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { runLoop = old })
	return s
}

func (s *startedLoops) started(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[name]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A repo that fails the private-only check is held, not the whole daemon:
// Start succeeds, the good repo syncs, the failing ones never start a loop,
// and status names why. A config refusal (not a mem-* name) and an API error
// (no answer is not "private") both hold.
func TestRouted_GuardFailureHoldsOnlyThatRepo(t *testing.T) {
	fastGuardRetry(t)
	var checks atomic.Int32
	api := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mem-project-c") {
			checks.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "x"})
			return
		}
		routedAPI("")(w, r)
	}
	cfgs := privateRouted(t, map[string]string{
		"a": "git@github.com:Org/mem-project-a.git",
		"b": "git@github.com:Org/claude-memories.git", // not a mem-* name
		"c": "git@github.com:Org/mem-project-c.git",   // the API fails for it
	}, api)
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	loops := stubLoops(t, nil)
	r, err := set.Start(context.Background())
	if err != nil {
		t.Fatalf("a repo failing the private-only check must not refuse the daemon: %v", err)
	}
	waitFor(t, "repo a to start", func() bool { return loops.started("a") == 1 })
	waitFor(t, "repo c to be re-checked", func() bool { return checks.Load() >= 4 })
	if n := loops.started("b"); n != 0 {
		t.Errorf("repo b (refused remote) started %d times", n)
	}
	if n := loops.started("c"); n != 0 {
		t.Errorf("repo c (API error) started %d times; an error must never read as private", n)
	}
	out := strings.Join(set.statusLines(), "\n")
	for _, n := range []string{"b", "c"} {
		b, err := os.ReadFile(loopFailedPath(cfgs[n].SyncDir))
		if err != nil || !strings.Contains(string(b), "private-only check failed") {
			t.Errorf("repo %s: state file = %q, %v; want the private-only failure", n, b, err)
		}
	}
	if !strings.Contains(out, "private-only check failed") {
		t.Errorf("status does not show the held repos:\n%s", out)
	}
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	for n, c := range cfgs {
		l, err := lock.Acquire(c.SyncDir)
		if err != nil {
			t.Fatalf("repo %s left locked after Stop: %v", n, err)
		}
		l.Release()
	}
}

// A held repo starts once a later check passes, and never before it: the loop
// starts only after the first passing answer, gets its push guard, and the
// held state is cleared.
func TestRouted_HeldRepoStartsOnlyAfterTheCheckPasses(t *testing.T) {
	fastGuardRetry(t)
	var calls atomic.Int32
	const failing = 3
	api := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mem-project-b") {
			n := calls.Add(1)
			pub := n <= failing
			_ = json.NewEncoder(w).Encode(repoJSON{Name: "mem-project-b", Private: tb(!pub), Visibility: map[bool]string{true: "public", false: "private"}[pub]})
			return
		}
		routedAPI("")(w, r)
	}
	cfgs := privateRouted(t, map[string]string{
		"a": "git@github.com:Org/mem-project-a.git",
		"b": "git@github.com:Org/mem-project-b.git",
	}, api)
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	loops := stubLoops(t, &calls)
	r, err := set.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	waitFor(t, "repo b to start", func() bool { return loops.started("b") == 1 })
	loops.mu.Lock()
	at, push := loops.at["b"], loops.push["b"]
	loops.mu.Unlock()
	if at <= failing {
		t.Errorf("repo b started after %d checks; the first %d said public", at, failing)
	}
	if !push {
		t.Error("a repo released from hold must still get its push guard")
	}
	waitFor(t, "the held state to clear", func() bool {
		_, err := os.Stat(loopFailedPath(cfgs["b"].SyncDir))
		return os.IsNotExist(err)
	})
}

// The retry delay doubles from guardBackoffMin and stops growing at
// guardBackoffMax, so a check that keeps failing costs a few calls an hour,
// not one every few seconds.
func TestAwaitPrivate_BackoffDoublesAndCaps(t *testing.T) {
	oldMin, oldMax, oldCheck, oldWait := guardBackoffMin, guardBackoffMax, guardCheck, guardWait
	t.Cleanup(func() { guardBackoffMin, guardBackoffMax, guardCheck, guardWait = oldMin, oldMax, oldCheck, oldWait })
	guardBackoffMin, guardBackoffMax = time.Second, 5*time.Second
	var waits []time.Duration
	guardWait = func(ctx context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return true
	}
	n := 0
	guardCheck = func(config.Config, func(string, ...any)) error {
		if n++; n < 6 {
			return errors.New("installation lookup: GitHub returned HTTP 404")
		}
		return nil
	}
	cfg := config.Config{SyncDir: t.TempDir()}
	if !awaitPrivate(context.Background(), "x", cfg, errors.New("first")) {
		t.Fatal("awaitPrivate must report true once the check passes")
	}
	want := []time.Duration{1, 2, 4, 5, 5, 5}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %d of them", waits, len(want))
	}
	for i, w := range want {
		if waits[i] != w*time.Second {
			t.Errorf("wait %d = %v, want %v", i, waits[i], w*time.Second)
		}
	}
}

// A daemon stopped while a repo is held returns false and starts nothing.
func TestAwaitPrivate_StopWhileHeld(t *testing.T) {
	oldCheck := guardCheck
	t.Cleanup(func() { guardCheck = oldCheck })
	guardCheck = func(config.Config, func(string, ...any)) error { return errors.New("still failing") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if awaitPrivate(ctx, "x", config.Config{SyncDir: t.TempDir()}, errors.New("first")) {
		t.Fatal("a stopped daemon must not release a held repo")
	}
}

// The single-repo daemon holds too: Start succeeds, no loop runs while the
// check fails, and the loop starts (with its push guard) once it passes.
func TestStartSingle_GuardFailureHoldsThenStarts(t *testing.T) {
	fastGuardRetry(t)
	var calls atomic.Int32
	const failing = 2
	h := func(w http.ResponseWriter, r *http.Request) {
		pub := calls.Add(1) <= failing
		_ = json.NewEncoder(w).Encode(repoJSON{Name: "mem-personal", Private: tb(!pub), Visibility: map[bool]string{true: "public", false: "private"}[pub]})
	}
	cfg := guardCfg(t, h, true)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var started atomic.Int32
	var at atomic.Int32
	var push atomic.Bool
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error {
		if started.Add(1) == 1 {
			at.Store(calls.Load())
			push.Store(l.PushAllowed != nil)
		}
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { runLoop = old })
	r, err := startSingle(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a failing private-only check must hold, not refuse: %v", err)
	}
	defer r.Stop()
	waitFor(t, "the loop to start", func() bool { return started.Load() == 1 })
	if at.Load() <= failing {
		t.Errorf("loop started after %d checks; the first %d said public", at.Load(), failing)
	}
	if !push.Load() {
		t.Error("the loop must get its push guard")
	}
}
