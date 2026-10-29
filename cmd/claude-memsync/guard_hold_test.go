package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		if err != nil || !strings.Contains(string(b), heldMarker) {
			t.Errorf("repo %s: state file = %q, %v; want the private-only hold", n, b, err)
		}
	}
	if !strings.Contains(out, "waiting for the private-only check") {
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
	oldMin, oldMax, oldCheck, oldWait := guardBackoffMin, guardBackoffMax, guardRelease, guardWait
	t.Cleanup(func() { guardBackoffMin, guardBackoffMax, guardRelease, guardWait = oldMin, oldMax, oldCheck, oldWait })
	guardBackoffMin, guardBackoffMax = time.Second, 5*time.Second
	var waits []time.Duration
	guardWait = func(ctx context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return true
	}
	n := 0
	guardRelease = func(config.Config, func(string, ...any)) error {
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
	oldCheck := guardRelease
	t.Cleanup(func() { guardRelease = oldCheck })
	guardRelease = func(config.Config, func(string, ...any)) error { return errors.New("still failing") }
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

// The case seen on a Mac: the check App is registered but not installed on the
// org, so the installation lookup answers 404. The daemon must stay up, hold
// the repo with a status line naming the 404, back off, and start the loop
// once the App is installed, never before.
func TestProgramStart_AppNotInstalledHoldsUntilInstalled(t *testing.T) {
	fastGuardRetry(t)
	var installed atomic.Bool
	var lookups, repoHits atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			lookups.Add(1)
			if !installed.Load() {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"id":5}`)
		case r.URL.Path == "/app/installations/5/access_tokens":
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, appTestToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			repoHits.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+appTestToken {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"name":"mem-personal","private":true,"visibility":"private"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	cfg := guardCfg(t, nil, false)
	appEnv(t, gh.URL)
	cfg.GitHubAPIURL, cfg.GitHubApp, cfg.GitHubTokenFile = gh.URL, "check", ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var started atomic.Int32
	var pushOK atomic.Bool
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error {
		before := repoHits.Load()
		pushOK.Store(l.PushAllowed != nil && l.PushAllowed() && repoHits.Load() > before)
		started.Add(1)
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { runLoop = old })
	p := &program{configPath: cfg.Path()}
	if err := p.Start(nil); err != nil {
		t.Fatalf("an App that is not installed must hold the repo, not stop the daemon: %v", err)
	}
	defer p.Stop(nil)
	waitFor(t, "several 404 lookups", func() bool { return lookups.Load() >= 4 })
	if n := started.Load(); n != 0 {
		t.Fatalf("loop started %d times while the App lookup 404s", n)
	}
	b, err := os.ReadFile(loopFailedPath(cfg.SyncDir))
	if err != nil || !strings.Contains(string(b), "HTTP 404") || !strings.HasPrefix(loopFailedLine(b), "waiting for the private-only check") {
		t.Errorf("state file = %q, %v; want a held line naming the 404", b, err)
	}
	installed.Store(true)
	waitFor(t, "the loop to start after the install", func() bool { return started.Load() == 1 })
	if !pushOK.Load() {
		t.Error("the released loop's push guard must allow and re-check against the API (built after the passing check)")
	}
	waitFor(t, "the held state to clear", func() bool {
		_, err := os.Stat(loopFailedPath(cfg.SyncDir))
		return os.IsNotExist(err)
	})
}

// B1: a token source that is configured but gives no token (a failing token
// command, an empty token file) is an unverifiable check, not "no token
// configured": the repo must be held, never passed.
func TestDaemonGuard_ConfiguredSourceGivingNoTokenHolds(t *testing.T) {
	logf := func(string, ...any) {}
	failing := guardCfg(t, privateH(), false)
	failing.GitHubTokenCmd = []string{tokScript(t, "exit 1")}
	if err := daemonGuard(failing, logf); err == nil {
		t.Error("a failing token command must fail the check")
	}
	empty := guardCfg(t, privateH(), false)
	f := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(f, nil, 0600); err != nil {
		t.Fatal(err)
	}
	empty.GitHubTokenFile = f
	if err := daemonGuard(empty, logf); err == nil {
		t.Error("an empty token file must fail the check")
	}
}

// B1 end to end: a repo held as public must stay held when the token command
// later fails; a failed retry is never a pass.
func TestStartSingle_PublicThenTokenFailsStaysHeld(t *testing.T) {
	fastGuardRetry(t)
	var checks atomic.Int32
	h := func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		jsonH(200, repoJSON{Name: "mem-personal", Private: tb(false), Visibility: "public"})(w, r)
	}
	cfg := guardCfg(t, h, false)
	flag := filepath.Join(t.TempDir(), "token-works")
	if err := os.WriteFile(flag, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubTokenCmd = []string{tokScript(t, "[ -e "+flag+" ] || exit 1; echo "+cmdTok)}
	var started atomic.Int32
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error { started.Add(1); <-ctx.Done(); return nil }
	t.Cleanup(func() { runLoop = old })
	r, err := startSingle(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	waitFor(t, "the public answer", func() bool { return checks.Load() >= 1 })
	os.Remove(flag) // the token command now fails
	b0, _ := os.ReadFile(loopFailedPath(cfg.SyncDir))
	waitFor(t, "a few failed retries", func() bool {
		b, _ := os.ReadFile(loopFailedPath(cfg.SyncDir))
		return string(b) != string(b0) && strings.Contains(string(b), "token")
	})
	time.Sleep(20 * time.Millisecond)
	if n := started.Load(); n != 0 {
		t.Fatalf("a repo held as public was released %d times after its token command failed", n)
	}
}

// B1, push side: a push guard built while a configured source gives no token
// blocks pushes (no fall back to local checks only), and asks the API once the
// source works again.
func TestPushWatch_ConfiguredSourceGivingNoTokenBlocksUntilItWorks(t *testing.T) {
	var hits atomic.Int32
	h := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		privateH()(w, r)
	}
	cfg := guardCfg(t, h, false)
	flag := filepath.Join(t.TempDir(), "token-works")
	cfg.GitHubTokenCmd = []string{tokScript(t, "[ -e "+flag+" ] || exit 1; echo "+cmdTok)}
	allowed := pushWatch(cfg, func(string, ...any) {})
	if allowed == nil {
		t.Fatal("no push guard")
	}
	if allowed() {
		t.Fatal("pushes must be blocked while the configured token source gives no token")
	}
	if err := os.WriteFile(flag, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if !allowed() {
		t.Fatal("pushes must resume once the source works and the repo is private")
	}
	if hits.Load() == 0 {
		t.Error("the recovered push guard never asked the API")
	}
}

// S1: a held marker left by a stopped daemon is cleared when the next start's
// first check passes (single-repo mode has no supervisor to clear it).
func TestStartSingle_StaleHeldMarkerClearedWhenCheckPasses(t *testing.T) {
	cfg := guardCfg(t, privateH(), true)
	failed := loopFailedPath(cfg.SyncDir)
	os.MkdirAll(filepath.Dir(failed), 0700)
	if err := os.WriteFile(failed, []byte("2026-01-01T00:00:00Z "+heldMarker+"x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error { <-ctx.Done(); return nil }
	t.Cleanup(func() { runLoop = old })
	r, err := startSingle(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Errorf("stale held marker kept after a passing start check: %v", err)
	}
}

// S3: a check that passes after the daemon was told to stop releases nothing.
func TestAwaitPrivate_PassAfterStopReleasesNothing(t *testing.T) {
	oldCheck, oldMin := guardRelease, guardBackoffMin
	t.Cleanup(func() { guardRelease, guardBackoffMin = oldCheck, oldMin })
	guardBackoffMin = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	guardRelease = func(config.Config, func(string, ...any)) error {
		cancel() // Stop arrives while the check is in flight
		return nil
	}
	if awaitPrivate(ctx, "x", config.Config{SyncDir: t.TempDir()}, errors.New("first")) {
		t.Fatal("a pass that lands after Stop must not release the repo")
	}
}

// N3: only a held marker reads as held; a dying loop whose error happens to
// say "repo held" is still a stopped loop.
func TestLoopFailedLine_MarkerNotPhrase(t *testing.T) {
	if l := loopFailedLine([]byte("t " + heldMarker + "x")); !strings.HasPrefix(l, "waiting for the private-only check") {
		t.Errorf("held marker: %q", l)
	}
	if l := loopFailedLine([]byte("t watch failed: repo held by another tool")); !strings.HasPrefix(l, "loop stopped") {
		t.Errorf("phrase misread as held: %q", l)
	}
}

// Review round 2, B1: a held repo is released only by a check that asked the
// API. Here config.json turns untrusted during the hold, so the daemon drops
// every token source and the check would be "skipped"; that must not release
// a repo the API called public.
func TestStartSingle_HeldRepoNotReleasedBySkippedCheck(t *testing.T) {
	fastGuardRetry(t)
	var checks atomic.Int32
	h := func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		jsonH(200, repoJSON{Name: "mem-personal", Private: tb(false), Visibility: "public"})(w, r)
	}
	cfg := savedAndLoaded(t, guardCfg(t, h, true))
	var started atomic.Int32
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error { started.Add(1); <-ctx.Done(); return nil }
	t.Cleanup(func() { runLoop = old })
	r, err := startSingle(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	waitFor(t, "the public answer", func() bool { return checks.Load() >= 1 })
	if err := os.Chmod(cfg.Path(), 0o644); err != nil { // no longer trusted
		t.Fatal(err)
	}
	b0, _ := os.ReadFile(loopFailedPath(cfg.SyncDir))
	waitFor(t, "a retry after the config turned untrusted", func() bool {
		b, _ := os.ReadFile(loopFailedPath(cfg.SyncDir))
		return len(b) > 0 && string(b) != string(b0)
	})
	time.Sleep(20 * time.Millisecond)
	if n := started.Load(); n != 0 {
		t.Fatalf("a repo held as public was released %d times by a skipped check", n)
	}
}

// Review round 2, S2: the push guard of a released repo is built after the
// passing check, so it uses the token that passed. Here the token file is
// rotated during the hold (the old token gets 401); no request may carry the
// old token after release.
func TestStartSingle_ReleasedPushGuardUsesTheTokenThatPassed(t *testing.T) {
	fastGuardRetry(t)
	var released atomic.Bool
	var staleAfter, newHits atomic.Int32
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new-token" {
			if released.Load() {
				staleAfter.Add(1)
			}
			w.WriteHeader(401)
			return
		}
		newHits.Add(1)
		privateH()(w, r)
	}
	cfg := guardCfg(t, h, false)
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("old-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubTokenFile = tok
	var pushed atomic.Int32
	var done atomic.Bool
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error {
		released.Store(true)
		if l.PushAllowed != nil && l.PushAllowed() {
			pushed.Add(1)
		}
		done.Store(true)
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { runLoop = old })
	r, err := startSingle(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if err := os.WriteFile(tok, []byte("new-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the loop to start", done.Load)
	if staleAfter.Load() != 0 || pushed.Load() != 1 {
		t.Fatalf("pushed=%d, %d requests with the old token after release", pushed.Load(), staleAfter.Load())
	}
}

// Review round 2: an unset named env variable is a configured source that
// gives no token.
func TestDaemonGuard_UnsetNamedEnvHolds(t *testing.T) {
	cfg := guardCfg(t, privateH(), false)
	cfg.GitHubTokenEnv = "MEMSYNC_TEST_UNSET_TOKEN_VAR"
	t.Setenv(cfg.GitHubTokenEnv, "")
	if err := daemonGuard(cfg, func(string, ...any) {}); err == nil {
		t.Error("an unset githubTokenEnv variable must fail the check")
	}
}

// Review round 2: the routed start clears a stale held marker too.
func TestRouted_StaleHeldMarkerClearedWhenCheckPasses(t *testing.T) {
	cfgs := privateRouted(t, map[string]string{"a": "git@github.com:Org/mem-project-a.git"}, routedAPI(""))
	failed := loopFailedPath(cfgs["a"].SyncDir)
	os.MkdirAll(filepath.Dir(failed), 0700)
	if err := os.WriteFile(failed, []byte("2026-01-01T00:00:00Z "+heldMarker+"x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	stubLoops(t, nil)
	r, err := set.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Errorf("stale held marker kept after a passing routed start: %v", err)
	}
}

// Review round 2: when a configured source starts giving a token again, the
// push guard hands over only after the API accepts it; a 401 keeps pushes
// blocked.
func TestPushWatch_RecoveredSourceNeedsAnAPIPassFirst(t *testing.T) {
	var ok atomic.Bool
	h := func(w http.ResponseWriter, r *http.Request) {
		if !ok.Load() {
			w.WriteHeader(401)
			return
		}
		privateH()(w, r)
	}
	cfg := guardCfg(t, h, false)
	flag := filepath.Join(t.TempDir(), "token-works")
	cfg.GitHubTokenCmd = []string{tokScript(t, "[ -e "+flag+" ] || exit 1; echo "+cmdTok)}
	allowed := pushWatch(cfg, func(string, ...any) {})
	if allowed() {
		t.Fatal("blocked while the source gives nothing")
	}
	os.WriteFile(flag, nil, 0600)
	if allowed() {
		t.Fatal("a token the API rejects must keep pushes blocked")
	}
	ok.Store(true)
	if !allowed() {
		t.Fatal("pushes resume once the API accepts the token and the repo is private")
	}
}

// Review round 2: only a record whose reason starts with the marker is held.
func TestLoopFailedLine_MarkerOnlyAfterTimestamp(t *testing.T) {
	dead := []byte("2026-01-01T00:00:00Z sync failed: remote said " + heldMarker + "x\n")
	if l := loopFailedLine(dead); !strings.HasPrefix(l, "loop stopped") {
		t.Errorf("marker inside a dead loop's error misread as held: %q", l)
	}
}

// Routed twin of TestStartSingle_ReleasedPushGuardUsesTheTokenThatPassed.
func TestRouted_ReleasedPushGuardUsesTheTokenThatPassed(t *testing.T) {
	fastGuardRetry(t)
	var released atomic.Bool
	var staleAfter atomic.Int32
	api := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new-token" {
			if released.Load() {
				staleAfter.Add(1)
			}
			w.WriteHeader(401)
			return
		}
		routedAPI("")(w, r)
	}
	cfgs := privateRouted(t, map[string]string{"a": "git@github.com:Org/mem-project-a.git"}, api)
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("old-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")
	cfg := cfgs["a"]
	cfg.GitHubTokenFile = tok
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	var pushed atomic.Int32
	var done atomic.Bool
	old := runLoop
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error {
		released.Store(true)
		if l.PushAllowed != nil && l.PushAllowed() {
			pushed.Add(1)
		}
		done.Store(true)
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { runLoop = old })
	r, err := set.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if err := os.WriteFile(tok, []byte("new-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the loop to start", done.Load)
	if staleAfter.Load() != 0 || pushed.Load() != 1 {
		t.Fatalf("pushed=%d, %d requests with the old token after release", pushed.Load(), staleAfter.Load())
	}
}
