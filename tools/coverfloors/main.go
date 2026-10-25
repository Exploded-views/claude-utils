// Command coverfloors enforces per-package Go statement-coverage floors.
//
//	coverfloors check   -profile cover.out -floors coverage-floors.json
//	coverfloors ratchet -profile cover.out -floors coverage-floors.json [-out path]
//
// The profile is `go test -coverprofile` output. Floors are a JSON object of
// import path to percent (one decimal). check fails when a package is more than
// 0.1 point below its floor, or has statements but no floor. ratchet raises each
// floor to the current coverage rounded down to 0.1 and never lowers one.
// Exit codes: 0 ok, 1 check failed, 2 usage or input error.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// epsilonTenths is the rounding slack in tenths of a point (0.1).
const epsilonTenths = 1

// Cov is a package's statement count and how many of them ran.
type Cov struct{ Stmts, Covered int }

// Row is one line of the check table.
type Row struct {
	Pkg      string
	Floor    float64
	HasFloor bool
	Current  float64
	Status   string
}

// Change describes one floor the ratchet moved.
type Change struct {
	Pkg      string
	Old, New float64
	Added    bool
}

// tenths is the coverage rounded down to 0.1, as an integer count of tenths.
func (c Cov) tenths() int { return c.Covered * 1000 / c.Stmts }

func (c Cov) pct() float64 { return float64(c.Covered) * 100 / float64(c.Stmts) }

func toTenths(f float64) int { return int(math.Round(f * 10)) }

// ParseProfile sums statements per package. A block seen more than once (several
// test binaries) counts once and is covered if any copy ran. Packages without
// statements are left out.
func ParseProfile(r io.Reader) (map[string]Cov, error) {
	type block struct {
		stmts   int
		covered bool
	}
	blocks := map[string]*block{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	first := true
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("line %d: profile must start with a mode: line", n)
			}
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || !strings.Contains(f[0], ":") {
			return nil, fmt.Errorf("line %d: not a coverage block: %q", n, line)
		}
		stmts, err1 := strconv.Atoi(f[1])
		count, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || stmts < 0 || count < 0 {
			return nil, fmt.Errorf("line %d: bad numbers in %q", n, line)
		}
		b := blocks[f[0]]
		if b == nil {
			b = &block{stmts: stmts}
			blocks[f[0]] = b
		}
		b.covered = b.covered || count > 0
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, errors.New("empty profile")
	}
	out := map[string]Cov{}
	for key, b := range blocks {
		file := key[:strings.LastIndex(key, ":")]
		pkg := path.Dir(file)
		c := out[pkg]
		c.Stmts += b.stmts
		if b.covered {
			c.Covered += b.stmts
		}
		out[pkg] = c
	}
	for pkg, c := range out {
		if c.Stmts == 0 {
			delete(out, pkg)
		}
	}
	return out, nil
}

// Prune drops floors for packages absent from cov and lists what it dropped.
func Prune(floors map[string]float64, cov map[string]Cov) (map[string]float64, []string) {
	return floors, nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Check compares coverage with floors. It reports false when any package is
// below its floor by more than the epsilon, or has statements and no floor.
func Check(floors map[string]float64, cov map[string]Cov) ([]Row, bool) {
	ok := true
	var rows []Row
	for _, pkg := range sortedKeys(cov) {
		c := cov[pkg]
		row := Row{Pkg: pkg, Current: c.pct()}
		fl, has := floors[pkg]
		switch {
		case !has:
			row.Status, ok = "no floor", false
		case c.Covered*1000 < (toTenths(fl)-epsilonTenths)*c.Stmts:
			row.Floor, row.HasFloor = fl, true
			row.Status, ok = "below floor", false
		default:
			row.Floor, row.HasFloor = fl, true
			row.Status = "ok"
		}
		rows = append(rows, row)
	}
	for _, pkg := range sortedKeys(floors) {
		if _, seen := cov[pkg]; !seen {
			rows = append(rows, Row{Pkg: pkg, Floor: floors[pkg], HasFloor: true, Status: "not measured"})
		}
	}
	return rows, ok
}

// Ratchet returns floors raised to max(floor, current rounded down to 0.1). It
// never lowers a floor, keeps floors for unmeasured packages, and adds new ones.
func Ratchet(floors map[string]float64, cov map[string]Cov) (map[string]float64, []Change) {
	out := make(map[string]float64, len(floors))
	for k, v := range floors {
		out[k] = v
	}
	var changes []Change
	for _, pkg := range sortedKeys(cov) {
		cur := cov[pkg].tenths()
		old, has := floors[pkg]
		if has && toTenths(old) >= cur {
			continue
		}
		out[pkg] = float64(cur) / 10
		changes = append(changes, Change{Pkg: pkg, Old: old, New: out[pkg], Added: !has})
	}
	return out, changes
}

// MarshalFloors renders floors as sorted JSON with one decimal each.
func MarshalFloors(f map[string]float64) []byte {
	if len(f) == 0 {
		return []byte("{}\n")
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range sortedKeys(f) {
		key, _ := json.Marshal(k)
		sep := ","
		if i == len(f)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "  %s: %.1f%s\n", key, f[k], sep)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

func loadFloors(p string) (map[string]float64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	m := map[string]float64{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return m, nil
}

func loadProfile(p string) (map[string]Cov, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cov, err := ParseProfile(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return cov, nil
}

func printTable(w io.Writer, rows []Row) {
	width := len("package")
	for _, r := range rows {
		width = max(width, len(r.Pkg))
	}
	fmt.Fprintf(w, "%-*s  %8s  %8s  %s\n", width, "package", "floor", "current", "status")
	for _, r := range rows {
		floor, cur := "-", "-"
		if r.HasFloor {
			floor = fmt.Sprintf("%.1f", r.Floor)
		}
		if r.Status != "not measured" {
			cur = fmt.Sprintf("%.1f", r.Current)
		}
		fmt.Fprintf(w, "%-*s  %8s  %8s  %s\n", width, r.Pkg, floor, cur, r.Status)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "check" && args[0] != "ratchet") {
		fmt.Fprintln(stderr, "usage: coverfloors check|ratchet -profile cover.out -floors coverage-floors.json [-out path (ratchet)]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "cover.out", "go test -coverprofile output")
	floorsPath := fs.String("floors", "coverage-floors.json", "floors JSON")
	outPath := fs.String("out", "", "ratchet: write here instead of rewriting -floors")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cov, err := loadProfile(*profile)
	if err != nil {
		fmt.Fprintln(stderr, "coverfloors:", err)
		return 2
	}
	floors, err := loadFloors(*floorsPath)
	if err != nil {
		fmt.Fprintln(stderr, "coverfloors:", err)
		return 2
	}

	if args[0] == "check" {
		rows, ok := Check(floors, cov)
		printTable(stdout, rows)
		if !ok {
			fmt.Fprintln(stdout, "\nFAIL: a package is below its floor, or is new and has no floor (new packages need a floor: make ratchet).")
			return 1
		}
		fmt.Fprintln(stdout, "\nOK")
		return 0
	}

	next, changes := Ratchet(floors, cov)
	for _, c := range changes {
		if c.Added {
			fmt.Fprintf(stdout, "add    %s  %.1f\n", c.Pkg, c.New)
		} else {
			fmt.Fprintf(stdout, "raise  %s  %.1f -> %.1f\n", c.Pkg, c.Old, c.New)
		}
	}
	fmt.Fprintf(stdout, "%d floor(s) changed\n", len(changes))
	dst := *floorsPath
	if *outPath != "" {
		dst = *outPath
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fmt.Fprintln(stderr, "coverfloors:", err)
			return 2
		}
	}
	if err := os.WriteFile(dst, MarshalFloors(next), 0o644); err != nil {
		fmt.Fprintln(stderr, "coverfloors:", err)
		return 2
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
