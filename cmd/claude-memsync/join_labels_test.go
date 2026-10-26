package main

import (
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
)

func lineWith(out, sub string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// join status on the new host lists every key of the pending set with its
// label, so the person compares labelled lines, not bare fingerprints.
func TestJoinCLI_StatusShowsEachKeysLabel(t *testing.T) {
	h := twoHosts(t)
	keyA, _ := agecrypt.IdentityRecipient(agecrypt.IdentityPath(h.ra))
	if err := agecrypt.LabelRecovery(h.ra, keyA); err != nil {
		t.Fatal(err)
	}
	if err := h.ra.Push("main"); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if code, _, e := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--allow-unlabelled", "--fingerprint", fpOf(out)); code != 0 {
		t.Fatal(e)
	}
	if _, err := h.rb.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	_, st, _ := runJoinCmd("status", "--config", h.b.Path())
	keyB, _ := agecrypt.IdentityRecipient(h.idB)
	own := lineWith(st, agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(keyB)))
	if !strings.Contains(own, "host:new-host") || !strings.Contains(own, "this host") {
		t.Errorf("this host's line must show its label and say it is this host: %q\n%s", own, st)
	}
	member := lineWith(st, agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(keyA)))
	if !strings.Contains(member, "recovery") || strings.Contains(member, "this host") {
		t.Errorf("the recovery key's line: %q\n%s", member, st)
	}
}

func TestJoinCLI_StatusMarksAnUnlabelledKey(t *testing.T) {
	h := twoHosts(t)
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--allow-unlabelled", "--fingerprint", fpOf(out))
	if _, err := h.rb.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	_, st, _ := runJoinCmd("status", "--config", h.b.Path())
	keyA, _ := agecrypt.IdentityRecipient(agecrypt.IdentityPath(h.ra))
	if l := lineWith(st, agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(keyA))); !strings.Contains(l, "unlabelled") {
		t.Errorf("an unlabelled key must say so: %q\n%s", l, st)
	}
}

// join list shows the label each request asks for, and whether the key is
// this host's own.
func TestJoinCLI_ListShowsLabelAndWhetherItIsThisHost(t *testing.T) {
	h := twoHosts(t)
	runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	_, onA, _ := runJoinCmd("list", "--config", h.a.Path())
	l := lineWith(onA, "new-host")
	if !strings.Contains(l, "host:new-host") || strings.Contains(l, "this host") {
		t.Errorf("member's list: %q", l)
	}
	_, onB, _ := runJoinCmd("list", "--config", h.b.Path())
	if l := lineWith(onB, "new-host"); !strings.Contains(l, "host:new-host") || !strings.Contains(l, "this host") {
		t.Errorf("the requesting host's own list: %q", l)
	}
}

// After an accept the member prints its OWN key and label: the one known line
// the new host compares against its `join status`.
func TestJoinCLI_AcceptPrintsTheAcceptingHostsKeyAndLabel(t *testing.T) {
	h := twoHosts(t)
	if err := agecrypt.LabelSelf(h.ra, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := h.ra.Push("main"); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	code, aout, e := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--fingerprint", fpOf(out))
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	keyA, _ := agecrypt.IdentityRecipient(agecrypt.IdentityPath(h.ra))
	want := "accepted by host:alpha, key fingerprint " + agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(keyA)) + "; on the new host, `join status` must show this key"
	if !strings.Contains(aout, want) {
		t.Errorf("missing %q in:\n%s", want, aout)
	}
}

func TestJoinCLI_AcceptByAnUnlabelledHostSaysSo(t *testing.T) {
	h := twoHosts(t)
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	_, aout, _ := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--allow-unlabelled", "--fingerprint", fpOf(out))
	keyA, _ := agecrypt.IdentityRecipient(agecrypt.IdentityPath(h.ra))
	if !strings.Contains(aout, "accepted by this host (unlabelled), key fingerprint "+agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(keyA))) {
		t.Errorf("unlabelled accepter:\n%s", aout)
	}
}
