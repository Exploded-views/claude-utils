package secretcmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func resetMode(t *testing.T) {
	t.Helper()
	old := Notice
	t.Cleanup(func() { SetDaemon(false); Notice = old })
	SetDaemon(false)
}

// A password manager may wait for the user to approve a prompt, so an
// interactive key command gets far longer than the daemon's 10 s.
func TestKeyCmdTimeoutInteractiveDefaultIs120s(t *testing.T) {
	resetMode(t)
	t.Setenv(TimeoutEnv, "")
	if got := KeyCmdTimeout(); got != 120*time.Second {
		t.Fatalf("KeyCmdTimeout() = %v, want 2m0s", got)
	}
}

func TestKeyCmdTimeoutEnvOverride(t *testing.T) {
	resetMode(t)
	t.Setenv(TimeoutEnv, "45s")
	if got := KeyCmdTimeout(); got != 45*time.Second {
		t.Fatalf("KeyCmdTimeout() = %v, want 45s", got)
	}
	for _, bad := range []string{"abc", "-5s", "0s", "10"} {
		t.Setenv(TimeoutEnv, bad)
		if got := KeyCmdTimeout(); got != 120*time.Second {
			t.Errorf("%s=%q: KeyCmdTimeout() = %v, want the 2m0s default", TimeoutEnv, bad, got)
		}
	}
}

// The daemon runs the command unattended in its loop: nobody can approve a
// prompt, so a stuck command must not hold the loop for two minutes.
func TestKeyCmdTimeoutDaemonStaysShort(t *testing.T) {
	resetMode(t)
	t.Setenv(TimeoutEnv, "")
	SetDaemon(true)
	if got := KeyCmdTimeout(); got != 10*time.Second {
		t.Fatalf("daemon KeyCmdTimeout() = %v, want 10s", got)
	}
}

func TestAnnouncePrintsOneLineInteractively(t *testing.T) {
	resetMode(t)
	var b bytes.Buffer
	Notice = &b
	Announce()
	want := "waiting for your password manager (approve the prompt if one appears)…\n"
	if b.String() != want {
		t.Fatalf("Announce printed %q, want %q", b.String(), want)
	}
}

func TestAnnounceIsSilentInTheDaemon(t *testing.T) {
	resetMode(t)
	var b bytes.Buffer
	Notice = &b
	SetDaemon(true)
	Announce()
	if strings.TrimSpace(b.String()) != "" {
		t.Fatalf("the daemon printed %q", b.String())
	}
}
