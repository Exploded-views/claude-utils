package agecrypt

import (
	"errors"
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

func TestParseRecipients_LabelIsPartOfWhatIsReviewed(t *testing.T) {
	_, rcpt := newIdentity(t, "id")
	plain, err := ParseRecipients([]byte(rcpt + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseRecipients([]byte(rcpt + " # host:vm-a\n"))
	if err != nil {
		t.Fatalf("a labelled age key must parse: %v", err)
	}
	b, _ := ParseRecipients([]byte(rcpt + " # host:vm-b\n"))
	if a.Fingerprint() == b.Fingerprint() || a.Fingerprint() == plain.Fingerprint() {
		t.Error("a label change must change the reviewed fingerprint")
	}
	again, _ := ParseRecipients([]byte(rcpt + "   # host:vm-a  \n"))
	if again.Fingerprint() != a.Fingerprint() {
		t.Error("whitespace around the comment is not a change")
	}
	// the same holds for ssh keys, whose other comment text is not reviewed
	ssh := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	s1, err := ParseRecipients([]byte(ssh + " me@laptop # host:vm-a\n"))
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := ParseRecipients([]byte(ssh + " other-comment # host:vm-a\n"))
	s3, _ := ParseRecipients([]byte(ssh + " me@laptop # host:vm-b\n"))
	if s1.Fingerprint() != s2.Fingerprint() || s1.Fingerprint() == s3.Fingerprint() {
		t.Error("ssh: free comment text is ignored, the host label is not")
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

// The label is bound to the key in the reviewed pin. A label swapped upstream
// is unreviewed; resolution goes through the pin, never the comment.
func TestGit_LabelResolvesThroughThePin(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	a.write(t, "n.md", "x\n")
	a.commit(t, "memsync: 1 files")
	_, rcptB := newIdentity(t, "idb")
	if err := AddRecipientLabelled(a.repo, rcptB, "vm-b"); err != nil {
		t.Fatal(err)
	}
	keys, err := PinnedKeysForLabel(a.repo, "vm-b")
	if err != nil || len(keys) != 1 || keys[0] != rcptB {
		t.Fatalf("pin should bind vm-b to its key: %v %v", keys, err)
	}
	// Upstream swaps the comments: vm-b's label now sits on the first host's key.
	lines, _ := ListRecipients(a.repo.Dir)
	own := strings.Fields(lines[0])[0]
	swapped := []string{own + " # host:vm-b", rcptB + " # host:vm-own"}
	if err := WriteRecipients(a.repo.Dir, swapped); err != nil {
		t.Fatal(err)
	}
	if u, _ := Unreviewed(a.repo); !u {
		t.Fatal("swapped labels must show as unreviewed")
	}
	keys, _ = PinnedKeysForLabel(a.repo, "vm-b")
	if len(keys) != 1 || keys[0] != rcptB {
		t.Errorf("the pin still says vm-b is %s, got %v", rcptB, keys)
	}
	before := a.revHead(t)
	if err := RemoveRecipientLabel(a.repo, "vm-b"); err == nil {
		t.Error("removal on unreviewed recipients must be refused")
	}
	if a.revHead(t) != before {
		t.Error("nothing may be committed")
	}
}

func TestGit_RemoveByLabelRemovesThePinnedKeyAndRefusesAmbiguity(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	a.write(t, "n.md", "x\n")
	a.commit(t, "memsync: 1 files")
	_, rcptB := newIdentity(t, "idb")
	_, rcptC := newIdentity(t, "idc")
	if err := AddRecipientLabelled(a.repo, rcptB, "vm-b"); err != nil {
		t.Fatal(err)
	}
	if err := AddRecipientLabelled(a.repo, rcptC, "vm-b"); err == nil {
		t.Error("a label already bound to another key must be refused at add")
	}
	if err := AddRecipientLabelled(a.repo, rcptC, "vm-c"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRecipientLabel(a.repo, "vm-b"); err != nil {
		t.Fatal(err)
	}
	rest, _ := ListRecipients(a.repo.Dir)
	joined := strings.Join(rest, "\n")
	if strings.Contains(joined, rcptB) || !strings.Contains(joined, rcptC) {
		t.Errorf("only vm-b's key may go: %v", rest)
	}
	// A duplicated label that a person accepted is still ambiguous.
	lines, _ := ListRecipients(a.repo.Dir)
	dup := append(lines, rcptB+" # host:vm-c")
	if err := WriteRecipients(a.repo.Dir, dup); err != nil {
		t.Fatal(err)
	}
	if err := AcceptRecipients(a.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := PinnedKeysForLabel(a.repo, "vm-c"); !errors.Is(err, ErrLabelAmbiguous) {
		t.Errorf("one label on two keys must be ambiguous: %v", err)
	}
	before := a.revHead(t)
	if err := RemoveRecipientLabel(a.repo, "vm-c"); !errors.Is(err, ErrLabelAmbiguous) {
		t.Errorf("want ErrLabelAmbiguous, got %v", err)
	}
	if a.revHead(t) != before {
		t.Error("an ambiguous removal must change nothing")
	}
}
