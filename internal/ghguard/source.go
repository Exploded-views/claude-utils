package ghguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// TokenSource yields a GitHub API token. Implementations may mint or fetch a
// fresh token on every call; callers must not keep the result for long.
// Errors never contain token text.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// EnvSource reads a token from an environment variable (DefaultTokenEnv when
// Name is empty). An empty result with a nil error means "not configured".
type EnvSource struct{ Name string }

func (s EnvSource) Token(context.Context) (string, error) {
	name := s.Name
	if name == "" {
		name = DefaultTokenEnv
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

// FileSource reads a token from a file.
type FileSource struct{ Path string }

func (s FileSource) Token(context.Context) (string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", errors.Unwrap(err))
	}
	return strings.TrimSpace(string(b)), nil
}

// CmdSource runs a command (an argv: no shell, 10 s timeout) that prints the
// token on stdout.
type CmdSource struct{ Argv []string }

func (s CmdSource) Token(context.Context) (string, error) {
	return RunTokenCmd(s.Argv, TokenCmdTimeout)
}

// SelectSource returns the first configured source, in the order app, file,
// cmd, env. It does not fall through when the chosen one fails: a configured
// source that errors is an error, never a silent downgrade. env is always
// configured (envName may be empty for the default).
func SelectSource(app TokenSource, file string, cmd []string, envName string) TokenSource {
	switch {
	case app != nil:
		return app
	case file != "":
		return FileSource{Path: file}
	case len(cmd) > 0:
		return CmdSource{Argv: cmd}
	}
	return EnvSource{Name: envName}
}
