package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func TestRecipientsLabelSelf(t *testing.T) {
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	repo := gitwt.New(cfg.SyncDir)
	if l, _ := agecrypt.ListLabels(cfg.SyncDir); len(l) != 0 {
		t.Fatalf("setup: the first host starts unlabelled: %v", l)
	}
	var out, errb bytes.Buffer
	if c := recipientsCmd(&out, &errb, []string{"label", "self", "--host", "vm-1", "--config", cfg.Path()}); c != 0 {
		t.Fatalf("exit %d: %s", c, errb.String())
	}
	if l, _ := agecrypt.ListLabels(cfg.SyncDir); len(l) != 1 || l[0] != "vm-1" {
		t.Errorf("labels: %v", l)
	}
	if u, _ := agecrypt.Unreviewed(repo); u {
		t.Error("the label must be re-pinned")
	}
	if rc, _ := gitwt.New(origin).Run("cat-file", "blob", "main:.age-recipients"); !strings.Contains(rc, "# host:vm-1") {
		t.Errorf("the label must be pushed: %q", rc)
	}
	if c, err := config.Load(cfg.Path()); err != nil || c.HostLabel != "vm-1" {
		t.Errorf("hostLabel not saved: %q %v", c.HostLabel, err)
	}
	// a different label is refused once hostLabel is set
	errb.Reset()
	if c := recipientsCmd(&out, &errb, []string{"label", "self", "--host", "vm-2", "--config", cfg.Path()}); c == 0 {
		t.Error("relabelling must be refused")
	}
	// usage errors
	if c := recipientsCmd(&out, &errb, []string{"label", "self", "--config", cfg.Path()}); c != 2 {
		t.Errorf("missing --host: %d", c)
	}
	if c := recipientsCmd(&out, &errb, []string{"label", "self", "--host", "Bad_1", "--config", cfg.Path()}); c != 2 {
		t.Errorf("bad label: %d", c)
	}
}

func TestRecipientsLabelRecovery(t *testing.T) {
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	_, rec := newTestIdentity(t, "rec")
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms, Recipients: []string{rec}}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	_, stranger := newTestIdentity(t, "stranger")
	if c := recipientsCmd(&out, &errb, []string{"label", "recovery", stranger, "--config", cfg.Path()}); c == 0 {
		t.Fatal("a key that is not a recipient cannot be marked")
	}
	if c := recipientsCmd(&out, &errb, []string{"label", "recovery", rec, "--config", cfg.Path()}); c != 0 {
		t.Fatalf("exit %d: %s", c, errb.String())
	}
	if rc, _ := gitwt.New(origin).Run("cat-file", "blob", "main:.age-recipients"); !strings.Contains(rc, rec+" # recovery") {
		t.Errorf("the mark must be pushed: %q", rc)
	}
	if c, err := config.Load(cfg.Path()); err != nil || len(c.RecoveryRecipients) != 1 || c.RecoveryRecipients[0] != rec {
		t.Errorf("recoveryRecipients not saved: %v %v", c.RecoveryRecipients, err)
	}
	if u, _ := agecrypt.Unreviewed(gitwt.New(cfg.SyncDir)); u {
		t.Error("the mark must be re-pinned")
	}
}

func TestInitRecoveryRecipientFlag(t *testing.T) {
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	_, rec := newTestIdentity(t, "rec")
	cfg.RecoveryRecipients = []string{rec}
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms, Recipients: []string{rec + " # recovery"}}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	if c, err := config.Load(cfg.Path()); err != nil || len(c.RecoveryRecipients) != 1 {
		t.Errorf("recoveryRecipients not saved: %v %v", c.RecoveryRecipients, err)
	}
	if l, _ := agecrypt.ListRecipients(cfg.SyncDir); !strings.Contains(strings.Join(l, "\n"), rec+" # recovery") {
		t.Errorf("recipients: %v", l)
	}
}
