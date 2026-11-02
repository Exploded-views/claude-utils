package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/stamplog"
)

// nowFn is the clock log append stamps with; tests replace it.
var nowFn = time.Now

// runLog implements `claude-memsync log append|check`: UTC-minute-stamped
// decision and state logs.
func runLog(args []string) int { return logCmd(os.Stdout, os.Stderr, os.Stdin, args) }

const logUsage = `usage:
  claude-memsync log append [--wait 5s] <file> [text...]
  claude-memsync log check [--] <file>...
append adds "- YYYY-MM-DD HH:MMZ <text>" (UTC, now) as one line; with no text it reads one line from stdin.
check prints "path:line: reason" for every line a prefix filter would skip.`

const logAppendUsage = `usage: claude-memsync log append [--wait 5s] <file> [text...]

Append one entry, "- YYYY-MM-DD HH:MMZ <text>", stamped with the current UTC minute.
The text is the remaining arguments joined by one space, or, with none, one line from
stdin. A new file is created 0600. Nothing is written when the text is empty, holds a
newline or another control character (a tab is fine), or makes the line longer than
4096 bytes (exit 2), or when the file does not end with a newline (exit 1; run log
check). Writers wait up to --wait for each other (exit 1 on timeout).
Exit status: 0 written, 1 refused by the file or the lock, 2 usage or bad text.`

const logCheckUsage = `usage: claude-memsync log check [--] <file>...

Print "path:line: reason" for every line that is not an entry, a "#" heading or a
blank line, has a bad stamp, no text, a control character, more than 4096 bytes, or
no final newline. Meant for files that follow the entry format only.
Exit status: 0 clean, 1 problems found, 2 usage or read error.`

func logCmd(stdout, stderr io.Writer, stdin io.Reader, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, logUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help":
		fmt.Fprintln(stdout, logUsage)
		return 0
	case "append":
		return logAppend(stdout, stderr, stdin, args[1:])
	case "check":
		return logCheck(stdout, stderr, args[1:])
	}
	fmt.Fprintf(stderr, "log: unknown subcommand %q\n%s\n", args[0], logUsage)
	return 2
}

func logAppend(stdout, stderr io.Writer, stdin io.Reader, args []string) int {
	flags := flag.NewFlagSet("log append", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, logAppendUsage) }
	wait := flags.Duration("wait", stamplog.DefaultLockTimeout, "how long to wait for another writer")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() == 0 {
		fmt.Fprintln(stderr, logAppendUsage)
		return 2
	}
	path, words := flags.Arg(0), flags.Args()[1:]
	text := strings.Join(words, " ")
	if len(words) == 0 {
		b, err := io.ReadAll(io.LimitReader(stdin, stamplog.MaxLine+1))
		if err != nil {
			fmt.Fprintln(stderr, "log append: reading stdin:", err)
			return 2
		}
		text = strings.TrimSuffix(string(b), "\n")
	}
	if err := stamplog.Append(path, text, nowFn(), stamplog.Options{LockTimeout: *wait}); err != nil {
		fmt.Fprintln(stderr, "log append:", err)
		if errors.Is(err, stamplog.ErrText) {
			return 2
		}
		return 1
	}
	return 0
}

func logCheck(stdout, stderr io.Writer, args []string) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintln(stdout, logCheckUsage)
		return 0
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	} else if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, logCheckUsage)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, logCheckUsage)
		return 2
	}
	problems, readErr := false, false
	for _, p := range args {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(stderr, "log check:", err)
			readErr = true
			continue
		}
		for _, pr := range stamplog.Check(f) {
			fmt.Fprintf(stdout, "%s:%d: %s\n", p, pr.Line, pr.Reason)
			problems = true
		}
		f.Close()
	}
	switch {
	case readErr:
		return 2
	case problems:
		return 1
	}
	return 0
}
