package agecrypt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDescribeLabel(t *testing.T) {
	for line, want := range map[string]string{
		"age1abc # host:alpha": "host:alpha",
		"age1abc # recovery":   "recovery",
		"age1abc":              "unlabelled",
		"age1abc # note":       "unlabelled",
	} {
		if got := DescribeLabel(line); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}

func TestOwnKeyInfo(t *testing.T) {
	a, b := pair(t)
	fp, desc, ok := OwnKeyInfo(a.repo)
	if !ok || desc != "unlabelled" || fp != GroupFingerprint(JoinFingerprint(a.rcpt)) {
		t.Fatalf("member: %q %q %v", fp, desc, ok)
	}
	if err := LabelSelf(a.repo, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, desc, ok := OwnKeyInfo(a.repo); !ok || desc != "host:alpha" {
		t.Errorf("labelled: %q %v", desc, ok)
	}
	if _, _, ok := OwnKeyInfo(b.repo); ok {
		t.Error("a host whose key is not a recipient has no own-key line")
	}
}

// A changed .gitattributes is reported with its source and is not a key.
func TestAttrsChangedAndRecipientsDiff(t *testing.T) {
	a, _ := pair(t)
	if _, changed := AttrsChanged(a.repo); changed {
		t.Fatal("nothing changed yet")
	}
	p := filepath.Join(a.repo.Dir, ".gitattributes")
	old, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(old, []byte("# edit\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch, changed := AttrsChanged(a.repo); !changed || !ch.Uncommitted {
		t.Fatalf("uncommitted edit: %+v %v", ch, changed)
	}
	a.commit(t, "memsync: attrs")
	ch, changed := AttrsChanged(a.repo)
	if !changed || ch.Uncommitted || ch.Commit == "" {
		t.Fatalf("committed edit: %+v %v", ch, changed)
	}
	added, removed, err := RecipientsDiff(a.repo)
	if err != nil || len(added) != 0 || len(removed) != 0 {
		t.Fatalf("a .gitattributes change is not a key: %v %v %v", added, removed, err)
	}
	_, other := newIdentity(t, "other")
	if err := WriteRecipients(a.repo.Dir, []string{other + " # host:zed"}); err != nil {
		t.Fatal(err)
	}
	added, removed, err = RecipientsDiff(a.repo)
	if err != nil || len(added) != 1 || len(removed) != 1 {
		t.Fatalf("added=%v removed=%v err=%v", added, removed, err)
	}
}
