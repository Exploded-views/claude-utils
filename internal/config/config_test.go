package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ProjectKey(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := Load(write(`{"remoteUrl":"x"}`))
	if err != nil || c.ProjectKey != ProjectKeyPath {
		t.Errorf("default = %q, %v; want %q", c.ProjectKey, err, ProjectKeyPath)
	}
	c, err = Load(write(`{"projectKey":"remote"}`))
	if err != nil || c.ProjectKey != ProjectKeyRemote {
		t.Errorf("got %q, %v", c.ProjectKey, err)
	}
	if _, err := Load(write(`{"projectKey":"bogus"}`)); err == nil {
		t.Error("an unknown projectKey must be rejected, not silently treated as path")
	}
}
