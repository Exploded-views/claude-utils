package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/related"
)

// ageClone writes an exported read clone of an encrypted repo: the recipients
// file in the clear and every note as ciphertext, as the repo stores them.
func ageClone(t *testing.T, rcpt string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, agecrypt.RecipientsFile), []byte(rcpt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := agecrypt.ParseRecipients([]byte(rcpt + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		ct, err := agecrypt.Encrypt([]byte(body), r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), ct, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newIdentity(t *testing.T) (path, rcpt string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "id")
	rcpt, err := agecrypt.GenerateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, rcpt
}

func TestAgeOpener_DecryptsCloneForARecipientHostOnly(t *testing.T) {
	idPath, rcpt := newIdentity(t)
	dir := ageClone(t, rcpt, map[string]string{"MEMORY.md": "# SECRET-HEADING\n", "n.md": "x"})

	mine := ageOpener{IdentityPath: idPath}
	if !mine.Readable(dir) {
		t.Fatal("a recipient host must read the clone")
	}
	ix, err := related.BuildIndex(dir, mine)
	if err != nil || len(ix.Headings) != 1 || !strings.Contains(ix.Headings[0], "SECRET-HEADING") || ix.Notes != 1 {
		t.Fatalf("index through the decrypting opener: %+v %v", ix, err)
	}

	otherPath, _ := newIdentity(t)
	if (ageOpener{IdentityPath: otherPath}).Readable(dir) {
		t.Error("a host whose key is not a recipient must not read it")
	}
	if (ageOpener{IdentityPath: filepath.Join(t.TempDir(), "none")}).Readable(dir) {
		t.Error("a host with no identity must not read it")
	}
	// A plaintext clone needs no key at all.
	plain := t.TempDir()
	os.WriteFile(filepath.Join(plain, "MEMORY.md"), []byte("# plain\n"), 0o600)
	o := ageOpener{IdentityPath: filepath.Join(t.TempDir(), "none")}
	if !o.Readable(plain) {
		t.Error("plaintext clone must stay readable")
	}
	if b, err := o.ReadFile(filepath.Join(plain, "MEMORY.md")); err != nil || string(b) != "# plain\n" {
		t.Errorf("plain read: %q %v", b, err)
	}
}

func TestRelatedOpener_DefaultIsTheDecryptingOne(t *testing.T) {
	if _, ok := relatedOpener.(ageOpener); !ok {
		t.Fatalf("relatedOpener = %T, want ageOpener", relatedOpener)
	}
}

// archive list/search reads archived notes through the repo's decrypting
// reader: a clone holding ciphertext (keyless checkout, or files not yet
// smudged) is still searchable on a host that holds the key.
func TestArchiveReader_DecryptsCiphertextFiles(t *testing.T) {
	initEnv(t)
	idPath, rcpt := newIdentity(t)
	dir := t.TempDir()
	repo := gitwt.New(dir)
	if err := repo.Init("main"); err != nil {
		t.Fatal(err)
	}
	if err := repo.ConfigSet(agecrypt.IdentityConfigKey, filepath.ToSlash(idPath)); err != nil {
		t.Fatal(err)
	}
	r, _ := agecrypt.ParseRecipients([]byte(rcpt + "\n"))
	ct, _ := agecrypt.Encrypt([]byte("the needle is here\n"), r)
	p := filepath.Join(dir, "archive", "2026-10-25", "p1")
	os.MkdirAll(p, 0o700)
	os.WriteFile(filepath.Join(p, "n.0123abcd.md"), ct, 0o600)
	os.WriteFile(filepath.Join(dir, agecrypt.RecipientsFile), []byte(rcpt+"\n"), 0o600)

	restore := useArchiveReader(dir)
	defer restore()
	hits, err := archive.Search(dir, "needle")
	if err != nil || len(hits) != 1 {
		t.Fatalf("search through the decrypting reader: %v %v", hits, err)
	}
	restore()
	if hits, _ := archive.Search(dir, "needle"); len(hits) != 0 {
		t.Error("the reader must be restored after the command")
	}
}

// With a token configured a relation's endpoints must read back as private; a
// public (or unverifiable) repo cannot be related. Without a token the name
// check is all there is.
func TestRelate_VisibilityUsesGHGuardWhenATokenIsConfigured(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		pub := strings.HasSuffix(r.URL.Path, "/mem-project-b")
		vis := "private"
		if pub {
			vis = "public"
		}
		_ = json.NewEncoder(w).Encode(repoJSON{Name: "x", Private: tb(!pub), Visibility: vis})
	}))
	defer srv.Close()
	skipAPIHostCheck = true
	defer func() { skipAPIHostCheck = false }()
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")

	// No token: names only, nothing asked of the API.
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatalf("without a token the name check decides: %d %s", code, errs)
	}
	if len(asked) != 0 {
		t.Fatalf("no token, yet the API was called: %v", asked)
	}
	if code, _, errs := run(func(o, e *bytes.Buffer) int { return unrelateCmd(o, e, []string{"mem-project-a", "mem-project-b"}) }); code != 0 {
		t.Fatalf("unrelate: %d %s", code, errs)
	}

	cfg := h.cfgs["a"]
	cfg.GitHubAPIURL, cfg.GitHubHost, cfg.GitHubOrg = srv.URL, "github.com", "Org"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "fake-token")
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code == 0 || !strings.Contains(errs, "mem-project-b") {
		t.Fatalf("a public endpoint must be refused: %d %q", code, errs)
	}
	if code, _, errs := relate("mem-project-a", "mem-project-c", "--read"); code != 0 {
		t.Fatalf("private endpoints are fine: %d %s", code, errs)
	}
	if len(asked) == 0 {
		t.Error("the API was never asked")
	}
}

// A related clone encrypted to the key of one of this host's own encrypted sync
// repos is readable although that key is not the host's default identity.
func TestRelatedOpener_TriesEachRoutedReposIdentity(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	idPath, rcpt := newIdentity(t)
	repo := gitwt.New(h.cfgs["a"].SyncDir)
	if err := agecrypt.Configure(repo, agecrypt.Opts{MemsyncExe: "/x/ms", MemmergeExe: "/x/mm", IdentityPath: idPath}); err != nil {
		t.Fatal(err)
	}
	dir := ageClone(t, rcpt, map[string]string{"MEMORY.md": "# ROUTED-KEY-HEADING\n"})

	if (ageOpener{}).Readable(dir) {
		t.Fatal("setup: the default identity must not read this clone")
	}
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	o := rc.opener()
	if !o.Readable(dir) {
		t.Fatal("the routed repo's identity should make the clone readable")
	}
	ix, err := related.BuildIndex(dir, o)
	if err != nil || len(ix.Headings) != 1 || !strings.Contains(ix.Headings[0], "ROUTED-KEY-HEADING") {
		t.Fatalf("index: %+v %v", ix, err)
	}
	// A clone for nobody on this host stays unreadable.
	_, other := newIdentity(t)
	if o.Readable(ageClone(t, other, map[string]string{"MEMORY.md": "# x\n"})) {
		t.Error("a clone encrypted to a stranger must stay unreadable")
	}
}

// A recipients line carries a "# host:<label>" or "# recovery" label; the key
// comparison must look at the key only.
func TestAgeOpener_ReadableWithLabelledRecipientLines(t *testing.T) {
	hp, hr := newPQKey(t, "h")
	other, _ := newPQKey(t, "o")
	dir := ageClone(t, hr+" # host:vm-1", map[string]string{"n.md": "x"})
	if !(ageOpener{IdentityPath: hp}).Readable(dir) {
		t.Error("a labelled hybrid recipient line must match its own key")
	}
	if (ageOpener{IdentityPath: other}).Readable(dir) {
		t.Error("another hybrid key must not read the clone")
	}
	ip, cr := newIdentity(t)
	dir2 := ageClone(t, cr+" # recovery", map[string]string{"n.md": "x"})
	if !(ageOpener{IdentityPath: ip}).Readable(dir2) {
		t.Error("a labelled classic recipient line must match its own key")
	}
}
