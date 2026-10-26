package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/route"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// program implements kardianos/service.Interface so that the `run`
// subcommand works identically when launched from a terminal, by a Windows
// scheduled task, or by a systemd/launchd user unit. The lifecycle install
// path (install/uninstall/start/stop/status) is handled separately per
// platform — see task_windows.go and task_other.go.
type program struct {
	configPath string
	routes     string // routes.json when several repos run in this process
	run        *running
	loop       *syncpkg.Loop // the single-repo loop, set by Start; tests inspect it
}

func (p *program) Start(s service.Service) error {
	if p.routes != "" {
		set, err := loadRouted(p.routes)
		if err != nil {
			return err
		}
		p.run, err = set.Start(context.Background())
		return err
	}
	cfg, err := loadConfig(p.configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", p.configPath, err)
	}
	p.run, err = startSingle(context.Background(), cfg)
	if err != nil {
		return err
	}
	p.loop = p.run.loop
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.run == nil {
		return nil
	}
	return p.run.Stop()
}

// unitRoutes is the absolute routes.json path `install` writes into the unit
// (empty for a single-repo install). The unit must not depend on the
// environment or default location it happens to be started from.
var unitRoutes string

func serviceConfig(configPath string) *service.Config {
	args := []string{"run"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if unitRoutes != "" {
		args = append(args, "--routes", unitRoutes)
	}
	return &service.Config{
		Name:        "claude-memsync",
		DisplayName: "Claude Memory Sync",
		Description: "Synchronizes Claude Code memories across workstations via a private git repository.",
		Arguments:   args,
	}
}

func defaultConfigPath() string {
	return filepath.Join(defaultSyncDir(), "config.json")
}

func defaultSyncDir() string {
	return config.Defaults().SyncDir
}

// pidFilePath is the location of the daemon PID file. It is written by
// runService at startup (and removed on exit) so that the start/stop/status
// subcommands on Windows can manage the daemon process directly.
func pidFilePath() string {
	return pidFilePathIn(primarySyncDir())
}

func pidFilePathIn(syncDir string) string {
	return filepath.Join(syncDir, "daemon.pid")
}

// primarySyncDir is where the PID file lives: the personal (or first) repo's
// sync dir when routes.json is in use, else the default sync dir.
func primarySyncDir() string {
	if path, ok, err := resolveRoutesPath(""); err == nil && ok {
		if set, err := loadRouted(path); err == nil {
			if c, ok := set.Primary(); ok {
				return c.SyncDir
			}
		}
	}
	return defaultSyncDir()
}

// legacyPidFilePath is where a daemon started before routing wrote its PID.
func legacyPidFilePath() string { return pidFilePathIn(defaultSyncDir()) }

// readPidFile reads the PID file of the running daemon: the current location,
// then the old ~/.claudesync one, so `stop` still finds a daemon that was
// started before an upgrade.
func readPidFile() (int, error) {
	var firstErr error
	for _, p := range []string{pidFilePath(), legacyPidFilePath()} {
		b, err := os.ReadFile(p)
		if err == nil {
			var pid int
			if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid, nil
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return 0, firstErr
}

func writePidFileAt(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0600)
}

func removePidFile() error {
	err := removePidFileAt(pidFilePath())
	if legacyPidFilePath() != pidFilePath() {
		err = errors.Join(err, removePidFileAt(legacyPidFilePath()))
	}
	return err
}

func removePidFileAt(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// installTaskFn is installTask; tests replace it so no service is touched.
var installTaskFn = installTask

// primaryConfigPath is the config.json of the routes file's primary repo.
func primaryConfigPath(routesPath string) (string, error) {
	set, err := loadRouted(routesPath)
	if err != nil {
		return "", err
	}
	if _, ok := set.Primary(); !ok {
		return "", fmt.Errorf("%s: this host has none of the repos; initialise one first", routesPath)
	}
	name := set.File.Personal
	if _, ok := set.Cfgs[name]; !ok || name == "" {
		name = ""
		for _, n := range set.File.RepoNames() {
			if _, have := set.Cfgs[n]; have {
				name = n
				break
			}
		}
	}
	return route.ExpandHome(set.File.Repos[name].Config), nil
}

func runInstall(args []string) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	routesFlag := flags.String("routes", "", "path to routes.json to run several repos (default: $"+routesEnv+", then ~/.config/claude-memsync/routes.json if it exists)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if rp, ok, err := resolveRoutesPath(*routesFlag); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	} else if ok {
		abs, err := filepath.Abs(rp)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve routes path:", err)
			return 1
		}
		unitRoutes = abs
		if *cfgPath == "" {
			// Routed mode: the legacy ~/.claudesync/config.json need not exist;
			// the personal (primary) repo's config stands in for it.
			p, err := primaryConfigPath(rp)
			if err != nil {
				fmt.Fprintln(os.Stderr, "install:", err)
				return 1
			}
			*cfgPath = p
		}
	}
	if *cfgPath == "" {
		*cfgPath = defaultConfigPath()
	}
	abs, err := filepath.Abs(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve config path:", err)
		return 1
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(os.Stderr, "config %s not found — run `claude-memsync init` first\n", abs)
		return 1
	}
	if err := installTaskFn(abs); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "installed; start with `claude-memsync start`")
	return 0
}

// The service actions are variables so tests can prove that -h and bad
// arguments never reach them.
var (
	startFn     = startTask
	stopFn      = stopTask
	uninstallFn = uninstallTask
)

func runUninstall(args []string) int {
	if done, code := noFlagArgs("uninstall", "remove the system service", args); done {
		return code
	}
	if err := uninstallFn(); err != nil {
		fmt.Fprintln(os.Stderr, "uninstall:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "uninstalled")
	return 0
}

func runStart(args []string) int {
	if done, code := noFlagArgs("start", "start the system service", args); done {
		return code
	}
	if err := startFn(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "started")
	return 0
}

func runStop(args []string) int {
	if done, code := noFlagArgs("stop", "stop the system service and clear a stale pull marker", args); done {
		return code
	}
	if err := stopFn(); err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
		return 1
	}
	// A pull the stop cut short leaves its marker; one that no stopped rebase
	// or merge needs must not outlive the pull.
	cfgs, err := allConfigs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
		return 1
	}
	for _, cfg := range cfgs {
		if err := syncpkg.ClearStalePullMarker(cfg.SyncDir); err != nil {
			fmt.Fprintln(os.Stderr, "stop: clear pull marker:", err)
		}
	}
	fmt.Fprintln(os.Stderr, "stopped")
	return 0
}

// statusFn reports the service state; tests replace it.
var statusFn = statusTask

// runStatus prints the service state, then the routing and per-repo state.
// A service that is not installed (a foreground run, a dry run) is one line,
// not a failure: the repos are still described. The exit code is non-zero only
// when a repo is in an error state or the routing cannot be read.
func runStatus(args []string) int {
	if done, code := noFlagArgs("status", "report the service state, the routing and each repo's state", args); done {
		return code
	}
	st, err := statusFn()
	code := 0
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		fmt.Println("service: not installed")
	case err != nil:
		fmt.Printf("service: unknown (%v)\n", err)
		code = 1
	default:
		fmt.Println(st)
	}
	if h := legacyIdentityHint(); h != "" {
		fmt.Println(h)
	}
	if handled, c := routedStatus(); handled {
		if c > code {
			code = c
		}
		return code
	}
	if lines := foreignStatusLines(); len(lines) > 0 {
		for _, l := range lines {
			fmt.Println(l)
		}
		code = 1
	}
	if cfg, err := loadDistillConfig(""); err == nil {
		if b, err := os.ReadFile(loopFailedPath(cfg.SyncDir)); err == nil {
			fmt.Println("loop stopped, restarting with backoff: " + strings.TrimSpace(string(b)))
			code = 1
		}
		if line := readOnlyLine(cfg); line != "" {
			fmt.Println(line)
		}
		if line := expiryLine(cfg, time.Now()); line != "" {
			fmt.Println(line)
		}
		if line := pausedLine(cfg); line != "" {
			fmt.Println(line)
		}
		if line := syncpkg.PullRefusedLine(cfg.SyncDir); line != "" {
			fmt.Println(line)
			code = 1
		}
		if line := visibilityLine(cfg); line != "" {
			fmt.Println(line)
		}
		for _, line := range encryptionLines(cfg) {
			fmt.Println(line)
		}
	}
	return code
}

// routedStatus prints the routing part of status. handled is false only when no
// routes file exists; an unusable one is an error, never a silent fall back.
func routedStatus() (handled bool, code int) {
	path, ok, err := resolveRoutesPath("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		return true, 1
	}
	if !ok {
		return false, 0
	}
	set, err := loadRouted(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		return true, 1
	}
	for _, l := range set.statusLines() {
		fmt.Println(l)
	}
	code = 0
	for _, l := range set.foreignLines() {
		fmt.Println(l)
		code = 1
	}
	for _, name := range set.names() {
		cfg := set.Cfgs[name]
		var lines []string
		if l := readOnlyLine(cfg); l != "" {
			lines = append(lines, l)
		}
		if l := expiryLine(cfg, time.Now()); l != "" {
			lines = append(lines, l)
		}
		if l := syncpkg.PullRefusedLine(cfg.SyncDir); l != "" {
			lines = append(lines, l)
			code = 1
		}
		if l := visibilityLine(cfg); l != "" {
			lines = append(lines, l)
		}
		lines = append(lines, encryptionLines(cfg)...)
		for _, l := range lines {
			fmt.Printf("  [%s] %s\n", name, l)
		}
		if _, err := os.Stat(loopFailedPath(cfg.SyncDir)); err == nil {
			code = 1 // a repo whose loop keeps dying is in an error state
		}
	}
	return true, code
}

// pausedLine says that syncing is paused for a rebase or merge being resolved by
// hand, or is "" if it is not. The service can be running and syncing nothing.
func pausedLine(cfg config.Config) string {
	since, ok := syncpkg.PausedSince(cfg.SyncDir)
	if !ok {
		return ""
	}
	when := ""
	if !since.IsZero() {
		when = " since " + since.Local().Format("2006-01-02 15:04")
	}
	return "syncing paused" + when + ": a rebase or merge is in progress in " + cfg.SyncDir +
		"; finish it, or give it up with git -C " + cfg.SyncDir + " rebase --abort (see docs: Rebase conflicts)"
}

// runService is the entry point for `claude-memsync run`. It wraps the loop
// in kardianos's service.Run() so the same code path serves both interactive
// invocation (terminal, Windows scheduled task, systemd user unit) and any
// future move to true SCM-managed mode.
func runService(configPath, routesFlag string, opts guardOpts) int {
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	routes, routed, err := resolveRoutesPath(routesFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	pidDir := ""
	if routed {
		set, err := loadRouted(routes)
		if err != nil {
			fmt.Fprintln(os.Stderr, "run:", err)
			return 1
		}
		if c, ok := set.Primary(); ok {
			pidDir = c.SyncDir
		}
	} else {
		routes = ""
		if c, err := loadConfig(configPath); err == nil {
			pidDir = c.SyncDir
		}
	}
	if pidDir == "" {
		pidDir = defaultSyncDir()
	}
	if err := startGuard(configPath, routes, routed, opts); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	pidPath := pidFilePathIn(pidDir)
	if err := writePidFileAt(pidPath, os.Getpid()); err != nil {
		log.Println("warning: write pid file:", err)
	}
	defer removePidFileAt(pidPath)

	prg := &program{configPath: configPath, routes: routes}
	svc, err := service.New(prg, serviceConfig(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	logger, err := svc.Logger(nil)
	if err == nil {
		log.SetOutput(serviceLogWriter{logger: logger})
	}
	if err := svc.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	return 0
}

type serviceLogWriter struct{ logger service.Logger }

func (w serviceLogWriter) Write(p []byte) (int, error) {
	_ = w.logger.Info(string(p))
	return len(p), nil
}

// allConfigs is every sync dir config the lifecycle commands act on: each repo
// of routes.json this host has, or the default one.
func allConfigs() ([]config.Config, error) {
	path, ok, err := resolveRoutesPath("")
	if err != nil {
		return nil, err
	}
	if ok {
		set, err := loadRouted(path)
		if err != nil {
			return nil, err
		}
		var out []config.Config
		for _, n := range set.names() {
			out = append(out, set.Cfgs[n])
		}
		return out, nil
	}
	cfg, err := loadDistillConfig("")
	if err != nil {
		return nil, err
	}
	return []config.Config{cfg}, nil
}

// noFlagArgs is the argument handling of the commands that take no flags and
// act when run. A lone -h/--help prints the usage and exits 0 without acting;
// any other argument is refused with exit 2, also without acting. done reports
// that the command must return code now.
func noFlagArgs(name, summary string, args []string) (done bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintf(os.Stdout, "usage: claude-memsync %s\n\n%s; takes no flags\n", name, capitalize(summary))
		return true, 0
	}
	fmt.Fprintf(os.Stderr, "%s: unexpected argument %q (the command takes no flags; -h shows its usage)\n", name, args[0])
	return true, 2
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
