package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pkgA = "example.test/m/a"
const pkgB = "example.test/m/b"

func TestParseProfileAggregatesPerPackage(t *testing.T) {
	prof := `mode: atomic
example.test/m/a/x.go:1.1,2.2 4 1
example.test/m/a/x.go:3.1,4.2 6 0
example.test/m/a/y.go:1.1,2.2 10 3
example.test/m/b/z.go:1.1,2.2 5 0
`
	got, err := ParseProfile(strings.NewReader(prof))
	if err != nil {
		t.Fatal(err)
	}
	if got[pkgA] != (Cov{Stmts: 20, Covered: 14}) {
		t.Errorf("a = %+v, want {20 14}", got[pkgA])
	}
	if got[pkgB] != (Cov{Stmts: 5, Covered: 0}) {
		t.Errorf("b = %+v, want {5 0}", got[pkgB])
	}
}

func TestParseProfileMergesDuplicateBlocksByMaxCount(t *testing.T) {
	// the same block reported by two test binaries counts once, covered if either hit it
	prof := `mode: set
example.test/m/a/x.go:1.1,2.2 4 0
example.test/m/a/x.go:1.1,2.2 4 1
example.test/m/a/x.go:3.1,4.2 6 0
example.test/m/a/x.go:3.1,4.2 6 0
`
	got, err := ParseProfile(strings.NewReader(prof))
	if err != nil {
		t.Fatal(err)
	}
	if got[pkgA] != (Cov{Stmts: 10, Covered: 4}) {
		t.Errorf("a = %+v, want {10 4}", got[pkgA])
	}
}

func TestParseProfileSkipsPackagesWithoutStatements(t *testing.T) {
	prof := "mode: set\nexample.test/m/a/x.go:1.1,2.2 0 0\n"
	got, err := ParseProfile(strings.NewReader(prof))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[pkgA]; ok {
		t.Errorf("package with no statements must be skipped, got %+v", got)
	}
}

func TestParseProfileRejectsMalformed(t *testing.T) {
	for _, prof := range []string{
		"example.test/m/a/x.go:1.1,2.2 4 1\n", // no mode line
		"mode: set\nnot a block line\n",
		"mode: set\nexample.test/m/a/x.go:1.1,2.2 four 1\n",
	} {
		if _, err := ParseProfile(strings.NewReader(prof)); err == nil {
			t.Errorf("expected an error for %q", prof)
		}
	}
}

func status(rows []Row, pkg string) string {
	for _, r := range rows {
		if r.Pkg == pkg {
			return r.Status
		}
	}
	return "absent"
}

func TestCheckPassesAtOrAboveFloor(t *testing.T) {
	rows, ok := Check(map[string]float64{pkgA: 70.0}, map[string]Cov{pkgA: {Stmts: 10, Covered: 7}})
	if !ok || status(rows, pkgA) != "ok" {
		t.Errorf("ok=%v rows=%+v", ok, rows)
	}
}

func TestCheckFailsBelowFloorBeyondEpsilon(t *testing.T) {
	// 69.8% against a 70.0 floor is 0.2 below: fail
	rows, ok := Check(map[string]float64{pkgA: 70.0}, map[string]Cov{pkgA: {Stmts: 1000, Covered: 698}})
	if ok || status(rows, pkgA) != "below floor" {
		t.Errorf("ok=%v rows=%+v", ok, rows)
	}
}

func TestCheckToleratesRoundingEpsilon(t *testing.T) {
	// exactly 0.1 below the floor is absorbed; anything more is not
	_, ok := Check(map[string]float64{pkgA: 70.0}, map[string]Cov{pkgA: {Stmts: 1000, Covered: 699}})
	if !ok {
		t.Error("69.9% vs 70.0 floor must pass (epsilon 0.1)")
	}
	_, ok = Check(map[string]float64{pkgA: 70.0}, map[string]Cov{pkgA: {Stmts: 10000, Covered: 6989}})
	if ok {
		t.Error("69.89% vs 70.0 floor must fail")
	}
}

func TestCheckFailsPackageWithoutFloor(t *testing.T) {
	rows, ok := Check(map[string]float64{}, map[string]Cov{pkgA: {Stmts: 10, Covered: 1}})
	if ok || status(rows, pkgA) != "no floor" {
		t.Errorf("ok=%v rows=%+v", ok, rows)
	}
}

func TestCheckStaleFloorIsNotAFailure(t *testing.T) {
	rows, ok := Check(map[string]float64{pkgA: 50.0}, map[string]Cov{})
	if !ok || status(rows, pkgA) != "not measured" {
		t.Errorf("ok=%v rows=%+v", ok, rows)
	}
}

func TestRatchetRaisesRoundsDownAndNeverLowers(t *testing.T) {
	floors := map[string]float64{pkgA: 50.0, pkgB: 90.0}
	cov := map[string]Cov{
		pkgA:               {Stmts: 3, Covered: 2},      // 66.666..% -> 66.6
		pkgB:               {Stmts: 10, Covered: 5},     // 50% < 90 floor: keep 90
		"example.test/m/c": {Stmts: 1000, Covered: 837}, // new: 83.7
	}
	got, changes := Ratchet(floors, cov)
	want := map[string]float64{pkgA: 66.6, pkgB: 90.0, "example.test/m/c": 83.7}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(changes) != 2 {
		t.Errorf("changes = %+v, want 2 (a raised, c added)", changes)
	}
	if floors[pkgA] != 50.0 {
		t.Error("Ratchet must not mutate its input")
	}
}

func TestRatchetKeepsFloorsForUnmeasuredPackages(t *testing.T) {
	got, _ := Ratchet(map[string]float64{pkgA: 50.0}, map[string]Cov{})
	if got[pkgA] != 50.0 {
		t.Errorf("got %v", got)
	}
}

func TestMarshalFloorsSortedOneDecimal(t *testing.T) {
	out := string(MarshalFloors(map[string]float64{pkgB: 90, pkgA: 66.6}))
	want := "{\n  \"" + pkgA + "\": 66.6,\n  \"" + pkgB + "\": 90.0\n}\n"
	if out != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunCheckExitCodesAndTable(t *testing.T) {
	dir := t.TempDir()
	prof := writeFile(t, dir, "cover.out", "mode: set\nexample.test/m/a/x.go:1.1,2.2 5 1\nexample.test/m/a/x.go:3.1,4.2 5 0\n")
	good := writeFile(t, dir, "good.json", `{"`+pkgA+`": 10.0}`)
	bad := writeFile(t, dir, "bad.json", `{"`+pkgA+`": 99.0}`)

	var out, errb bytes.Buffer
	if code := run([]string{"check", "-profile", prof, "-floors", good}, &out, &errb); code != 0 {
		t.Fatalf("good: code %d, out %s err %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), pkgA) {
		t.Errorf("table should name the package: %s", out.String())
	}
	out.Reset()
	if code := run([]string{"check", "-profile", prof, "-floors", bad}, &out, &errb); code != 1 {
		t.Fatalf("bad: code %d, want 1", code)
	}
	if !strings.Contains(out.String(), "below floor") {
		t.Errorf("table should say why: %s", out.String())
	}
	if code := run([]string{"check", "-profile", filepath.Join(dir, "nope"), "-floors", good}, &out, &errb); code != 2 {
		t.Errorf("missing profile: code %d, want 2", code)
	}
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Errorf("unknown command: code %d, want 2", code)
	}
}

func TestRunRatchetWritesFloorsAndOut(t *testing.T) {
	dir := t.TempDir()
	prof := writeFile(t, dir, "cover.out", "mode: set\nexample.test/m/a/x.go:1.1,2.2 5 1\nexample.test/m/a/x.go:3.1,4.2 5 0\n")
	fl := writeFile(t, dir, "f.json", `{"`+pkgA+`": 10.0}`)
	var out, errb bytes.Buffer
	if code := run([]string{"ratchet", "-profile", prof, "-floors", fl}, &out, &errb); code != 0 {
		t.Fatalf("code %d err %s", code, errb.String())
	}
	b, _ := os.ReadFile(fl)
	if !strings.Contains(string(b), `"`+pkgA+`": 50.0`) {
		t.Errorf("floors not raised: %s", b)
	}
	// -out leaves the input alone
	fl2 := writeFile(t, dir, "g.json", `{"`+pkgA+`": 10.0}`)
	dst := filepath.Join(dir, "sub", "out.json")
	if code := run([]string{"ratchet", "-profile", prof, "-floors", fl2, "-out", dst}, &out, &errb); code != 0 {
		t.Fatalf("code %d err %s", code, errb.String())
	}
	if b, _ := os.ReadFile(fl2); !strings.Contains(string(b), "10.0") {
		t.Errorf("input changed: %s", b)
	}
	if b, _ := os.ReadFile(dst); !strings.Contains(string(b), "50.0") {
		t.Errorf("out not written: %s", b)
	}
}
