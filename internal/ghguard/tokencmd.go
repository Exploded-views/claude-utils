package ghguard

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// TokenCmdTimeout bounds one run of a token command.
const TokenCmdTimeout = 10 * time.Second

// RunTokenCmd runs argv directly (no shell) and returns its trimmed stdout as
// a token. The token is only returned, never stored. Errors are fixed strings:
// the command's stdout and stderr are never put into them.
func RunTokenCmd(argv []string, timeout time.Duration) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("token command is empty")
	}
	if timeout <= 0 {
		timeout = TokenCmdTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil // discarded
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", errors.New("token command timed out")
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", errors.New("token command failed")
		}
		return "", errors.New("token command could not be started")
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" {
		return "", errors.New("token command printed no token")
	}
	return tok, nil
}
