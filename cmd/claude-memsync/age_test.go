package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func TestBootstrapEncrypt_NewRemoteStoresOnlyCiphertext(t *testing.T) {
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(enc.IdentityPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("identity should be generated at 0600: %v %v", fi, err)
	}
	repo := gitwt.New(cfg.SyncDir)
	if !agecrypt.Enabled(repo) {
		t.Fatal("encryption should be on")
	}
	o := gitwt.New(origin)
	files := originFiles(t, origin)
	for _, f := range []string{"clients.md", "ok.md", ".age-recipients", ".gitattributes"} {
		if !strings.Contains(files, f) {
			t.Errorf("origin lacks %s:\n%s", f, files)
		}
	}
	for _, f := range []string{"projects/p1/memory/ok.md", "projects/p1/memory/clients.md"} {
		b, _ := o.Run("cat-file", "blob", "main:"+f)
		if !agecrypt.IsCiphertext([]byte(b)) || strings.Contains(b, "harmless") || strings.Contains(b, "Nightjar") {
			t.Errorf("%s must be ciphertext in origin", f)
		}
	}
	rc, _ := o.Run("cat-file", "blob", "main:.age-recipients")
	if strings.Contains(rc, "SECRET") || !strings.Contains(rc, "age1") {
		t.Errorf("recipients should hold public keys only: %q", rc)
	}
	idData, _ := os.ReadFile(enc.IdentityPath)
	if strings.Contains(rc, strings.TrimSpace(strings.Split(string(idData), "AGE-SECRET-KEY")[1])) {
		t.Error("secret leaked into the recipients file")
	}
	if msg, _ := o.Run("log", "-1", "--format=%s"); strings.TrimSpace(msg) != "memsync: 3 files" && !strings.HasPrefix(strings.TrimSpace(msg), "memsync: ") {
		t.Errorf("commit message should be neutral, got %q", msg)
	}
	if d, _ := repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
		t.Errorf("dirty after init: %s", d)
	}
}

func TestBootstrapEncrypt_RefusesUnencryptedRemote(t *testing.T) {
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	if err := bootstrap(cfg, false, nil); err != nil { // plain init pushes plaintext
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.SyncDir = filepath.Join(t.TempDir(), "second")
	cfg2.MergeDriverPath = mm
	err := bootstrapWith(cfg2, false, nil, &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "id"), MemsyncExe: ms})
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestBootstrapEncrypt_SecondHostIsKeylessUntilAdded(t *testing.T) {
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	first := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "id1"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, first); err != nil {
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.SyncDir = filepath.Join(t.TempDir(), "second")
	cfg2.ClaudeProjectsDir = filepath.Join(t.TempDir(), "claude2")
	second := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "id2"), MemsyncExe: ms}
	if err := bootstrapWith(cfg2, false, nil, second); err != nil {
		t.Fatal(err)
	}
	r2 := gitwt.New(cfg2.SyncDir)
	if err := agecrypt.CheckKeyed(r2); err == nil {
		t.Fatal("second host should be keyless")
	}
	if _, err := os.Stat(cfg2.ClaudeProjectsDir); err == nil {
		entries, _ := os.ReadDir(cfg2.ClaudeProjectsDir)
		if len(entries) > 0 {
			t.Error("ciphertext must not be reconciled into the Claude tree")
		}
	}
	if lines := encryptionLines(cfg2); len(lines) == 0 || !strings.Contains(lines[0], "KEYLESS") {
		t.Errorf("status should report keyless: %v", lines)
	}
	// Admit it from the first host.
	rcpt2, _ := agecrypt.IdentityRecipient(second.IdentityPath)
	if err := agecrypt.AddRecipient(gitwt.New(cfg.SyncDir), rcpt2); err != nil {
		t.Fatal(err)
	}
	if err := gitwt.New(cfg.SyncDir).Push("main"); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if lines := encryptionLines(cfg2); !strings.Contains(strings.Join(lines, "\n"), "UNREVIEWED") {
		t.Errorf("status should flag the unreviewed recipients: %v", lines)
	}
	if err := agecrypt.AcceptRecipients(r2); err != nil {
		t.Fatal(err)
	}
	if lines := encryptionLines(cfg2); !strings.Contains(lines[0], "can decrypt") {
		t.Errorf("status after admission: %v", lines)
	}
	_ = origin
}
