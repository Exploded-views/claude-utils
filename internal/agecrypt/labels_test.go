package agecrypt

import (
	"reflect"
	"strings"
	"testing"
)

func TestLabelOf(t *testing.T) {
	cases := map[string]string{
		"age1abc # host:vm-3":                      "vm-3",
		"ssh-ed25519 AAAA comment # host:laptop-2": "laptop-2",
		"age1abc":                        "",
		"age1abc # recovery key":         "",
		"ssh-ed25519 AAAA host:laptop-2": "", // not a trailing comment
		"age1abc # host:Bad_Label":       "",
		"age1abc #host:vm-3":             "vm-3",
	}
	for in, want := range cases {
		if got := LabelOf(in); got != want {
			t.Errorf("LabelOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRecipients_AgeKeyWithTrailingComment(t *testing.T) {
	_, rcpt := newIdentity(t, "id")
	a, err := ParseRecipients([]byte(rcpt + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseRecipients([]byte(rcpt + " # host:vm-3\n"))
	if err != nil {
		t.Fatalf("a labelled age key must parse: %v", err)
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("a label must not change the recipient set's fingerprint (it would trip the review pin)")
	}
}

func TestGit_LabelledRecipients(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	a.write(t, "n.md", "before\n")
	a.commit(t, "memsync: 1 files")
	_, rcptB := newIdentity(t, "idb")
	_, rcptC := newIdentity(t, "idc")
	if err := AddRecipientLabelled(a.repo, rcptB, "vm-b"); err != nil {
		t.Fatal(err)
	}
	if err := AddRecipientLabelled(a.repo, rcptC, "vm-c"); err != nil {
		t.Fatal(err)
	}
	if err := AddRecipientLabelled(a.repo, rcptC, "Bad Label"); err == nil {
		t.Error("an invalid label must be refused")
	}
	got, err := ListLabels(a.repo.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"vm-b", "vm-c"}) {
		t.Errorf("labels = %v", got)
	}
	lines, _ := ListRecipients(a.repo.Dir)
	if len(lines) != 3 || LabelOf(lines[0]) != "" {
		t.Errorf("unlabelled line must stay unlabelled: %v", lines)
	}
	if err := RemoveRecipientLabel(a.repo, "vm-zzz"); err == nil {
		t.Error("an unknown label must be an error")
	}
	if err := RemoveRecipientLabel(a.repo, "vm-b"); err != nil {
		t.Fatal(err)
	}
	rest, _ := ListRecipients(a.repo.Dir)
	if strings.Contains(strings.Join(rest, "\n"), rcptB) || len(rest) != 2 {
		t.Errorf("only vm-b's line should be gone: %v", rest)
	}
	if d := a.dirty(t); d != "" {
		t.Errorf("dirty: %s", d)
	}
}
