package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/ghapp"
	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
)

// Test seams: registerOnReady is called when the registration form is ready,
// registerWait (when non-zero) shortens the wait for the callback.
var (
	registerOnReady func(ghapp.Ready)
	registerWait    time.Duration
)

const appUsage = `usage:
  claude-memsync app key-fingerprint --key-file <pem>
  claude-memsync app register --name <n> --org <org> --url <homepage> --perm <p:level>... (--key-out <path> | --key-cmd-store "<cmd>" --key-cmd "<cmd>")`

func runApp(args []string) int { return appCmd(os.Stdout, os.Stderr, args) }

func appCmd(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, appUsage)
		return 2
	}
	switch args[0] {
	case "register":
		return appRegister(stdout, stderr, args[1:])
	case "key-fingerprint":
		return appKeyFingerprint(stdout, stderr, args[1:])
	}
	fmt.Fprintln(stderr, appUsage)
	return 2
}

func appRegister(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("app register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "GitHub App name; also the apps.json entry name (required)")
	org := fs.String("org", "", "org that will own the app (required)")
	home := fs.String("url", "", "homepage URL recorded in the app manifest (required)")
	keyOut := fs.String("key-out", "", "write the private key to this new file (mode 0600; refused if it exists)")
	keyStore := fs.String("key-cmd-store", "", "command that reads the private key on stdin and stores it (shell-style quotes, no shell)")
	keyCmd := fs.String("key-cmd", "", "with --key-cmd-store: the command that prints the stored key, recorded in apps.json (shell-style quotes, no shell)")
	appsFile := fs.String("apps-file", "", "apps.json path (default: ~/.config/claude-memsync/apps.json)")
	webURL := fs.String("github-url", "", "GitHub web base URL (default https://github.com; for Enterprise)")
	apiURL := fs.String("api-url", "", "GitHub API base URL (default: public GitHub; for Enterprise); recorded as apiBase in apps.json")
	var permFlags stringList
	fs.Var(&permFlags, "perm", "app permission <name>:<read|write|admin> (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bad := func(msg string) int { fmt.Fprintln(stderr, "app register:", msg); return 2 }
	switch {
	case *name == "" || *org == "" || *home == "":
		return bad("--name, --org and --url are required")
	case (*keyOut == "") == (*keyStore == ""):
		return bad("give exactly one of --key-out and --key-cmd-store (the key is shown only once)")
	case *keyStore != "" && *keyCmd == "":
		return bad("--key-cmd-store needs --key-cmd, the command that reads the key back")
	case *keyOut != "" && *keyCmd != "":
		return bad("--key-cmd goes with --key-cmd-store")
	}
	perms, err := ghapp.ParsePerms(permFlags)
	if err != nil || len(perms) == 0 {
		return bad("at least one valid --perm is required")
	}
	path, err := appsPath(*appsFile)
	if err != nil {
		return bad(err.Error())
	}
	storeArgv, err1 := secretcmd.SplitArgs(*keyStore)
	readArgv, err2 := secretcmd.SplitArgs(*keyCmd)
	if err1 != nil || err2 != nil {
		return bad("--key-cmd-store / --key-cmd: unbalanced quote")
	}
	entry := ghapp.App{Name: *name, Org: *org, KeyCmd: readArgv, APIBase: strings.TrimRight(*apiURL, "/")}
	if *keyOut != "" {
		abs, err := filepath.Abs(*keyOut)
		if err != nil {
			return bad(err.Error())
		}
		*keyOut, entry.KeyFile = abs, abs
		if _, err := os.Lstat(abs); err == nil {
			return bad("--key-out already exists; refusing to overwrite it")
		}
	}
	entry.AppID = "0" // placeholder so Validate can run before anything is created
	if err := entry.Validate(); err != nil {
		return bad(err.Error())
	}
	if _, err := ghapp.LookupApp(path, *name); err == nil {
		return bad("apps.json already has an entry named " + *name)
	}

	conv, err := ghapp.Register(context.Background(), ghapp.RegisterParams{
		Name: *name, Org: *org, Homepage: *home, Perms: perms, WebBase: *webURL, APIBase: *apiURL, WaitTimeout: registerWait,
		OnReady: func(r ghapp.Ready) {
			fmt.Fprintf(stdout, "Open this file in a browser where you are signed in to GitHub as an owner of %s:\n  %s\n  %s\n", *org, r.FormPath, r.FileURL)
			fmt.Fprintf(stdout, "Waiting up to %d minutes for GitHub to call back on %s ...\n", int(ghapp.DefaultRegisterWait.Minutes()), r.CallbackURL)
			if registerOnReady != nil {
				registerOnReady(r)
			}
		},
	})
	if err != nil {
		fmt.Fprintln(stderr, "app register:", err)
		return 1
	}
	defer func() { // best effort: see ghapp.Conversion
		for i := range conv.PEM {
			conv.PEM[i] = 0
		}
	}()
	fingerprint, ferr := ghapp.KeyFingerprint(append([]byte(nil), conv.PEM...))
	if ferr != nil {
		fingerprint = ""
	}
	web := strings.TrimRight(*webURL, "/")
	if web == "" {
		web = ghapp.DefaultWebBase
	}
	fmt.Fprintf(stdout, "App created: id %d, slug %s\nInstall it on the org: %s/apps/%s/installations/new\n", conv.ID, conv.Slug, web, conv.Slug)

	if *keyOut != "" {
		f, err := os.OpenFile(*keyOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = f.Write(conv.PEM)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "app register: the app exists (id %d) but its key could not be written; generate a new key for it in the app settings\n", conv.ID)
			return 1
		}
		fmt.Fprintln(stdout, "Private key written to", *keyOut)
	} else {
		if err := secretcmd.Feed(context.Background(), storeArgv, conv.PEM, secretcmd.DefaultTimeout); err != nil {
			fmt.Fprintf(stderr, "app register: the app exists (id %d) but the key store command failed (%v); generate a new key for it in the app settings\n", conv.ID, err)
			return 1
		}
		fmt.Fprintln(stdout, "Private key stored with the key store command")
	}
	entry.AppID = fmt.Sprint(conv.ID)
	entry.KeyFingerprint = fingerprint
	if err := ghapp.SaveApp(path, entry); err != nil {
		fmt.Fprintf(stderr, "app register: could not write %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintln(stdout, "Recorded in", path)
	return 0
}

func appKeyFingerprint(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("app key-fingerprint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyFile := fs.String("key-file", "", "PEM private key file (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keyFile == "" {
		fmt.Fprintln(stderr, "app key-fingerprint: --key-file is required")
		return 2
	}
	b, err := ghapp.ReadKeyFileAnyMode(*keyFile)
	if err != nil {
		fmt.Fprintln(stderr, "app key-fingerprint:", err)
		return 1
	}
	fp, err := ghapp.KeyFingerprint(b)
	if err != nil {
		fmt.Fprintln(stderr, "app key-fingerprint:", err)
		return 1
	}
	fmt.Fprintln(stdout, fp)
	return 0
}
