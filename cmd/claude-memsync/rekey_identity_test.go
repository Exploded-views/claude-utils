package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
)

// After rekey-pq a host holds identity.txt (classic, retired or not) and
// identity-pq.txt. A later init of another pq repo must reuse the hybrid
// identity, not create a second one.
func TestInit_DefaultIdentityPrefersTheHybridFileUnderPQ(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	t.Setenv("APPDATA", cfgDir)
	base := defaultIdentityPath()
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := defaultIdentityFor(agecrypt.SchemePQ); got != base {
		t.Errorf("no hybrid file yet: %q, want %q", got, base)
	}
	if _, err := agecrypt.GenerateIdentity(base); err != nil {
		t.Fatal(err)
	}
	if got := defaultIdentityFor(agecrypt.SchemePQ); got != base {
		t.Errorf("only a classic file: %q, want %q", got, base)
	}
	pqp := newIdentityPath(base)
	if _, err := agecrypt.GenerateHybridIdentity(pqp); err != nil {
		t.Fatal(err)
	}
	if got := defaultIdentityFor(agecrypt.SchemePQ); got != pqp {
		t.Errorf("pq scheme with identity-pq.txt present: %q, want %q", got, pqp)
	}
	if got := defaultIdentityFor(agecrypt.SchemeClassic); got != base {
		t.Errorf("classic scheme must keep %q, got %q", base, got)
	}
}
