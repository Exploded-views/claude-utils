package main

import (
	"os"
	"testing"
)

// On macOS the service logger is syslog at LOG_INFO, which the system does not
// keep, so the std logger stays on stderr (launchd sends that to a file).
func TestServiceLogOutputDarwinKeepsStderr(t *testing.T) {
	if w := serviceLogOutput("darwin", nil); w != os.Stderr {
		t.Fatalf("darwin output = %v, want os.Stderr", w)
	}
}

func TestServiceLogOutputOthersUseServiceWriter(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		if _, ok := serviceLogOutput(goos, nil).(serviceLogWriter); !ok {
			t.Errorf("%s output is not the service writer", goos)
		}
	}
}
