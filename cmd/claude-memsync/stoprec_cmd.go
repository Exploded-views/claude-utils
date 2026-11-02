package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/proc"
	"github.com/MarimerLLC/claude-utils/internal/scrub"
	"github.com/MarimerLLC/claude-utils/internal/stoprec"
	"github.com/MarimerLLC/claude-utils/internal/version"
)

// journalUnit is the systemd user unit whose journal a record carries.
const journalUnit = "claude-memsync.service"

// journalMaxBytes bounds what is read from the journal, whatever it sends.
const journalMaxBytes = 1 << 20

// journalTimeout is how long record-stop waits for the journal. A variable so
// a test can shorten it.
var journalTimeout = 5 * time.Second

// journalctlFn returns the last n lines of the service's journal; tests replace
// it (TestMain makes it panic: no test reads the real journal).
var journalctlFn = runJournalctl

// recordStopHook runs first in record-stop's guarded body; a test makes it
// panic to prove the recover.
var recordStopHook = func() {}

// recordStopConfigFn is the config whose denylist the tail is scrubbed with.
var recordStopConfigFn = recordStopConfig

func runJournalctl(ctx context.Context, n int) (string, error) {
	cmd := proc.GroupedCommandContext(ctx, "journalctl", "--user", "-u", journalUnit,
		"-n", fmt.Sprint(n), "--no-pager", "-o", "short-iso")
	var out capped
	cmd.Stdout = &out
	if err := proc.Run(cmd); err != nil {
		return "", err
	}
	return out.String(), ctx.Err()
}

// capped keeps the first journalMaxBytes written and drops the rest.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := journalMaxBytes - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func recordStopUsage(w io.Writer) {
	fmt.Fprint(w, `usage: claude-memsync record-stop [--result R] [--exit-code C] [--exit-status S]

Record an unclean stop of the service: the result and exit code the service
manager reports, and a scrubbed tail of the journal, in a private, bounded
record under the per-host config directory (stops/). It runs as the unit's
ExecStopPost, which gives it SERVICE_RESULT, EXIT_CODE and EXIT_STATUS; the
flags override them. A clean stop (success, exited, 0) does nothing.

It always exits 0 and never fails the unit; problems go to stderr.
Lines that look like secrets are withheld and denylisted paths are redacted, but
prose cannot be detected: review a record before pasting it.
`)
}

// runRecordStop is the record-stop command. It returns 0 whatever happens.
func runRecordStop(args []string, env func(string) string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "record-stop: panic: %v\n", r)
		}
		code = 0
	}()
	if runtime.GOOS == "windows" {
		fmt.Fprintln(stdout, "record-stop: not supported on windows")
		return 0
	}
	recordStopHook()

	fs := flag.NewFlagSet("record-stop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	result := fs.String("result", env("SERVICE_RESULT"), "")
	exitCode := fs.String("exit-code", env("EXIT_CODE"), "")
	exitStatus := fs.String("exit-status", env("EXIT_STATUS"), "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			recordStopUsage(stdout)
			return 0
		}
		fmt.Fprintf(stderr, "record-stop: %v (-h shows its usage)\n", err)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "record-stop: unexpected argument %q (-h shows its usage)\n", fs.Arg(0))
		return 0
	}
	if *result == "success" && *exitCode == "exited" && *exitStatus == "0" {
		return 0 // a clean stop: no I/O, no spawn
	}
	source := "manual" // a flag was given, or nothing came from the service manager
	if env("SERVICE_RESULT") != "" {
		source = "systemd"
	}
	fs.Visit(func(*flag.Flag) { source = "manual" })

	dir, err := stoprec.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "record-stop: no records directory: %v\n", err)
		return 0
	}
	// Refuse the directory before spending the journal deadline on it.
	if err := stoprec.EnsureDir(dir); err != nil {
		fmt.Fprintf(stderr, "record-stop: %v\n", err)
		return 0
	}

	lines, err := journalTail(journalctlFn, stoprec.Default.MaxLines)
	if err != nil {
		fmt.Fprintf(stderr, "record-stop: no journal tail: %v\n", err)
	}
	sc := recordStopScanner(stderr)
	kept, withheld := stoprec.ScrubTail(lines, sc)

	rec := stoprec.Record{
		Result: *result, ExitCode: *exitCode, ExitStatus: *exitStatus,
		Version: version.Strings("claude-memsync"), Source: source,
		Tail: kept, Withheld: withheld,
	}
	if err := stoprec.Write(dir, rec, time.Now(), stoprec.Default); err != nil {
		fmt.Fprintf(stderr, "record-stop: %v\n", err)
	}
	return 0
}

// journalTail asks fn for the last n lines under journalTimeout. A seam that
// ignores its context is abandoned at the deadline.
func journalTail(fn func(context.Context, int) (string, error), n int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()
	type res struct {
		out string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- res{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		out, err := fn(ctx, n)
		ch <- res{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return lastLines(r.out, n), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("the journal did not answer within %v", journalTimeout)
	}
}

// lastLines is the last n lines of out, looking only at its last journalMaxBytes.
func lastLines(out string, n int) []string {
	if len(out) > journalMaxBytes {
		out = out[len(out)-journalMaxBytes:]
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// recordStopConfig is the primary repo's config, read without the trust check
// (no git), for its denylist.
func recordStopConfig() (config.Config, error) {
	if path, ok, err := resolveRoutesPath(""); err != nil {
		return config.Config{}, err
	} else if ok {
		set, err := loadRoutedWith(path, loadConfigHook)
		if err != nil {
			return config.Config{}, err
		}
		if c, ok := set.Primary(); ok {
			return c, nil
		}
	}
	return loadHookConfig("")
}

// recordStopScanner is the scanner for the tail: the primary denylist when
// there is one, none when there is no denylist, and nil (every path-like
// token is redacted) when it cannot be told or read.
func recordStopScanner(stderr io.Writer) *scrub.Scanner {
	cfg, err := recordStopConfigFn()
	if err != nil {
		fmt.Fprintf(stderr, "record-stop: cannot read the config (%v); redacting every path\n", err)
		return nil
	}
	sc, hideAll := denylistScanner(cfg, stderr)
	switch {
	case hideAll:
		return nil
	case sc == nil:
		return scrub.New(nil)
	}
	return sc
}

// recordStopMain is the process entry for record-stop.
func recordStopMain(args []string) int {
	return runRecordStop(args, os.Getenv, os.Stdout, os.Stderr)
}

// uncleanStopLine is the status line for the newest record, "" if none.
func uncleanStopLine() string {
	dir, err := stoprec.Dir()
	if err != nil {
		return ""
	}
	rec, ok, err := stoprec.Latest(dir)
	if err != nil || !ok {
		return ""
	}
	var parts []string
	if rec.ExitStatus != "" {
		parts = append(parts, "exit "+rec.ExitStatus)
	}
	if rec.Repeats > 1 {
		parts = append(parts, fmt.Sprintf("×%d", rec.Repeats))
	}
	detail := ""
	if len(parts) > 0 {
		detail = " (" + strings.Join(parts, ", ") + ")"
	}
	return fmt.Sprintf("last unclean stop: %s %s%s; records in %s", rec.Last.UTC().Format(time.RFC3339), rec.Result, detail, dir)
}
