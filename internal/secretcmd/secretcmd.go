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
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// DefaultTimeout is how long a secret command may run.
const DefaultTimeout = 10 * time.Second

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
