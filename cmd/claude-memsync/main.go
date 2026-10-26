package main

import (
	"fmt"
	"io"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		os.Exit(runVersion(os.Stdout, os.Stderr, os.Args[2:]))
	case "help", "--help", "-h":
		usage()
	case "init":
		os.Exit(runInit(os.Args[2:]))
	case "doctor":
		os.Exit(runDoctor(os.Args[2:]))
	case "repo":
		os.Exit(runRepo(os.Args[2:]))
	case "app":
		os.Exit(runApp(os.Args[2:]))
	case "app-token":
		os.Exit(runAppToken(os.Args[2:]))
	case "run":
		os.Exit(runRun(os.Args[2:]))
	case "distill":
		os.Exit(runDistill(os.Args[2:]))
	case "archive":
		os.Exit(runArchive(os.Args[2:]))
	case "age-filter":
		os.Exit(runAgeFilter(os.Args[2:]))
	case "age-textconv":
		os.Exit(runAgeTextconv(os.Args[2:]))
	case "recipients":
		os.Exit(runRecipients(os.Args[2:]))
	case "join":
		os.Exit(runJoin(os.Args[2:]))
	case "identity":
		os.Exit(runIdentity(os.Args[2:]))
	case "route":
		os.Exit(runRoute(os.Args[2:]))
	case "changes":
		os.Exit(runChanges(os.Args[2:]))
	case "relate":
		os.Exit(runRelate(os.Args[2:]))
	case "unrelate":
		os.Exit(runUnrelate(os.Args[2:]))
	case "related":
		os.Exit(runRelated(os.Args[2:]))
	case "host":
		os.Exit(runHost(os.Args[2:]))
	case "install":
		os.Exit(runInstall(os.Args[2:]))
	case "uninstall":
		os.Exit(runUninstall(os.Args[2:]))
	case "start":
		os.Exit(runStart(os.Args[2:]))
	case "stop":
		os.Exit(runStop(os.Args[2:]))
	case "status":
		os.Exit(runStatus(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runVersion(stdout, stderr io.Writer, args []string) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintln(stdout, "usage: claude-memsync version [--json]\n\nPrint the version (--json: one JSON line with name, version, commit, go, os, arch)")
		return 0
	}
	if len(args) == 1 && args[0] == "--json" {
		fmt.Fprintln(stdout, version.JSON("claude-memsync"))
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "version: unexpected argument %q (the command takes no flags; -h shows its usage)\n", args[0])
		return 2
	}
	fmt.Fprintln(stdout, version.Strings("claude-memsync"))
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, `claude-memsync — sync Claude Code memories across workstations

Usage:
  claude-memsync <subcommand> [args]

Subcommands:
  init        Bootstrap the local sync repo against a remote
  doctor      Check every mem-* repo in the org is private (--org, --install-timer)
  app         app register: create a GitHub App through the manifest flow
  app-token   Print a GitHub App installation token (--app <name>) for use as another tool's token command
  repo        repo create <name>: create a private mem-* repo and verify it
  run         Run the sync daemon in the foreground
  distill     Rebuild the distilled-memory catalog index (see --prune, --dry-run)
  archive     List or search the archive of deleted notes (archive list, archive search <term>)
  route       Edit routing: the home folder, publish or inherit a host template
  changes     List memory files changed in the sync repo since a commit (for hooks)
  relate      Declare a read or join relation between two memory repos (registry)
  unrelate    Remove a relation
  related     List relations; "related sync" fetches read clones, "related context" prints a session index, "related suggest" offers edges from this host's data
  recipients  add | remove | list | accept the age recipients of an encrypted repo
  identity    identity backup: store this host's age identity through a password-manager command, and verify it
  join        join request|status|list|accept|reject: add a new host as an age recipient of an existing repo
  host        list | check-key: the machines that share these repos (deploy keys, recipients, expiry)
  install     Install as a system service (Windows Service / systemd unit / launchd plist)
  uninstall   Remove the system service
  start       Start the system service
  stop        Stop the system service
  status      Report service status
  version     Print version
  help        Show this help`)
}
