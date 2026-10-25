package ghapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// App is one entry of apps.json: where an app's key lives on this host. The
// key itself is never stored in apps.json.
type App struct {
	Name    string `json:"name"`
	AppID   string `json:"appId"`
	Org     string `json:"org"`
	KeyFile string `json:"keyFile,omitempty"`
	KeyCmd  Argv   `json:"keyCmd,omitempty"`

	// APIBase is the only API the app's JWT is ever sent to (default
	// https://api.github.com; set it for GitHub Enterprise). It lives here,
	// not in config.json, because a JWT proves control of the app.
	APIBase string `json:"apiBase,omitempty"`
}

// Argv is a command line as a JSON array (["secret-tool","read","vault://My Vault/x"]).
// A plain string is also accepted and split shell-style (quotes group words;
// nothing is expanded, no shell runs).
type Argv []string

func (a *Argv) UnmarshalJSON(b []byte) error {
	var arr []string
	if json.Unmarshal(b, &arr) == nil {
		*a = arr
		return nil
	}
	var line string
	if err := json.Unmarshal(b, &line); err != nil {
		return errors.New("keyCmd must be a JSON array of strings, or a string")
	}
	argv, err := secretcmd.SplitArgs(line)
	if err != nil {
		return errors.New("keyCmd: " + err.Error())
	}
	*a = argv
	return nil
}

// API returns the API base this app mints against.
func (a App) API() string {
	if a.APIBase == "" {
		return DefaultAPIBase
	}
	return strings.TrimRight(a.APIBase, "/")
}

// validAPIBase accepts https URLs, and http only for a loopback host (tests).
func validAPIBase(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	}
	return false
}

var (
	numRE  = regexp.MustCompile(`^[0-9]{1,20}$`)
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Validate checks the entry is complete and unambiguous.
func (a App) Validate() error {
	switch {
	case !nameRE.MatchString(a.Name):
		return errors.New("app name must be 1-64 of letters, digits, dot, dash, underscore")
	case !numRE.MatchString(a.AppID):
		return errors.New("appId must be numeric")
	case !ValidOrg(a.Org):
		return errors.New("org is not a valid GitHub login")
	case (a.KeyFile == "") == (len(a.KeyCmd) == 0):
		return errors.New("set exactly one of keyFile and keyCmd")
	case a.APIBase != "" && !validAPIBase(a.APIBase):
		return errors.New("apiBase must be an https URL")
	}
	return nil
}

// DefaultAppsPath is $XDG_CONFIG_HOME/claude-memsync/apps.json, or
// ~/.config/claude-memsync/apps.json.
func DefaultAppsPath() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "claude-memsync", "apps.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "claude-memsync", "apps.json"), nil
}

func readApps(path string) ([]App, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("apps.json must be mode 0600 (it names key commands that get run)")
	}
	var apps []App
	if err := json.NewDecoder(f).Decode(&apps); err != nil {
		return nil, errors.New("apps.json is not a JSON array of app entries")
	}
	return apps, nil
}

// LookupApp finds the named entry and validates it.
func LookupApp(path, name string) (App, error) {
	apps, err := readApps(path)
	if err != nil {
		if os.IsNotExist(err) {
			return App{}, fmt.Errorf("no apps file at %s", path)
		}
		return App{}, err
	}
	for _, a := range apps {
		if a.Name == name {
			if err := a.Validate(); err != nil {
				return App{}, fmt.Errorf("app %q: %w", name, err)
			}
			return a, nil
		}
	}
	return App{}, fmt.Errorf("no app named %q in %s", name, path)
}

// SaveApp adds or replaces an entry, writing the file 0600 atomically.
func SaveApp(path string, a App) error {
	if err := a.Validate(); err != nil {
		return err
	}
	apps, err := readApps(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	replaced := false
	for i := range apps {
		if apps[i].Name == a.Name {
			apps[i], replaced = a, true
		}
	}
	if !replaced {
		apps = append(apps, a)
	}
	b, _ := json.MarshalIndent(apps, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".apps-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Minter builds a caching token source for the app. It mints only against the
// entry's own API base.
func (a App) Minter() (*Minter, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	load := func(ctx context.Context) ([]byte, error) { return LoadKeyFile(a.KeyFile) }
	if len(a.KeyCmd) > 0 {
		load = func(ctx context.Context) ([]byte, error) { return LoadKeyCmd(ctx, a.KeyCmd) }
	}
	return &Minter{AppID: a.AppID, Org: a.Org, APIBase: a.API(), LoadKey: load}, nil
}
