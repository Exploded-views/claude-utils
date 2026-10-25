package secretcmd

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipWin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs POSIX utilities")
	}
}

func bg() context.Context { return context.Background() }

func TestSplitArgs(t *testing.T) {
	ok := map[string][]string{
		`secret-tool read vault://Vault/Item/field`:               {"secret-tool", "read", "vault://Vault/Item/field"},
		`secret-tool read "vault://My Vault/Item Name/field"`:     {"secret-tool", "read", "vault://My Vault/Item Name/field"},
		`secret-tool read 'vault://My Vault/Item Name/field' --x`: {"secret-tool", "read", "vault://My Vault/Item Name/field", "--x"},
		`  a   b  `:                           {"a", "b"},
		`a "" b`:                              {"a", "", "b"},
		`say "he said \"hi\""`:                {"say", `he said "hi"`},
		`tool 'a $HOME ` + "`x`" + ` $(y) *'`: {"tool", "a $HOME `x` $(y) *"}, // no expansion
		`x "a"'b'c`:                           {"x", "abc"},
		`C:\tools\op.exe read x`:              {`C:\tools\op.exe`, "read", "x"},
		`a;b && c | d`:                        {"a;b", "&&", "c", "|", "d"},
		`"" `:                                 {""},
		``:                                    nil,
	}
	for in, want := range ok {
		got, err := SplitArgs(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %#v %v, want %#v", in, got, err, want)
		}
	}
	for _, bad := range []string{`a "b`, `a 'b`, `a "b\"`} {
		if _, err := SplitArgs(bad); err == nil {
			t.Errorf("%q: unterminated quote accepted", bad)
		}
	}
}

func TestRunPassesArgsWithSpacesIntact(t *testing.T) {
	skipWin(t)
	argv, _ := SplitArgs(`printf %s "vault://My Vault/Item Name/field"`)
	out, err := Run(bg(), argv, time.Second)
	if err != nil || string(out) != "vault://My Vault/Item Name/field" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestRunCapturesStdoutNoShell(t *testing.T) {
	skipWin(t)
	out, err := Run(bg(), []string{"printf", "hello"}, time.Second)
	if err != nil || string(out) != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	out, err = Run(bg(), []string{"echo", "a;echo b", "$HOME"}, time.Second)
	if err != nil || strings.TrimSpace(string(out)) != "a;echo b $HOME" {
		t.Fatalf("shell interpreted the args: out=%q err=%v", out, err)
	}
}

func TestRunRejectsEmptyAndFailure(t *testing.T) {
	skipWin(t)
	for _, argv := range [][]string{nil, {}, {""}} {
		if _, err := Run(bg(), argv, time.Second); err == nil {
			t.Errorf("empty argv %q accepted", argv)
		}
	}
	if _, err := Run(bg(), []string{"false"}, time.Second); err == nil {
		t.Error("failing command accepted")
	}
}

func TestRunTimeout(t *testing.T) {
	skipWin(t)
	start := time.Now()
	_, err := Run(bg(), []string{"sleep", "30"}, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not enforced")
	}
}

func TestRunErrorsNeverCarryOutputOrArgs(t *testing.T) {
	skipWin(t)
	_, err := Run(bg(), []string{"sh", "-c", "echo SECRET-VALUE; exit 1"}, time.Second)
	if err == nil || strings.Contains(err.Error(), "SECRET-VALUE") {
		t.Fatalf("err = %v", err)
	}
	_, err = Run(bg(), []string{"/nonexistent/bin", "SECRET-ARG"}, time.Second)
	if err == nil || strings.Contains(err.Error(), "SECRET-ARG") || strings.Contains(err.Error(), "/nonexistent") {
		t.Errorf("error leaks argv: %v", err)
	}
}

func TestRunMaxOutput(t *testing.T) {
	skipWin(t)
	if out, err := Run(bg(), []string{"head", "-c", "200000", "/dev/zero"}, 5*time.Second); err == nil {
		t.Fatalf("oversized output accepted (%d bytes)", len(out))
	}
}

func TestDefaultTimeoutIs10s(t *testing.T) {
	if DefaultTimeout != 10*time.Second {
		t.Errorf("DefaultTimeout = %v", DefaultTimeout)
	}
}

func TestFeedPipesStdin(t *testing.T) {
	skipWin(t)
	out := t.TempDir() + "/got"
	if err := Feed(bg(), []string{"tee", out}, []byte("PEM-DATA"), time.Second); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != "PEM-DATA" {
		t.Errorf("stdin = %q", b)
	}
}

func TestFeedFailureAndTimeoutHideInput(t *testing.T) {
	skipWin(t)
	err := Feed(bg(), []string{"false"}, []byte("TOPSECRET"), time.Second)
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("err = %v", err)
	}
	err = Feed(bg(), []string{"sleep", "30"}, []byte("x"), 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
}

// The same table drives scripts/bootstrap_test.sh against the bootstrap's
// shell implementation of these rules.
func TestSplitArgsSharedTable(t *testing.T) {
	b, err := os.ReadFile("testdata/splitargs.tsv")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		in, want, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("bad row %q", line)
		}
		got, err := SplitArgs(in)
		if want == "ERR" {
			if err == nil {
				t.Errorf("%q: want an error, got %q", in, got)
			}
			n++
			continue
		}
		var exp []string
		if jerr := json.Unmarshal([]byte(want), &exp); jerr != nil {
			t.Fatalf("%q: %v", want, jerr)
		}
		if err != nil || !reflect.DeepEqual(append([]string{}, got...), append([]string{}, exp...)) {
			t.Errorf("%q = %q, %v; want %q", in, got, err, exp)
		}
		n++
	}
	if n < 10 {
		t.Fatalf("table too short: %d rows", n)
	}
}
