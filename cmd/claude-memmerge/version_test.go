package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/version"
)

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runVersion(&out, &errb, nil); code != 0 || out.String() != version.Strings("claude-memmerge")+"\n" || errb.Len() != 0 {
		t.Errorf("version: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := runVersion(&out, &errb, []string{"--json"}); code != 0 || out.String() != version.JSON("claude-memmerge")+"\n" {
		t.Errorf("version --json: %d %q %q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), `"name":"claude-memmerge"`) {
		t.Errorf("wrong name in %q", out.String())
	}
	for _, bad := range [][]string{{"x"}, {"--json", "x"}, {"--json=1"}, {"-json"}} {
		out.Reset()
		errb.Reset()
		if code := runVersion(&out, &errb, bad); code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), "unexpected argument") {
			t.Errorf("version %v: %d %q %q", bad, code, out.String(), errb.String())
		}
	}
}
