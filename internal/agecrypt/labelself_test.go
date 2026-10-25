package agecrypt

import (
	"strings"
	"testing"
)

// request pulls b's join request into a and returns its fingerprint.
func request(t *testing.T, a, b *host, label string) string {
	t.Helper()
	if _, _, err := RequestJoin(b.repo, label, joinNow); err != nil {
		t.Fatal(err)
	}
	b.repo.Run("push", "origin", "main")
	if _, err := a.repo.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	list, err := ListJoins(a.repo)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	return list[0].Join.Fingerprint
}

func TestLabelSelfLabelsOwnLineAndRepins(t *testing.T) {
	a, _ := pair(t)
	if err := LabelSelf(a.repo, "bad_label"); err == nil {
		t.Error("an invalid label must be refused")
	}
	if err := LabelSelf(a.repo, "host-a"); err != nil {
		t.Fatal(err)
	}
	if labels, _ := ListLabels(a.repo.Dir); len(labels) != 1 || labels[0] != "host-a" {
		t.Fatalf("labels: %v", labels)
	}
	if u, _ := Unreviewed(a.repo); u {
		t.Error("the new label must be pinned")
	}
	if k, err := PinnedKeysForLabel(a.repo, "host-a"); err != nil || len(k) != 1 {
		t.Errorf("pin: %v %v", k, err)
	}
	head := a.revHead(t)
	if err := LabelSelf(a.repo, "host-a"); err != nil || a.revHead(t) != head {
		t.Errorf("labelling twice must be a no-op: %v", err)
	}
	if err := LabelSelf(a.repo, "other"); err == nil || !strings.Contains(err.Error(), "already labelled") {
		t.Errorf("relabelling must be refused: %v", err)
	}
}

func TestLabelSelfRefusals(t *testing.T) {
	a, _ := pair(t)
	_, other := newIdentity(t, "other")
	if err := AddRecipientLabelled(a.repo, other, "taken"); err != nil {
		t.Fatal(err)
	}
	if err := LabelSelf(a.repo, "taken"); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Errorf("a label another key holds must be refused: %v", err)
	}
}

func TestAcceptRefusesOwnLabel(t *testing.T) {
	a, b := pair(t)
	fp := request(t, a, b, "host-b")
	head := a.revHead(t)
	err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{OwnLabel: "host-b", AllowUnlabelled: true})
	if err == nil || !strings.Contains(err.Error(), "own label") {
		t.Fatalf("a requester may not take this host's label: %v", err)
	}
	if a.revHead(t) != head {
		t.Error("a refused accept must not commit")
	}
}

func TestAcceptRefusesLabelBoundInPin(t *testing.T) {
	a, b := pair(t)
	if err := LabelSelf(a.repo, "host-b"); err != nil { // a itself holds the label
		t.Fatal(err)
	}
	fp := request(t, a, b, "host-b")
	err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{AllowUnlabelled: true})
	if err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("a label bound in the pin must be refused: %v", err)
	}
}

func TestAcceptRefusesUnlabelledRecipientUnlessAllowed(t *testing.T) {
	a, b := pair(t)
	fp := request(t, a, b, "host-b")
	head := a.revHead(t)
	err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{})
	if err == nil || !strings.Contains(err.Error(), "recipients label self") || !strings.Contains(err.Error(), "--allow-unlabelled") {
		t.Fatalf("an unlabelled recipient must block the accept with a hint: %v", err)
	}
	if a.revHead(t) != head {
		t.Fatal("a refused accept must not commit")
	}
	if err := LabelSelf(a.repo, "host-a"); err != nil {
		t.Fatal(err)
	}
	if err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{}); err != nil {
		t.Fatalf("with every recipient labelled the accept goes through: %v", err)
	}
}

func TestAcceptExemptsOnlyTheConfiguredRecoveryKey(t *testing.T) {
	a, b := pair(t)
	_, rec := newIdentity(t, "rec")
	_, stranger := newIdentity(t, "stranger")
	if err := AddRecipient(a.repo, rec); err != nil {
		t.Fatal(err)
	}
	if err := LabelRecovery(a.repo, rec); err != nil {
		t.Fatal(err)
	}
	if err := LabelRecovery(a.repo, rec); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := LabelRecovery(a.repo, stranger); err == nil {
		t.Error("a key that is not a recipient cannot be marked")
	}
	if err := LabelSelf(a.repo, "host-a"); err != nil {
		t.Fatal(err)
	}
	if err := LabelRecovery(a.repo, a.rcpt); err == nil {
		t.Error("a host's labelled key is not a recovery key")
	}
	fp := request(t, a, b, "host-b")
	// not configured as recovery: the comment gives no exemption
	if err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{}); err == nil {
		t.Fatal("a # recovery line whose key is not configured must block the accept")
	}
	if err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{RecoveryKeys: []string{stranger}}); err == nil {
		t.Fatal("configuring a different key must not exempt this one")
	}
	if err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{RecoveryKeys: []string{rec}}); err != nil {
		t.Fatalf("the configured recovery key passes: %v", err)
	}
}

// A "# recovery" comment on a stranger's key, added by someone with write
// access to the remote, must not earn an exemption.
func TestAcceptFakeRecoveryCommentOnStrangerBlocks(t *testing.T) {
	a, b := pair(t)
	_, rec := newIdentity(t, "rec")
	_, stranger := newIdentity(t, "stranger")
	if err := LabelSelf(a.repo, "host-a"); err != nil {
		t.Fatal(err)
	}
	if err := AddRecipient(a.repo, rec+" # recovery"); err != nil {
		t.Fatal(err)
	}
	if err := AddRecipient(a.repo, stranger+" # recovery"); err != nil {
		t.Fatal(err)
	}
	fp := request(t, a, b, "host-b")
	head := a.revHead(t)
	err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{RecoveryKeys: []string{rec}})
	if err == nil || !strings.Contains(err.Error(), "no host label") {
		t.Fatalf("the stranger's fake recovery line must block: %v", err)
	}
	if a.revHead(t) != head {
		t.Error("a refused accept must not commit")
	}
}
