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

	"github.com/kardianos/service"

	"github.com/MarimerLLC/claude-utils/internal/config"
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
	cfg, err := config.Load(p.configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", p.configPath, err)
	}
	p.run, err = startSingle(context.Background(), cfg)
	return err
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

func runInstall(args []string) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	routesFlag := flags.String("routes", "", "path to routes.json to run several repos (default: $"+routesEnv+", then ~/.config/claude-memsync/routes.json if it exists)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" {
		*cfgPath = defaultConfigPath()
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
	if err := installTask(abs); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "installed; start with `claude-memsync start`")
	return 0
}

func runUninstall(args []string) int {
	if err := uninstallTask(); err != nil {
		fmt.Fprintln(os.Stderr, "uninstall:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "uninstalled")
	return 0
}

func runStart(args []string) int {
	if err := startTask(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "started")
	return 0
}

func runStop(args []string) int {
	if err := stopTask(); err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
		return 1
	}
	// A pull the stop cut short leaves its marker; one that no stopped rebase
	// or merge needs must not outlive the pull.
	for _, cfg := range allConfigs() {
		if err := syncpkg.ClearStalePullMarker(cfg.SyncDir); err != nil {
			fmt.Fprintln(os.Stderr, "stop: clear pull marker:", err)
		}
	}
	fmt.Fprintln(os.Stderr, "stopped")
	return 0
}

func runStatus(args []string) int {
	st, err := statusTask()
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		return 1
	}
	fmt.Println(st)
	if path, ok, err := resolveRoutesPath(""); err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
	} else if ok {
		set, err := loadRouted(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
			return 1
		}
		for _, l := range set.statusLines() {
			fmt.Println(l)
		}
		return 0
	}
	if cfg, err := loadDistillConfig(""); err == nil {
		if line := pausedLine(cfg); line != "" {
			fmt.Println(line)
		}
	}
	return 0
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
func runService(configPath, routesFlag string) int {
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
		if c, err := config.Load(configPath); err == nil {
			pidDir = c.SyncDir
		}
	}
	if pidDir == "" {
		pidDir = defaultSyncDir()
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
func allConfigs() []config.Config {
	if path, ok, err := resolveRoutesPath(""); err == nil && ok {
		if set, err := loadRouted(path); err == nil {
			var out []config.Config
			for _, n := range set.names() {
				out = append(out, set.Cfgs[n])
			}
			return out
		}
	}
	if cfg, err := loadDistillConfig(""); err == nil {
		return []config.Config{cfg}
	}
	return nil
}
