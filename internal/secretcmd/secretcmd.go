// Package secretcmd runs a user-configured command that prints a secret (a
// token or a key) on stdout, for example a password-manager CLI.
//
// A command is an argv: it is executed directly, with no shell, so quoting and
// metacharacters in its arguments have no special meaning. Errors never carry
// the command's arguments or any of its output.
package secretcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// DefaultTimeout is how long a secret command may run when nobody can answer a
// prompt (the daemon's loop) and the fallback of Run and Feed.
const DefaultTimeout = 10 * time.Second

// InteractiveTimeout is how long a key or token command may run when a person
// is at the terminal: a password manager may wait for them to approve a prompt.
const InteractiveTimeout = 120 * time.Second

// TimeoutEnv overrides the key and token command timeout (a Go duration, such
// as "45s" or "3m"). An unusable or non-positive value is ignored.
const TimeoutEnv = "CLAUDE_MEMSYNC_SECRET_TIMEOUT"

// WaitMessage is printed once before an interactive key command runs.
const WaitMessage = "waiting for your password manager (approve the prompt if one appears)…"

// Notice is where WaitMessage goes.
var Notice io.Writer = os.Stderr

var daemon atomic.Bool

// SetDaemon marks this process as the sync daemon, whose commands run
// unattended: they keep the short timeout and print nothing.
func SetDaemon(on bool) { daemon.Store(on) }

// KeyCmdTimeout is the timeout for a key or token command: TimeoutEnv when set,
// else DefaultTimeout in the daemon and InteractiveTimeout everywhere else.
func KeyCmdTimeout() time.Duration {
	if v := os.Getenv(TimeoutEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if daemon.Load() {
		return DefaultTimeout
	}
	return InteractiveTimeout
}

// Announce tells the person a password manager may be waiting for them. It is
// silent in the daemon.
func Announce() {
	if !daemon.Load() {
		fmt.Fprintln(Notice, WaitMessage)
	}
}

// MaxOutput bounds what is read from the command.
const MaxOutput = 64 << 10

var errTooLarge = errors.New("output too large")

// SplitArgs splits a command line into an argv, shell style but without a
// shell: whitespace separates arguments, "double quotes" group (with \" and \\
// as the only escapes) and 'single quotes' group literally. Nothing is
// expanded: $VAR, `cmd`, globs, ; & | and ~ are ordinary characters. A
// backslash outside double quotes is literal, so Windows paths work.
func SplitArgs(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	have := false
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		case c == '\'':
			have = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				cur.WriteRune(rs[j])
				j++
			}
			if j == len(rs) {
				return nil, errors.New("unterminated ' quote in command")
			}
			i = j
		case c == '"':
			have = true
			j := i + 1
			closed := false
			for j < len(rs) {
				if rs[j] == '\\' && j+1 < len(rs) && (rs[j+1] == '"' || rs[j+1] == '\\') {
					cur.WriteRune(rs[j+1])
					j += 2
					continue
				}
				if rs[j] == '"' {
					closed = true
					break
				}
				cur.WriteRune(rs[j])
				j++
			}
			if !closed {
				return nil, errors.New("unterminated \" quote in command")
			}
			i = j
		default:
			have = true
			cur.WriteRune(c)
		}
	}
	if have {
		args = append(args, cur.String())
	}
	return args, nil
}

type capWriter struct{ buf bytes.Buffer }

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > MaxOutput {
		return 0, errTooLarge
	}
	return w.buf.Write(p)
}

func check(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("secret command is empty")
	}
	return nil
}

func classify(ctx context.Context, err error) error {
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return errors.New("secret command timed out")
	case errors.Is(err, errTooLarge):
		return errors.New("secret command output too large")
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return errors.New("secret command failed (exit status " + strconv.Itoa(ee.ExitCode()) + ")")
	}
	return errors.New("secret command could not be run")
}

// Run executes argv and returns its stdout. stderr is discarded.
func Run(ctx context.Context, argv []string, timeout time.Duration) ([]byte, error) {
	if err := check(argv); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.HideWindow(cmd)
	out := &capWriter{}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, classify(ctx, err)
	}
	return out.buf.Bytes(), nil
}

// Feed runs argv with input on its stdin, to store a secret via a
// password-manager CLI. stdout and stderr are discarded.
func Feed(ctx context.Context, argv []string, input []byte, timeout time.Duration) error {
	if err := check(argv); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.HideWindow(cmd)
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return classify(ctx, err)
	}
	return nil
}
