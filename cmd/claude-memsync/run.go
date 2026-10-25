package main

import (
	"flag"
	"fmt"
	"os"
)

// runRun implements `claude-memsync run`. It delegates to the kardianos
// service runner, which works identically when launched interactively or
// by a system service supervisor (SCM / systemd / launchd).
func runRun(args []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	routes := flags.String("routes", "", "path to routes.json (default: $"+routesEnv+", then ~/.config/claude-memsync/routes.json if it exists)")
	var opts guardOpts
	flags.BoolVar(&opts.IgnoreForeign, "ignore-foreign-writers", false, "start although a foreignWriters entry is active or cannot be checked (logged)")
	flags.BoolVar(&opts.ForceQuiet, "quiet-window", false, "run the start-up quiet window although this is not the first start after init")
	flags.BoolVar(&opts.SkipQuiet, "skip-quiet-window", false, "skip the start-up quiet window on a deliberate first start")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := opts.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 2
	}
	return runService(*cfgPath, *routes, opts)
}
