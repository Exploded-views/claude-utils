package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

func TestTokenCmdFlagUsesShellStyleQuotes(t *testing.T) {
	var cfg config.Config
	applyTokenCmdFlag(&cfg, `secret-tool read "vault://My Vault/Item Name/field" --no-newline`)
	want := []string{"secret-tool", "read", "vault://My Vault/Item Name/field", "--no-newline"}
	if !reflect.DeepEqual(cfg.GitHubTokenCmd, want) || !cfg.TokenCmdFromFlag {
		t.Errorf("argv = %#v", cfg.GitHubTokenCmd)
	}
	var bad config.Config
	applyTokenCmdFlag(&bad, `secret-tool read "unterminated`)
	if len(bad.GitHubTokenCmd) != 0 {
		t.Errorf("an unbalanced quote must run nothing: %#v", bad.GitHubTokenCmd)
	}
}

func TestTokenCmdFromConfigIsAJSONArray(t *testing.T) {
	var c config.Config
	if err := jsonUnmarshal(`{"githubTokenCmd":["secret-tool","read","vault://My Vault/Item Name/field"]}`, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.GitHubTokenCmd) != 3 || c.GitHubTokenCmd[2] != "vault://My Vault/Item Name/field" {
		t.Errorf("argv = %#v", c.GitHubTokenCmd)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
