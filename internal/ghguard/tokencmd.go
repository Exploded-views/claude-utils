package ghguard

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
)

// TokenCmdTimeout is the daemon's bound on one run of a token command; a
// person at the terminal gets secretcmd.KeyCmdTimeout.
const TokenCmdTimeout = secretcmd.DefaultTimeout

// RunTokenCmd runs argv directly (no shell) and returns its trimmed stdout as
// a token. The token is only returned, never stored. Errors are fixed strings:
// the command's stdout and stderr are never put into them.
func RunTokenCmd(argv []string, timeout time.Duration) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("token command is empty")
	}
	out, err := secretcmd.Run(context.Background(), argv, timeout)
	if err != nil {
		return "", errors.New(strings.Replace(err.Error(), "secret command", "token command", 1))
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", errors.New("token command printed no token")
	}
	return tok, nil
}
