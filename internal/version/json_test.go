package version

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestJSON_OneLineWithTheStableKeys(t *testing.T) {
	s := JSON("claude-memsync")
	if strings.ContainsAny(s, "\n") {
		t.Fatalf("not one line: %q", s)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not a JSON object of strings: %v: %q", err, s)
	}
	if len(m) != 6 {
		t.Errorf("keys = %v, want exactly name, version, commit, go, os, arch", m)
	}
	want := map[string]string{"name": "claude-memsync", "version": String(), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if _, ok := m["commit"]; !ok {
		t.Error("commit key missing (it is empty when the build has no VCS info)")
	}
	if i := strings.Index(s, `"name"`); i != 1 {
		t.Errorf("key order changed: %q", s)
	}
}
