// Command coverfloors enforces per-package Go statement-coverage floors.
package main

import (
	"io"
	"os"
)

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

func ParseProfile(r io.Reader) (map[string]Cov, error) { return nil, nil }

func Check(floors map[string]float64, cov map[string]Cov) ([]Row, bool) { return nil, true }

func Ratchet(floors map[string]float64, cov map[string]Cov) (map[string]float64, []Change) {
	return nil, nil
}

func MarshalFloors(f map[string]float64) []byte { return nil }

func run(args []string, stdout, stderr io.Writer) int { return 0 }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
