package ghapp

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"

	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
)

// keyCmdTimeout is how long a key command may run; tests shorten it.
var keyCmdTimeout = secretcmd.DefaultTimeout

const maxKeyFile = 64 << 10

// LoadKeyFile reads a PEM key from path. The file must be readable by its
// owner only (no group or other bits, so 0600 or 0400); otherwise it is
// refused without reading it. Mode bits mean nothing on Windows, where the
// check is skipped.
func LoadKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open the app key file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("the app key file is not a regular file")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("the app key file must be mode 0600 (no group or other access); refusing to read it")
	}
	if err := ownerAndLinksOK(st, "the app key file"); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile))
	if err != nil {
		return nil, errors.New("cannot read the app key file")
	}
	return b, nil
}

// LoadKeyCmd runs a command (an argv: no shell, 10 s timeout) that prints the
// PEM key on stdout.
func LoadKeyCmd(ctx context.Context, argv []string) ([]byte, error) {
	b, err := secretcmd.Run(ctx, argv, keyCmdTimeout)
	if err != nil {
		return nil, errors.New("app key command: " + err.Error())
	}
	return b, nil
}

// ReadKeyFileAnyMode reads a PEM key without the mode check. It is for
// fingerprinting a key GitHub just downloaded (often mode 0644), which sends
// nothing anywhere; minting always goes through LoadKeyFile.
func ReadKeyFileAnyMode(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open the key file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile))
	if err != nil {
		return nil, errors.New("cannot read the key file")
	}
	return b, nil
}
