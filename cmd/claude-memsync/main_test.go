package main

import (
	"os"
	"testing"
)

// No test may reach a real service: commands that meet a held sync lock ask
// the service manager for its state, and a live daemon on the machine running
// the tests would otherwise be stopped. Tests that need a service install their
// own fakes (installFakeSvc).
func TestMain(m *testing.M) {
	statusFn = func() (string, error) { return "stopped", nil }
	startFn = func() error { panic("a test started a real service") }
	stopFn = func() error { panic("a test stopped a real service") }
	os.Exit(m.Run())
}
