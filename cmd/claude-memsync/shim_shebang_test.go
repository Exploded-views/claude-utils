package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Every script a budget test writes onto PATH must be POSIX sh: the Docker CI
// image, the Pi and the Ubuntu runners have no zsh (B9 once failed there).
func TestBudgetShimsAreBinSh(t *testing.T) {
	files, err := filepath.Glob("budget_*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no budget test sources found: %v", err)
	}
	shebang := regexp.MustCompile(`#!/[^\s\\"` + "`" + `]*`)
	found := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range shebang.FindAllString(string(b), -1) {
			found++
			if m != "#!/bin/sh" {
				t.Errorf("%s writes a %q script; budget shims must start with #!/bin/sh", f, m)
			}
		}
	}
	if found == 0 {
		t.Error("no shim scripts found in the budget tests: the guard is looking in the wrong place")
	}
}
