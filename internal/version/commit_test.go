package version

import (
	"encoding/json"
	"testing"
)

func TestJSON_CommitFromLinkTimeStamp(t *testing.T) {
	old := Commit
	Commit = "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() { Commit = old })
	var m map[string]string
	if err := json.Unmarshal([]byte(JSON("claude-memsync")), &m); err != nil {
		t.Fatal(err)
	}
	if m["commit"] != Commit {
		t.Errorf("commit = %q, want the stamped %q", m["commit"], Commit)
	}
}
