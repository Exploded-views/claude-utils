package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/route"
)

// Test seams.
var (
	hostOS = runtime.GOOS
	// beforeInheritWrite runs between the shown diff and the locked write.
	beforeInheritWrite = func() {}
	stdinIsTTY         = func() bool {
		st, err := os.Stdin.Stat()
		return err == nil && st.Mode()&os.ModeCharDevice != 0
	}
)

func runRoute(args []string) int { return routeCmd(args, os.Stdin, os.Stdout, os.Stderr) }

const routeUsage = `usage:
  claude-memsync route home [<repo>|deny] [--folder F] [--confirm-move-out-of-client]
  claude-memsync route home --unpin | --suggest | --ask
  claude-memsync route publish --as <label> [--registry DIR]
  claude-memsync route inherit <label> [--home] [--yes] [--confirm-move-out-of-client] [--registry DIR]
all take --routes <file>`

func routeCmd(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errw, routeUsage)
		return 2
	}
	switch args[0] {
	case "home":
		return routeHome(args[1:], in, out, errw)
	case "publish":
		return routePublish(args[1:], out, errw)
	case "inherit":
		return routeInherit(args[1:], in, out, errw)
	}
	fmt.Fprintf(errw, "route: unknown subcommand %q\n%s\n", args[0], routeUsage)
	return 2
}

// parseMixed parses flags that may come before or after the positional
// arguments.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func routesFile(flagVal string) (string, error) {
	p, ok, err := resolveRoutesPath(flagVal)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no routes file (looked at %s); pass --routes", defaultRoutesPath())
	}
	return p, nil
}

func repoConfigured(f route.File) func(string) bool {
	return func(name string) bool {
		r, ok := f.Repos[name]
		if !ok {
			return false
		}
		_, err := os.Stat(route.ExpandHome(r.Config))
		return err == nil
	}
}

func routeHome(args []string, in io.Reader, out, errw io.Writer) int {
	fs := flag.NewFlagSet("route home", flag.ContinueOnError)
	fs.SetOutput(errw)
	routes := fs.String("routes", "", "routes file")
	folder := fs.String("folder", "", "home folder name (default: from $HOME)")
	confirm := fs.Bool("confirm-move-out-of-client", false, "allow taking the home folder out of a client or orch repo")
	suggest := fs.Bool("suggest", false, "print a suggested choice and the reason; changes nothing")
	unpin := fs.Bool("unpin", false, "remove the home pin (the folder then routes by the other rules)")
	ask := fs.Bool("ask", false, "choose interactively (needs a terminal)")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(errw, "route home: "+format+"\n", a...)
		return 1
	}
	if len(pos) > 1 {
		return fail("one repo name or deny, not %d arguments", len(pos))
	}
	if *ask && !stdinIsTTY() {
		return fail("stdin is not a terminal: pass the repo instead: claude-memsync route home <repo>|deny")
	}
	path, err := routesFile(*routes)
	if err != nil {
		return fail("%v", err)
	}
	hf := *folder
	if hf == "" {
		if hf, err = route.HomeFolder(); err != nil {
			return fail("%v", err)
		}
	}
	f, err := route.Load(path)
	if err != nil {
		return fail("%v", err)
	}
	if *unpin && (len(pos) > 0 || *suggest || *ask) {
		return fail("--unpin takes no repo and cannot be combined with --suggest or --ask")
	}
	opts := route.HomeOpts{ConfirmMoveOutOfClient: *confirm, Unpin: *unpin, Logf: func(format string, a ...any) {
		fmt.Fprintf(errw, format+"\n", a...)
	}}
	apply := func(target string) int {
		repo := target
		if target == "deny" {
			repo = ""
		}
		ch, err := route.SetHomePin(path, hf, repo, opts)
		if err != nil {
			return fail("%v", err)
		}
		switch {
		case *unpin && !ch.Changed:
			fmt.Fprintln(out, "home folder: no pin to remove")
		case *unpin:
			fmt.Fprintln(out, "home pin removed; the folder now routes by the other rules\nRestart the daemon to apply it.")
		case !ch.Changed:
			fmt.Fprintf(out, "home folder: unchanged (%s)\n", describeTarget(target))
		default:
			fmt.Fprintf(out, "home folder now: %s\nRestart the daemon (or wait for its next pull tick after a restart) to apply it.\n", describeTarget(target))
		}
		return 0
	}

	switch {
	case *suggest || *ask:
		if len(pos) > 0 {
			return fail("--suggest and --ask take no repo")
		}
		s, why := route.SuggestHome(f, hostOS, repoConfigured(f), hf)
		if s == "" {
			s = "deny"
		}
		fmt.Fprintf(out, "suggest: %s\nreason: %s\n", s, why)
		if *suggest {
			return 0
		}
		rd := bufio.NewReader(in)
		choice, err := askHome(f, s, rd, out)
		if err != nil {
			return fail("%v", err)
		}
		// A bare Enter never applies a change that needs consent: it must be
		// typed out.
		if !*confirm {
			repo := choice
			if repo == "deny" {
				repo = ""
			}
			if _, _, err := f.WithHome(hf, repo, route.HomeOpts{}); errors.Is(err, route.ErrNeedsConfirm) {
				fmt.Fprintf(out, "\n%v\nType yes to confirm: ", err)
				line, _ := rd.ReadString('\n')
				if strings.TrimSpace(line) != "yes" {
					return fail("not confirmed; nothing changed")
				}
				opts.ConfirmMoveOutOfClient = true
			}
		}
		return apply(choice)
	case *unpin:
		return apply("")
	case len(pos) == 1:
		return apply(pos[0])
	}
	pin, has := f.HomePin(hf)
	eff := f.Effective(hf)
	if has && pin == "" {
		fmt.Fprintln(out, "home folder: pinned: deny")
	} else if has {
		fmt.Fprintf(out, "home folder: pinned to %s\n", pin)
	} else {
		fmt.Fprintln(out, "home folder: no pin")
	}
	if eff == "" {
		fmt.Fprintln(out, "routes to: denied (no repo carries it)")
	} else {
		fmt.Fprintf(out, "routes to: %s (%s)\n", eff, f.Repos[eff].Kind)
	}
	return 0
}

func describeTarget(t string) string {
	if t == "deny" {
		return "denied, no repo carries it"
	}
	return t
}

// askHome lists the repos by kind and reads one choice: a number, a repo name,
// "deny", or Enter for the suggestion.
func askHome(f route.File, suggested string, rd *bufio.Reader, out io.Writer) (string, error) {
	fmt.Fprintln(out, "\nWhere should the home folder go?")
	var names []string
	n := 0
	for _, kind := range []string{route.KindPersonal, route.KindProject, route.KindInfra, route.KindOrch, route.KindClient} {
		var of []string
		for _, name := range f.RepoNames() {
			if f.Repos[name].Kind == kind {
				of = append(of, name)
			}
		}
		if len(of) == 0 {
			continue
		}
		fmt.Fprintf(out, " %s:\n", kind)
		for _, name := range of {
			n++
			names = append(names, name)
			mark := ""
			if name == suggested {
				mark = " (suggested)"
			}
			fmt.Fprintf(out, "  %d) %s%s\n", n, name, mark)
		}
	}
	n++
	names = append(names, "deny")
	mark := ""
	if suggested == "deny" {
		mark = " (suggested)"
	}
	fmt.Fprintf(out, " deny:\n  %d) deny%s\n", n, mark)
	fmt.Fprint(out, "Choice (number or name, Enter for the suggestion): ")
	line, err := rd.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no choice read")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return suggested, nil
	}
	if i, err := strconv.Atoi(line); err == nil {
		if i < 1 || i > len(names) {
			return "", fmt.Errorf("no choice %d", i)
		}
		return names[i-1], nil
	}
	for _, nm := range names {
		if nm == line {
			return nm, nil
		}
	}
	return "", fmt.Errorf("%q is not a repo of this host or deny", line)
}

func routePublish(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("route publish", flag.ContinueOnError)
	fs.SetOutput(errw)
	routes := fs.String("routes", "", "routes file")
	label := fs.String("as", "", "label for this host's template (a name you choose, such as laptop-2)")
	reg := fs.String("registry", "", "registry repo work-tree (default: \"registry\" in routes.json)")
	if _, err := parseMixed(fs, args); err != nil {
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(errw, "route publish:", err); return 1 }
	if *label == "" {
		return fail(fmt.Errorf("--as <label> is required"))
	}
	path, err := routesFile(*routes)
	if err != nil {
		return fail(err)
	}
	hf, _ := route.HomeFolder()
	hd, _ := os.UserHomeDir()
	p, err := route.PublishTemplate(path, *reg, *label, hf, hd)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(out, "published %s (committed locally; the registry's own sync pushes it)\n", p)
	return 0
}

func routeInherit(args []string, in io.Reader, out, errw io.Writer) int {
	fs := flag.NewFlagSet("route inherit", flag.ContinueOnError)
	fs.SetOutput(errw)
	routes := fs.String("routes", "", "routes file")
	reg := fs.String("registry", "", "registry repo work-tree (default: \"registry\" in routes.json)")
	home := fs.Bool("home", false, "apply the template's home pin to this host's home folder")
	yes := fs.Bool("yes", false, "apply without asking")
	confirm := fs.Bool("confirm-move-out-of-client", false, "allow dropping this host's client or orch routes")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(errw, "route inherit: "+format+"\n", a...)
		return 1
	}
	if len(pos) != 1 {
		return fail("exactly one label is required")
	}
	label := pos[0]
	if err := route.ValidLabel(label); err != nil {
		return fail("%v", err)
	}
	path, err := routesFile(*routes)
	if err != nil {
		return fail("%v", err)
	}
	cur, err := route.Load(path)
	if err != nil {
		return fail("%v", err)
	}
	dir, err := route.RegistryDir(cur, *reg)
	if err != nil {
		return fail("%v", err)
	}
	tpl, err := route.ReadTemplate(dir, label)
	if err != nil {
		return fail("%v", err)
	}
	hf, _ := route.HomeFolder()
	folders := knownFolders(cur)
	next, rep, err := tpl.Inherit(cur, hf, *home, folders)
	if err != nil {
		return fail("%v", err)
	}
	for _, d := range rep.Dropped {
		fmt.Fprintln(out, "dropped:", d)
	}
	diff := route.DiffRoutes(cur.Routes, next.Routes)
	if len(diff) == 0 {
		fmt.Fprintln(out, "nothing to change")
		return 0
	}
	fmt.Fprintln(out, "changes to the rules of "+path+":")
	for _, l := range diff {
		fmt.Fprintln(out, "  "+l)
	}
	if len(rep.Leaving) > 0 && !*confirm {
		for _, l := range rep.Leaving {
			fmt.Fprintln(errw, "  would leave a client repo:", l)
		}
		return fail("this takes folders out of client or orch repos: pass --confirm-move-out-of-client if that is intended")
	}
	if !*yes {
		if !stdinIsTTY() {
			return fail("not applied: pass --yes to apply without asking")
		}
		fmt.Fprint(out, "Apply? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "not applied")
			return 1
		}
	}
	beforeInheritWrite()
	err = route.EditRoutes(path, func(f route.File) (route.File, error) {
		// What was shown and confirmed must be what is written: recompute
		// under the lock and refuse if the file moved meanwhile.
		g, r2, err := tpl.Inherit(f, hf, *home, folders)
		if err != nil {
			return f, err
		}
		if !route.SameRoutes(g.Routes, next.Routes) || !slices.Equal(r2.Leaving, rep.Leaving) {
			return f, fmt.Errorf("routes.json changed while this was decided; nothing written, run it again")
		}
		return g, nil
	})
	if err != nil {
		return fail("%v", err)
	}
	if len(rep.Leaving) > 0 {
		fmt.Fprintf(errw, "route: inherited %q: %d folder(s) or rule(s) left a client repo (confirmed)\n", label, len(rep.Leaving))
	}
	fmt.Fprintln(out, "applied; restart the daemon to use it")
	return 0
}

// knownFolders lists the Claude project folders of every repo of this host.
func knownFolders(f route.File) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range f.RepoNames() {
		cfg, err := loadConfig(route.ExpandHome(f.Repos[n].Config))
		if err != nil {
			continue
		}
		for _, d := range claudeFolders(cfg.ClaudeProjectsDir) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}
