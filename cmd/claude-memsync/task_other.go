//go:build !windows

package main

// On Linux and macOS we use kardianos/service with the UserService option,
// which generates a systemd user unit (~/.config/systemd/user/) on Linux
// and a launchd plist (~/Library/LaunchAgents/) on macOS. The unit/plist
// runs in the user's session — full access to their credentials and SSH
// keys — and can be installed/started without root.

import (
	"fmt"
	"os"

	"github.com/kardianos/service"
)

// userOptions are the kardianos options of the user service.
func userOptions() service.KeyValue {
	// kardianos defaults RunAtLoad to false, which leaves a launchd agent not
	// started at login. KeepAlive defaults to true; it stays.
	return service.KeyValue{"UserService": true, "RunAtLoad": true, "KeepAlive": true}
}

func userSvc(configPath string) (service.Service, error) {
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	cfg := serviceConfig(configPath)
	if cfg.Option == nil {
		cfg.Option = service.KeyValue{}
	}
	for k, v := range userOptions() {
		cfg.Option[k] = v
	}
	prg := &program{configPath: configPath}
	return service.New(prg, cfg)
}

// The kardianos calls are variables so tests never register a real service.
var (
	kardianosInstall = func(svc service.Service) error { return svc.Install() }
	kardianosStart   = func(svc service.Service) error { return svc.Start() }
)

func installTask(configPath string) error {
	svc, err := userSvc(configPath)
	if err != nil {
		return err
	}
	// An existing unit is repaired in place (kardianos refuses to install
	// over one): its autostart target, and its ExecStart when the requested
	// config or routes differ.
	_, content, ok, rerr := loadUserUnit()
	if rerr != nil {
		fmt.Fprintln(os.Stderr, unreadableWarning(rerr))
		return fmt.Errorf("install: cannot read the service unit: %w", rerr)
	}
	if ok {
		exe, err := installExeFn()
		if err != nil {
			return err
		}
		if exeThrowawayFn(exe) {
			// A go run or test binary must not become the installed build:
			// nothing that names a build is touched.
			fmt.Fprintf(os.Stderr, "warning: %s runs from a temp dir; the unit and the bin/ symlinks are left on the installed build\n", exe)
			if userUnitIsWrong() {
				return repairUserUnitTo(os.Stderr, "", "")
			}
			return nil
		}
		if err := relinkBins(os.Stderr, exe); err != nil {
			return fmt.Errorf("install: cannot repoint the bin/ symlinks: %w", err)
		}
		want := ""
		if cur, found := currentExecStart(content); found {
			// Keep the unit's own arguments (and any hand edit); only the
			// flags given on this install replace their counterparts. The
			// binary path is refreshed.
			if _, args, parsed := parseExecStart(cur); parsed {
				if installCfgExplicit && configPath != "" {
					args = withFlag(args, "--config", configPath)
				}
				if installRoutesExplicit && unitRoutes != "" {
					args = withFlag(args, "--routes", unitRoutes)
				}
				want = execStartLine(exe, args)
			}
		}
		_, changed := setExecStart(content, want)
		_, condChanged := setCondition(content, exe)
		if (want != "" && changed) || condChanged || userUnitIsWrong() {
			return repairUserUnitTo(os.Stderr, want, exe)
		}
	}
	if err := kardianosInstall(svc); err != nil {
		return err
	}
	return ensureUserUnitTarget(os.Stderr)
}

func userUnitIsWrong() bool { _, _, w := userUnitWrong(); return w }

func uninstallTask() error {
	svc, err := userSvc("")
	if err != nil {
		return err
	}
	_ = svc.Stop()
	return svc.Uninstall()
}

func startTask() error {
	svc, err := userSvc("")
	if err != nil {
		return err
	}
	if err := ensureUserUnitTarget(os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "start: could not fix the unit's autostart target:", err)
	}
	return kardianosStart(svc)
}

func stopTask() error {
	svc, err := userSvc("")
	if err != nil {
		return err
	}
	return svc.Stop()
}

func statusTask() (string, error) {
	svc, err := userSvc("")
	if err != nil {
		return "unknown", err
	}
	st, err := svc.Status()
	if err != nil {
		return "unknown", err
	}
	switch st {
	case service.StatusRunning:
		return "running", nil
	case service.StatusStopped:
		return "stopped", nil
	default:
		return "unknown", nil
	}
}
