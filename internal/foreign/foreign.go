// Package foreign guards against a predecessor sync tool that still writes
// into Claude memory folders after claude-memsync has taken over. Mirroring
// those copies would carry notes into repos they were never routed to.
//
// The guard has two parts: a configured list of foreign writers that are
// checked before a run starts (Check), and a quiet-window check that needs no
// configuration (QuietCheck). Both log and report kinds, configured values and
// folder hashes only, never note names.
package foreign

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Writer is one entry of foreignWriters. Exactly one of File, SystemdUnit,
// Process and ClaudePlugin is set.
type Writer struct {
	// File: active when the file exists and, if JSONKeyNonEmpty is set, that
	// top-level key of the JSON object holds a non-empty value.
	File            string `json:"file,omitempty"`
	JSONKeyNonEmpty string `json:"jsonKeyNonEmpty,omitempty"`
	// SystemdUnit: active when `systemctl --user` calls it enabled or active.
	SystemdUnit string `json:"systemdUnit,omitempty"`
	// Process: active when a running process's command line contains it.
	Process string `json:"process,omitempty"`
	// ClaudePlugin (name@marketplace): active when enabled in
	// ~/.claude/settings.json, unless UnlessFile exists, and only if
	// OnlyIfFile (when set) exists.
	ClaudePlugin string `json:"claudePlugin,omitempty"`
	UnlessFile   string `json:"unlessFile,omitempty"`
	OnlyIfFile   string `json:"onlyIfFile,omitempty"`
}

// Kind and Value name the entry for messages.
func (w Writer) Kind() (kind, value string) {
	switch {
	case w.File != "":
		return "file", w.File
	case w.SystemdUnit != "":
		return "systemdUnit", w.SystemdUnit
	case w.Process != "":
		return "process", w.Process
	case w.ClaudePlugin != "":
		return "claudePlugin", w.ClaudePlugin
	}
	return "", ""
}

// Validate refuses an entry that is ambiguous or empty: a typo here would
// silently switch the guard off.
func Validate(ws []Writer) error {
	for i, w := range ws {
		n := 0
		for _, s := range []string{w.File, w.SystemdUnit, w.Process, w.ClaudePlugin} {
			if s != "" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("foreignWriters[%d]: exactly one of file, systemdUnit, process and claudePlugin is required", i)
		}
		if w.JSONKeyNonEmpty != "" && w.File == "" {
			return fmt.Errorf("foreignWriters[%d]: jsonKeyNonEmpty belongs to file entries only", i)
		}
		if (w.UnlessFile != "" || w.OnlyIfFile != "") && w.ClaudePlugin == "" {
			return fmt.Errorf("foreignWriters[%d]: unlessFile and onlyIfFile belong to claudePlugin entries only", i)
		}
		if w.ClaudePlugin != "" && !strings.Contains(w.ClaudePlugin, "@") {
			return fmt.Errorf("foreignWriters[%d]: claudePlugin must be name@marketplace", i)
		}
	}
	return nil
}

// Env is what the checks touch; tests replace the parts.
type Env struct {
	GOOS string
	Home string
	// ClaudeDir is the Claude config dir (CLAUDE_CONFIG_DIR, else Home/.claude).
	ClaudeDir string
	// Systemctl runs `systemctl <args>` and returns its stdout. The exit status
	// is not an answer (is-enabled exits non-zero for "disabled"): only an
	// empty output with an error means systemctl could not be asked.
	Systemctl func(args ...string) (string, error)
	// Processes lists the command lines of the running processes, this one
	// excluded.
	Processes func() ([]string, error)
	Logf      func(format string, args ...any)
}

// DefaultEnv is the real host.
func DefaultEnv(logf func(string, ...any)) Env {
	home, _ := os.UserHomeDir()
	cdir := os.Getenv("CLAUDE_CONFIG_DIR")
	if cdir == "" {
		cdir = filepath.Join(home, ".claude")
	}
	return Env{GOOS: runtime.GOOS, Home: home, ClaudeDir: cdir, Systemctl: runSystemctl, Processes: procCmdlines, Logf: logf}
}

func runSystemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).Output()
	return string(out), err
}

// Active is a writer found at work.
type Active struct{ Kind, Value string }

func (a Active) String() string { return "foreign writer " + a.Kind + " " + a.Value }

// Result is what a check found: entries that are active, and entries that
// could not be checked on a host where they should have been.
type Result struct{ Active, Unchecked []Active }

// Refusal is the one-line error for a start that must not go ahead, or nil.
// An entry that could not be checked refuses too: failing open is how a
// predecessor slips through.
func Refusal(r Result) error {
	var parts []string
	for _, a := range r.Active {
		parts = append(parts, a.String()+" is active")
	}
	for _, a := range r.Unchecked {
		parts = append(parts, a.String()+" could not be checked")
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "; ") + ", refusing to start (--ignore-foreign-writers overrides)")
}

// Check returns the entries that are active now.
func Check(ws []Writer, env Env) []Active { return Inspect(ws, env).Active }

// Inspect checks every entry. Off Linux, systemdUnit and process entries are
// skipped with a log line (only file and claudePlugin are checked there); on
// Linux, one that cannot be asked is Unchecked.
func Inspect(ws []Writer, env Env) Result {
	if env.Logf == nil {
		env.Logf = func(string, ...any) {}
	}
	var r Result
	for _, w := range ws {
		kind, val := w.Kind()
		a := Active{kind, val}
		switch state := probe(w, env); state {
		case yes:
			r.Active = append(r.Active, a)
		case unknown:
			r.Unchecked = append(r.Unchecked, a)
		}
	}
	return r
}

type tri int

const (
	no tri = iota
	yes
	unknown
)

func b2t(b bool) tri {
	if b {
		return yes
	}
	return no
}

func probe(w Writer, env Env) tri {
	switch {
	case w.File != "":
		return b2t(fileActive(expand(w.File, env.Home), w.JSONKeyNonEmpty))
	case w.SystemdUnit != "":
		if env.GOOS != "linux" {
			env.Logf("foreign writers: skipping systemdUnit %s: not Linux", w.SystemdUnit)
			return no
		}
		return unitActive(w.SystemdUnit, env)
	case w.Process != "":
		if env.GOOS != "linux" {
			env.Logf("foreign writers: skipping process %s: not Linux", w.Process)
			return no
		}
		cmds, err := env.Processes()
		if err != nil {
			env.Logf("foreign writers: cannot list processes (%v); process %s not checked", err, w.Process)
			return unknown
		}
		for _, c := range cmds {
			if strings.Contains(c, w.Process) {
				return yes
			}
		}
		return no
	case w.ClaudePlugin != "":
		return b2t(pluginActive(w, env))
	}
	return no
}

func expand(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// fileActive treats a file that exists but cannot be read or parsed as active:
// it is more likely the predecessor's than not.
func fileActive(path, key string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return !os.IsNotExist(err) && exists(path)
	}
	if key == "" {
		return true
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return true
	}
	raw, ok := m[key]
	if !ok {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return true
	}
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(x) != ""
	case bool:
		return x
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func unitActive(unit string, env Env) tri {
	asked := false
	for _, q := range []struct {
		verb string
		yes  []string
	}{{"is-enabled", []string{"enabled", "enabled-runtime"}}, {"is-active", []string{"active", "activating", "reloading"}}} {
		out, err := env.Systemctl("--user", q.verb, unit)
		out = strings.TrimSpace(out)
		if out == "" && err != nil {
			continue
		}
		asked = true
		for _, y := range q.yes {
			if out == y {
				return yes
			}
		}
	}
	if !asked {
		env.Logf("foreign writers: systemctl --user could not be asked about %s", unit)
		return unknown
	}
	return no
}

// pluginActive reads the Claude settings files that can enable a plugin. A
// file that exists but cannot be read or understood counts as the plugin being
// enabled: the settings are how it would be switched off.
func pluginActive(w Writer, env Env) bool {
	dir := env.ClaudeDir
	if dir == "" {
		dir = filepath.Join(env.Home, ".claude")
	}
	enabled := false
	for _, name := range []string{"settings.json", "settings.local.json"} {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			enabled = true
			continue
		}
		var s struct {
			EnabledPlugins map[string]bool `json:"enabledPlugins"`
		}
		if json.Unmarshal(b, &s) != nil || s.EnabledPlugins[w.ClaudePlugin] {
			enabled = true
		}
	}
	if !enabled {
		return false
	}
	if w.UnlessFile != "" && exists(expand(w.UnlessFile, env.Home)) {
		return false
	}
	if w.OnlyIfFile != "" && !exists(expand(w.OnlyIfFile, env.Home)) {
		return false
	}
	return true
}

// procCmdlines scans /proc. Off Linux there is no /proc and the result is an
// error, which Check logs.
func procCmdlines() ([]string, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := strconv.Itoa(os.Getpid())
	var out []string
	for _, e := range ents {
		name := e.Name()
		if name == self || name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil || len(b) == 0 {
			continue
		}
		out = append(out, strings.ReplaceAll(strings.TrimRight(string(b), "\x00"), "\x00", " "))
	}
	return out, nil
}
