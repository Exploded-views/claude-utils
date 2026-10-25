package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/ghapp"
	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
)

// appAdd records an app that already exists (created by hand, or by a
// register run that lost its key) in apps.json.
func appAdd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("app add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "apps.json entry name (required)")
	appID := fs.String("app-id", "", "numeric GitHub App ID (required)")
	org := fs.String("org", "", "org the app is installed on (required)")
	keyCmd := fs.String("key-cmd", "", "command that prints the private key (shell-style quotes, no shell)")
	keyFile := fs.String("key-file", "", "private key file (mode 0600, yours, one hard link)")
	apiURL := fs.String("api-url", "", "GitHub API base URL (default: public GitHub); recorded as apiBase")
	appsFile := fs.String("apps-file", "", "apps.json path (default: ~/.config/claude-memsync/apps.json)")
	replace := fs.Bool("replace", false, "overwrite an existing entry with this name")
	verify := fs.Bool("verify", false, "mint a token against the org to prove the entry works (prints MINT-OK)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bad := func(msg string) int { fmt.Fprintln(stderr, "app add:", msg); return 2 }
	if f := placeholderFlag(fs); f != "" {
		return bad("--" + f + " still contains a <placeholder>; fill it in first")
	}
	if *name == "" || *appID == "" || *org == "" {
		return bad("--name, --app-id and --org are required")
	}
	if (*keyCmd == "") == (*keyFile == "") {
		return bad("give exactly one of --key-cmd and --key-file")
	}
	argv, err := secretcmd.SplitArgs(*keyCmd)
	if err != nil {
		return bad("--key-cmd: unbalanced quote")
	}
	path, err := appsPath(*appsFile)
	if err != nil {
		return bad(err.Error())
	}
	entry := ghapp.App{Name: *name, AppID: *appID, Org: *org, KeyCmd: argv, APIBase: strings.TrimRight(*apiURL, "/")}
	if *keyFile != "" {
		abs, err := filepath.Abs(*keyFile)
		if err != nil {
			return bad(err.Error())
		}
		entry.KeyFile = abs
	}
	if err := entry.Validate(); err != nil {
		return bad(err.Error())
	}
	if _, err := ghapp.LookupApp(path, *name); err == nil && !*replace {
		return bad("apps.json already has an entry named " + *name + "; use --replace to overwrite it")
	}
	// Read the key the way minting will (mode, owner and link checks), so a
	// bad file is refused now rather than at the first mint.
	var key []byte
	if entry.KeyFile != "" {
		key, err = ghapp.LoadKeyFile(entry.KeyFile)
	} else {
		key, err = ghapp.LoadKeyCmd(context.Background(), entry.KeyCmd)
	}
	if err != nil {
		if entry.KeyFile != "" {
			fmt.Fprintln(stderr, "app add:", err)
			return 1
		}
		fmt.Fprintln(stderr, "app add: warning:", err, "(entry recorded without a key fingerprint)")
	} else if fp, ferr := ghapp.KeyFingerprint(append([]byte(nil), key...)); ferr == nil {
		entry.KeyFingerprint = fp
	} else if entry.KeyFile != "" {
		fmt.Fprintln(stderr, "app add: the key file is not a usable private key")
		return 1
	}
	if err := ghapp.SaveApp(path, entry); err != nil {
		fmt.Fprintf(stderr, "app add: could not write %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintln(stdout, "Recorded in", path)
	if *verify {
		m, err := entry.Minter()
		if err == nil {
			_, err = m.Token(context.Background())
		}
		if err != nil {
			fmt.Fprintln(stderr, "app add: verify failed:", err)
			return 1
		}
		fmt.Fprintln(stdout, "MINT-OK")
	}
	return 0
}
