package ghapp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadKeyFilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	k := genKey(t)
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, pkcs1PEM(k), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := LoadKeyFile(p); err != nil || len(b) == 0 {
		t.Fatalf("0600 refused: %v", err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeyFile(p)
		if err == nil {
			t.Errorf("mode %o accepted", mode)
		} else if strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("error leaks key: %v", err)
		}
	}
	if _, err := LoadKeyFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestLoadKeyCmd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs printf/sleep")
	}
	out, err := LoadKeyCmd(context.Background(), []string{"printf", "hello"})
	if err != nil || string(out) != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	out, err = LoadKeyCmd(context.Background(), []string{"echo", "a;echo b"})
	if err != nil || strings.TrimSpace(string(out)) != "a;echo b" {
		t.Fatalf("shell interpreted: out=%q err=%v", out, err)
	}
}

func TestLoadKeyCmdTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sleep")
	}
	old := keyCmdTimeout
	keyCmdTimeout = 200 * time.Millisecond
	defer func() { keyCmdTimeout = old }()
	start := time.Now()
	_, err := LoadKeyCmd(context.Background(), []string{"sleep", "30"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not enforced")
	}
}

func TestKeyCmdDefaultTimeoutIs10s(t *testing.T) {
	if keyCmdTimeout != 10*time.Second {
		t.Errorf("keyCmdTimeout = %v", keyCmdTimeout)
	}
}

func TestKeyCmdErrorDoesNotLeakOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	_, err := LoadKeyCmd(context.Background(), []string{"sh", "-c", "echo SECRET-KEY-MATERIAL; exit 1"})
	if err == nil || strings.Contains(err.Error(), "SECRET-KEY-MATERIAL") {
		t.Fatalf("err = %v", err)
	}
}

// A second hard link to the key file could be a tracked file's name: refused
// unread, like a loose mode.
func TestLoadKeyFileRefusesHardLinkedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no link counts")
	}
	k := genKey(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(p, pkcs1PEM(k), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(p, filepath.Join(dir, "other.pem")); err != nil {
		t.Skip("no hard links:", err)
	}
	_, err := LoadKeyFile(p)
	if err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("hard-linked key file accepted: %v", err)
	}
}
