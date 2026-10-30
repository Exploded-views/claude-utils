//go:build budget

package main

import (
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// growable makes every directory count 1, except the ones in big, which count 300.
func growable(t *testing.T, big map[string]int) {
	t.Helper()
	restore := testenv.SetCountEntriesForTest(func(dir string) (int, error) {
		if n, ok := big[dir]; ok {
			return n, nil
		}
		return 1, nil
	})
	t.Cleanup(restore)
}

func needQuiet(t *testing.T) {
	t.Helper()
	old := loadAvgFn
	loadAvgFn = func() float64 { return 0.5 }
	t.Cleanup(func() { loadAvgFn = old })
}

// An ancestor that crosses the limit during a gate makes it INVALID (skipped),
// even when the gate has already recorded a failure.
func TestTimingBudgetEndCheckBeatsAFailure(t *testing.T) {
	needQuiet(t)
	big := map[string]int{}
	growable(t, big)
	var sub *testing.T
	t.Run("gate", func(s *testing.T) {
		sub = s
		fail, done := timingBudget(s)
		defer done()
		fail("missed budget")
		big["/"] = 300
	})
	if !sub.Skipped() || sub.Failed() {
		t.Errorf("skipped=%v failed=%v; want skipped (INVALID) and not failed", sub.Skipped(), sub.Failed())
	}
}

// B1 points TMPDIR at its own big fixture mid-run; that must not make it INVALID.
func TestTimingBudgetIgnoresTheFixtureTMPDIR(t *testing.T) {
	needQuiet(t)
	growable(t, map[string]int{"/fakefixture/T": 10000})
	var sub *testing.T
	t.Run("gate", func(s *testing.T) {
		sub = s
		_, done := timingBudget(s)
		defer done()
		s.Setenv("TMPDIR", "/fakefixture/T")
	})
	if sub.Skipped() || sub.Failed() {
		t.Errorf("skipped=%v failed=%v; want a pass", sub.Skipped(), sub.Failed())
	}
}

func TestLogLoadEndCheckSkipsAPassingGateWhoseAncestorGrew(t *testing.T) {
	needQuiet(t)
	big := map[string]int{}
	growable(t, big)
	var sub *testing.T
	t.Run("gate", func(s *testing.T) {
		sub = s
		logLoad(s)
		big["/"] = 300
	})
	if !sub.Skipped() || sub.Failed() {
		t.Errorf("skipped=%v failed=%v; want skipped (INVALID)", sub.Skipped(), sub.Failed())
	}
}

func TestLogLoadIgnoresTheFixtureTMPDIR(t *testing.T) {
	needQuiet(t)
	growable(t, map[string]int{"/fakefixture/T": 10000})
	var sub *testing.T
	t.Run("gate", func(s *testing.T) {
		sub = s
		logLoad(s)
		s.Setenv("TMPDIR", "/fakefixture/T")
	})
	if sub.Skipped() || sub.Failed() {
		t.Errorf("skipped=%v failed=%v; want a pass", sub.Skipped(), sub.Failed())
	}
}
