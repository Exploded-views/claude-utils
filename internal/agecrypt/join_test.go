package agecrypt

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var joinNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func goodJoin(t *testing.T, label string) (Join, string) {
	t.Helper()
	_, rcpt := newIdentity(t, "j")
	j := Join{Label: label, Recipient: rcpt, Requested: joinNow}
	j.Fingerprint = JoinFingerprint(rcpt)
	return j, j.Format()
}

func TestJoinFormatParseRoundTrip(t *testing.T) {
	j, text := goodJoin(t, "host-1")
	if !strings.HasPrefix(text, "label: host-1\nrecipient: age1") || !strings.Contains(text, "\nrequested: 2026-10-25T12:00:00Z\n") {
		t.Fatalf("format: %q", text)
	}
	got, err := ParseJoin([]byte(text))
	if err != nil || got.Label != "host-1" || got.Recipient != j.Recipient || got.Fingerprint != j.Fingerprint || !got.Requested.Equal(joinNow) {
		t.Fatalf("parse: %+v %v", got, err)
	}
	if len(j.Fingerprint) != 32 {
		t.Errorf("fingerprint: %q", j.Fingerprint)
	}
}

func TestParseJoinStrict(t *testing.T) {
	j, good := goodJoin(t, "host-1")
	lines := strings.Split(strings.TrimSuffix(good, "\n"), "\n")
	_, sshKey := func() (string, string) {
		return "", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHcbV3DnP0R7ZqzuZXlzJ4W0eKgQh8WZ1xX3nYF8mO6c"
	}()
	cases := map[string]string{
		"extra line":      good + "extra: stuff\n",
		"extra trailing":  good + "\n",
		"no trailing nl":  strings.TrimSuffix(good, "\n"),
		"crlf":            strings.ReplaceAll(good, "\n", "\r\n"),
		"comment":         "# hi\n" + good,
		"reordered":       strings.Join([]string{lines[1], lines[0], lines[2], lines[3]}, "\n") + "\n",
		"upper label":     strings.Replace(good, "host-1", "Host-1", 1),
		"long label":      strings.Replace(good, "host-1", strings.Repeat("a", 33), 1),
		"leading dash":    strings.Replace(good, "host-1", "-host", 1),
		"ssh recipient":   strings.Join([]string{lines[0], "recipient: " + sshKey, lines[2], lines[3]}, "\n") + "\n",
		"bad fingerprint": strings.Replace(good, j.Fingerprint, strings.Repeat("0", len(j.Fingerprint)), 1),
		"non-utc time":    strings.Replace(good, "12:00:00Z", "12:00:00+02:00", 1),
		"garbage time":    strings.Replace(good, "2026-10-25T12:00:00Z", "yesterday", 1),
		"two recipients":  strings.Replace(good, j.Recipient, j.Recipient+" "+j.Recipient, 1),
		"trailing space":  strings.Replace(good, "host-1\n", "host-1 \n", 1),
		"empty":           "",
		"huge":            good + strings.Repeat("x", 5000),
	}
	for name, c := range cases {
		if _, err := ParseJoin([]byte(c)); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestIsJoinPathAndValidBlob(t *testing.T) {
	_, good := goodJoin(t, "host-1")
	for p, want := range map[string]bool{
		"joins/host-1.txt":     true,
		"joins/Host.txt":       false,
		"joins/a/b.txt":        false,
		"joins/host-1.md":      false,
		"joins/.txt":           false,
		"xjoins/host-1.txt":    false,
		"projects/joins/a.txt": false,
		"joins/host-1.txt/x":   false,
	} {
		if IsJoinPath(p) != want {
			t.Errorf("IsJoinPath(%q) = %v", p, !want)
		}
	}
	if !ValidJoinBlob("joins/host-1.txt", []byte(good)) {
		t.Error("good blob refused")
	}
	if ValidJoinBlob("joins/other.txt", []byte(good)) {
		t.Error("label must match the file name")
	}
	if ValidJoinBlob("notes/host-1.txt", []byte(good)) {
		t.Error("path outside joins/")
	}
}

// pair builds member a and keyless requester b over one remote.
func pair(t *testing.T) (a, b *host) {
	t.Helper()
	gitEnv(t)
	remote := bareRemote(t)
	a = newHost(t, remote, true)
	a.write(t, "MEMORY.md", "- first\n")
	a.commit(t, "memsync: 1 files")
	if _, err := a.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	b = newHost(t, remote, false)
	if CheckKeyed(b.repo) == nil {
		t.Fatal("b must be keyless")
	}
	return a, b
}

func headTreePaths(t *testing.T, h *host, rev string) string {
	t.Helper()
	out, err := h.repo.Run("diff", "--name-status", "--no-renames", rev+"~1", rev)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func TestRequestJoinCommitsOnlyThePathAndPushes(t *testing.T) {
	a, b := pair(t)
	// A secret staged in the real index on the keyless host must stay staged.
	b.write(t, "secret.txt", "TOPSECRET\n")
	if _, err := b.repo.Run("add", "secret.txt"); err != nil {
		t.Fatal(err)
	}
	j, st, err := RequestJoin(b.repo, "host-b", joinNow)
	if err != nil || st != JoinRequested {
		t.Fatalf("request: %v %v", st, err)
	}
	if j.Recipient != b.rcpt {
		t.Error("recipient must be this host's public key")
	}
	if got := headTreePaths(t, b, "HEAD"); got != "A\tjoins/host-b.txt" {
		t.Fatalf("commit must hold only the join file, got %q", got)
	}
	if staged, _ := b.repo.Run("diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "secret.txt" {
		t.Errorf("staged secret must stay staged and uncommitted, index diff: %q", staged)
	}
	if st := b.dirty(t); strings.Contains(st, "joins") {
		t.Errorf("join file should be tracked and clean: %q", st)
	}
	if err := PushGuard(b.repo, "main"); err != nil {
		t.Fatalf("guard must accept a valid join request: %v", err)
	}
	if _, err := b.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if !IsJoinPlaintextInRemote(t, a) {
		t.Fatal("join file must be plaintext on the remote")
	}
}

// IsJoinPlaintextInRemote fetches into a and reads the blob raw.
func IsJoinPlaintextInRemote(t *testing.T, a *host) bool {
	t.Helper()
	if _, err := a.repo.Run("fetch", "-q", "origin"); err != nil {
		t.Fatal(err)
	}
	out, err := a.repo.Run("cat-file", "blob", "refs/remotes/origin/main:joins/host-b.txt")
	return err == nil && strings.HasPrefix(out, "label: host-b\n")
}

func TestRequestJoinIdempotent(t *testing.T) {
	_, b := pair(t)
	if _, st, err := RequestJoin(b.repo, "host-b", joinNow); err != nil || st != JoinRequested {
		t.Fatal(st, err)
	}
	before := b.revHead(t)
	_, st, err := RequestJoin(b.repo, "host-b", joinNow.Add(time.Hour))
	if err != nil || st != JoinExists {
		t.Fatalf("second request: %v %v", st, err)
	}
	if b.revHead(t) != before {
		t.Error("an idempotent request must not commit again")
	}
	// Same key under another label is the same request.
	if _, st, _ := RequestJoin(b.repo, "other-name", joinNow); st != JoinExists {
		t.Errorf("same key, other label: %v", st)
	}
}

func TestRequestJoinLabelTakenByAnotherKey(t *testing.T) {
	a, b := pair(t)
	// a plants a request for the label under a different key
	_, other := newIdentity(t, "o")
	j := Join{Label: "host-b", Recipient: other, Requested: joinNow}
	j.Fingerprint = JoinFingerprint(other)
	a.write(t, "joins/host-b.txt", j.Format())
	a.commit(t, "x")
	a.repo.Run("push", "origin", "main")
	if err := SyncFromOrigin(b.repo); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RequestJoin(b.repo, "host-b", joinNow); err == nil {
		t.Fatal("a label held by another key must be refused")
	}
}

func TestRequestJoinRefusals(t *testing.T) {
	a, b := pair(t)
	if _, _, err := RequestJoin(b.repo, "Bad_Label", joinNow); err == nil {
		t.Error("bad label")
	}
	if _, st, err := RequestJoin(a.repo, "host-a", joinNow); err != nil || st != JoinAlreadyMember {
		t.Errorf("a member has nothing to request: %v %v", st, err)
	}
}

func TestJoinAcceptEndToEnd(t *testing.T) {
	a, b := pair(t)
	if _, _, err := RequestJoin(b.repo, "host-b", joinNow); err != nil {
		t.Fatal(err)
	}
	b.repo.Run("push", "origin", "main")
	if _, err := a.repo.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	list, err := ListJoins(a.repo)
	if err != nil || len(list) != 1 || !list[0].Valid || list[0].Join.Label != "host-b" {
		t.Fatalf("list: %+v %v", list, err)
	}
	fp := list[0].Join.Fingerprint

	// No fingerprint, wrong fingerprint: refused, nothing changes.
	head := a.revHead(t)
	for _, bad := range []string{"", "deadbeef", strings.ToUpper(fp), fp[:len(fp)-1], fp + "0"} {
		if err := AcceptJoin(a.repo, "host-b", bad, AcceptOpts{AllowUnlabelled: true}); err == nil {
			t.Errorf("fingerprint %q must be refused", bad)
		}
	}
	if a.revHead(t) != head {
		t.Fatal("a refused accept must not commit")
	}
	if err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{AllowUnlabelled: true}); err != nil {
		t.Fatal(err)
	}
	if got := headTreePaths(t, a, "HEAD"); !strings.Contains(got, "D\tjoins/host-b.txt") || !strings.Contains(got, ".age-recipients") {
		t.Fatalf("one commit must add the recipient and remove the join file: %q", got)
	}
	if n := strings.Count(headTreePaths(t, a, "HEAD"), "\n") + 1; n < 2 {
		t.Fatalf("paths: %d", n)
	}
	if _, err := os.Stat(filepath.Join(a.repo.Dir, "joins", "host-b.txt")); err == nil {
		t.Error("join file must be gone from the work-tree")
	}
	if strings.Contains(a.dirty(t), "joins") {
		t.Errorf("dirty: %q", a.dirty(t))
	}
	keys, _ := ListRecipients(a.repo.Dir)
	if !strings.Contains(strings.Join(keys, "\n"), b.rcpt) {
		t.Error("recipient not added")
	}
	// The requester's label is on its line and bound by the pin, so
	// `host retire host-b` finds the key.
	if labels, _ := ListLabels(a.repo.Dir); len(labels) != 1 || labels[0] != "host-b" {
		t.Errorf("labels after accept: %v", labels)
	}
	pk, perr := PinnedKeysForLabel(a.repo, "host-b")
	if perr != nil || len(pk) != 1 || !strings.HasPrefix(b.rcpt, pk[0]) {
		t.Errorf("pin must bind host-b to the requester's key: %v %v", pk, perr)
	}
	if err := PushGuard(a.repo, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	// b: pull sees itself in the recipients; the pinned-set gate still applies.
	if st, err := JoinStatus(b.repo); err != nil || st.State == "" {
		t.Fatal(st, err)
	}
	if _, err := b.repo.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if u, _ := Unreviewed(b.repo); !u {
		t.Fatal("b must have to accept the recipients change")
	}
	if st, _ := JoinStatus(b.repo); st.State != JoinStateAccepted {
		t.Errorf("status after accept: %+v", st)
	}
	if err := AcceptRecipients(b.repo); err != nil {
		t.Fatal(err)
	}
	if err := CheckKeyed(b.repo); err != nil {
		t.Fatal(err)
	}
}

func TestJoinStatusPendingThenNone(t *testing.T) {
	_, b := pair(t)
	if st, _ := JoinStatus(b.repo); st.State != JoinStateNone {
		t.Errorf("before request: %+v", st)
	}
	RequestJoin(b.repo, "host-b", joinNow)
	st, err := JoinStatus(b.repo)
	if err != nil || st.State != JoinStatePending || st.Join.Label != "host-b" || st.Join.Fingerprint == "" {
		t.Errorf("after request: %+v %v", st, err)
	}
}

func TestJoinReject(t *testing.T) {
	a, b := pair(t)
	RequestJoin(b.repo, "host-b", joinNow)
	b.repo.Run("push", "origin", "main")
	a.repo.Run("pull", "-q", "--rebase", "origin", "main")
	before, _ := ListRecipients(a.repo.Dir)
	if err := RejectJoin(a.repo, "host-b"); err != nil {
		t.Fatal(err)
	}
	if got := headTreePaths(t, a, "HEAD"); got != "D\tjoins/host-b.txt" {
		t.Fatalf("reject commit: %q", got)
	}
	after, _ := ListRecipients(a.repo.Dir)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Error("reject must not touch recipients")
	}
	if err := RejectJoin(a.repo, "host-b"); err == nil {
		t.Error("rejecting a missing request is an error")
	}
}

// plant commits path=content on the remote from a, bypassing the filter, the
// way a malicious writer with push access would.
func plant(t *testing.T, a *host, path, content string) {
	t.Helper()
	oid, err := a.repo.RunStdin([]byte(content), "hash-object", "-w", "--no-filters", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.repo.Run("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(oid)+","+path); err != nil {
		t.Fatal(err)
	}
	if _, err := a.repo.Run("commit", "-q", "-m", "plant"); err != nil {
		t.Fatal(err)
	}
}

func TestPushGuardRefusesBadPlaintext(t *testing.T) {
	_, good := goodJoin(t, "host-x")
	j, _ := ParseJoin([]byte(good))
	staleFP := strings.Replace(good, j.Fingerprint, "0123456789abcdef", 1)
	cases := map[string]struct{ path, body string }{
		"extra content":     {"joins/host-x.txt", good + "more\n"},
		"stale fingerprint": {"joins/host-x.txt", staleFP},
		"outside joins":     {"notes/host-x.txt", good},
		"nested":            {"joins/sub/host-x.txt", good},
		"wrong ext":         {"joins/host-x.md", good},
		"label mismatch":    {"joins/other.txt", good},
		"root":              {"host-x.txt", good},
		"plain secret":      {"joins/host-x.txt", "api key is hunter2\n"},
	}
	for name, c := range cases {
		c := c
		t.Run(name, func(t *testing.T) {
			a, _ := pair(t)
			plant(t, a, c.path, c.body)
			if err := PushGuard(a.repo, "main"); err == nil {
				t.Fatalf("guard must refuse %s", name)
			}
		})
	}
	t.Run("valid passes", func(t *testing.T) {
		a, _ := pair(t)
		plant(t, a, "joins/host-x.txt", good)
		if err := PushGuard(a.repo, "main"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAcceptRefusesTamperedAndInvalid(t *testing.T) {
	a, _ := pair(t)
	_, good := goodJoin(t, "host-x")
	orig, _ := ParseJoin([]byte(good))

	// Consistently modified after the request (new key, matching fingerprint):
	// the fingerprint the human compared no longer matches.
	_, other := newIdentity(t, "evil")
	evil := Join{Label: "host-x", Recipient: other, Requested: joinNow}
	evil.Fingerprint = JoinFingerprint(other)
	plant(t, a, "joins/host-x.txt", evil.Format())
	if err := AcceptJoin(a.repo, "host-x", orig.Fingerprint, AcceptOpts{AllowUnlabelled: true}); err == nil {
		t.Fatal("modified join file must be refused against the original fingerprint")
	}
	// Invalid file: refused even with whatever fingerprint it claims.
	plant(t, a, "joins/host-y.txt", "label: host-y\nrecipient: ssh-ed25519 AAAA\nfingerprint: 00\nrequested: now\n")
	if err := AcceptJoin(a.repo, "host-y", "00", AcceptOpts{AllowUnlabelled: true}); err == nil {
		t.Fatal("invalid file must be refused")
	}
	list, err := ListJoins(a.repo)
	if err != nil || len(list) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	for _, e := range list {
		if e.Join.Label == "host-y" && e.Valid {
			t.Error("host-y must be listed as invalid")
		}
	}
	keys, _ := ListRecipients(a.repo.Dir)
	if strings.Contains(strings.Join(keys, "\n"), other) {
		t.Error("recipient added despite refusal")
	}
}

func TestAcceptNeedsAKeyedHost(t *testing.T) {
	a, b := pair(t)
	RequestJoin(b.repo, "host-b", joinNow)
	b.repo.Run("push", "origin", "main")
	a.repo.Run("pull", "-q", "--rebase", "origin", "main")
	list, _ := ListJoins(a.repo)
	// b is keyless: it cannot accept anything.
	b.repo.Run("fetch", "-q", "origin")
	if err := AcceptJoin(b.repo, "host-b", list[0].Join.Fingerprint, AcceptOpts{AllowUnlabelled: true}); err == nil {
		t.Error("keyless host must not accept")
	}
}

func TestInfoAttributesExemptJoins(t *testing.T) {
	_, b := pair(t)
	if err := WriteInfoAttributes(b.repo); err != nil {
		t.Fatal(err)
	}
	out, err := b.repo.Run("check-attr", "filter", "diff", "merge", "--", "joins/x.txt", "other/x.txt", "joins/a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"joins/x.txt: filter: unspecified", "other/x.txt: filter: memage", "joins/a/b.txt: filter: memage"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(Attributes(), "/joins/*.txt") {
		t.Error(".gitattributes content must exempt joins/*.txt")
	}
}

// acceptRefs lists what file base (in dir) may not contain. Only join.go and
// join_all.go of cmd/claude-memsync, the interactive `join accept`, may call
// AcceptJoin; in that directory nothing but main.go and join*.go may mention
// the join command's entry points, as a call or as a value (a daemon path must
// not reach them).
func acceptRefs(dir, base string, src []byte) []string {
	if strings.HasSuffix(base, "_test.go") {
		return nil
	}
	inCmd := filepath.Base(dir) == "claude-memsync"
	var bad []string
	text := string(src)
	if !(inCmd && (base == "join.go" || base == "join_all.go")) && strings.Contains(text, "AcceptJoin(") {
		bad = append(bad, "AcceptJoin(")
	}
	if inCmd && base != "main.go" && !strings.HasPrefix(base, "join") {
		for _, m := range joinEntryRE.FindAllString(text, -1) {
			bad = append(bad, m)
		}
	}
	return bad
}

var joinEntryRE = regexp.MustCompile(`\bjoin(Cmd|All|ChangeAll|ChangeOne)\b`)

// Nothing in the sync loop may be able to accept a request.
func TestNoDaemonPathAccepts(t *testing.T) {
	dirs := []string{"../sync", "../../cmd/claude-memsync"}
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.go"))
		for _, f := range files {
			b, _ := os.ReadFile(f)
			for _, call := range acceptRefs(d, filepath.Base(f), b) {
				t.Errorf("%s references %s", f, call)
			}
		}
	}
}

// The guard itself: a probe file with each forbidden call, under a name that
// merely ends like an allowed one, is caught; the allowed files are not.
func TestNoDaemonPathAccepts_GuardCatchesProbes(t *testing.T) {
	cmdDir := "../../cmd/claude-memsync"
	for _, tc := range []struct {
		dir, base, src string
		want           bool
	}{
		{cmdDir, "daemon_join_all.go", "AcceptJoin(r)", true},
		{cmdDir, "xjoin.go", "AcceptJoin(r)", true},
		{cmdDir, "join.go", "AcceptJoin(r)", false},
		{cmdDir, "join_all.go", "AcceptJoin(r)", false},
		{"../sync", "loop.go", "AcceptJoin(r)", true},
		{cmdDir, "service.go", "joinAll(w)", true},
		{cmdDir, "service.go", "joinChangeAll(w)", true},
		{cmdDir, "service.go", "joinChangeOne(w)", true},
		{cmdDir, "service.go", "joinCmd(w)", true},
		{cmdDir, "main.go", "joinCmd(w)", false},
		{cmdDir, "join_labels.go", "joinCmd(w)", false},
		{cmdDir, "service_test.go", "AcceptJoin( joinCmd(", false},
		{"../sync", "join.go", "AcceptJoin(r)", true},
		{cmdDir, "service.go", "run := joinCmd", true},
		{cmdDir, "service.go", "f := joinAll", true},
		{cmdDir, "join_all.go", "x := joinChangeOne", false},
	} {
		got := len(acceptRefs(tc.dir, tc.base, []byte(tc.src))) > 0
		if got != tc.want {
			t.Errorf("%s/%s %q: flagged=%v, want %v", filepath.Base(tc.dir), tc.base, tc.src, got, tc.want)
		}
	}
}

func TestFingerprintIs128BitsAndGroups(t *testing.T) {
	j, _ := goodJoin(t, "host-1")
	if len(j.Fingerprint) != 32 {
		t.Fatalf("want 32 hex chars, got %q", j.Fingerprint)
	}
	g := GroupFingerprint(j.Fingerprint)
	if len(strings.Fields(g)) != 8 || strings.ReplaceAll(g, " ", "") != j.Fingerprint || strings.Count(g, " ") != 7 {
		t.Errorf("grouping: %q", g)
	}
	if NormalizeFingerprint(g) != j.Fingerprint {
		t.Error("normalize must undo grouping")
	}
	if NormalizeFingerprint(strings.ToUpper(g)) == j.Fingerprint {
		t.Error("case is not forgiven")
	}
	// a 64-bit fingerprint (the earlier form) is no longer valid in a file
	short := strings.Replace(j.Format(), j.Fingerprint, j.Fingerprint[:16], 1)
	if _, err := ParseJoin([]byte(short)); err == nil {
		t.Error("64-bit fingerprint must be refused")
	}
}

// A string that looks like an age key (right shape, consistent fingerprint)
// but fails the bech32 checksum must be refused: only the real parser sees it.
func TestParseJoinRefusesBadChecksumAgeKey(t *testing.T) {
	j, _ := goodJoin(t, "host-1")
	last := j.Recipient[len(j.Recipient)-1]
	alt := byte('q')
	if last == 'q' {
		alt = 'p'
	}
	bad := j.Recipient[:len(j.Recipient)-1] + string(alt)
	if !joinAgeRE.MatchString(bad) {
		t.Fatal("test setup: the bad key must pass the shape check")
	}
	forged := Join{Label: "host-1", Recipient: bad, Fingerprint: JoinFingerprint(bad), Requested: joinNow}
	if _, err := ParseJoin([]byte(forged.Format())); err == nil {
		t.Fatal("bad-checksum age1 key must be refused")
	}
	if ValidJoinBlob("joins/host-1.txt", []byte(forged.Format())) {
		t.Fatal("and the guard must refuse it")
	}
}

func TestAcceptJoinRefusesWhileRecipientsUnreviewed(t *testing.T) {
	a, b := pair(t)
	RequestJoin(b.repo, "host-b", joinNow)
	b.repo.Run("push", "origin", "main")
	a.repo.Run("pull", "-q", "--rebase", "origin", "main")
	list, _ := ListJoins(a.repo)
	// someone changed .age-recipients upstream: the pin no longer matches
	_, extra := newIdentity(t, "x")
	keys, _ := ListRecipients(a.repo.Dir)
	if err := WriteRecipients(a.repo.Dir, append(keys, extra)); err != nil {
		t.Fatal(err)
	}
	if u, _ := Unreviewed(a.repo); !u {
		t.Fatal("setup: must be unreviewed")
	}
	head := a.revHead(t)
	err := AcceptJoin(a.repo, "host-b", list[0].Join.Fingerprint, AcceptOpts{AllowUnlabelled: true})
	if err == nil || !strings.Contains(err.Error(), "not reviewed") {
		t.Fatalf("accept must stop at the review gate, got %v", err)
	}
	if a.revHead(t) != head {
		t.Error("nothing may be committed")
	}
}

// Join requests still waiting are plaintext in HEAD after an accept: the
// re-encryption must leave them alone.
func TestAcceptLeavesOtherPendingRequestsPlaintext(t *testing.T) {
	a, b := pair(t)
	RequestJoin(b.repo, "host-b", joinNow)
	cj, ctext := goodJoin(t, "host-c")
	a.write(t, "joins/host-c.txt", ctext)
	b.repo.Run("push", "origin", "main")
	a.repo.Run("pull", "-q", "--rebase", "origin", "main")
	a.commit(t, "c asks")
	list, _ := ListJoins(a.repo)
	var fp string
	for _, e := range list {
		if e.Join.Label == "host-b" {
			fp = e.Join.Fingerprint
		}
	}
	if err := AcceptJoin(a.repo, "host-b", fp, AcceptOpts{AllowUnlabelled: true}); err != nil {
		t.Fatal(err)
	}
	if got := a.blob(t, "HEAD", "joins/host-c.txt"); got != ctext {
		t.Fatalf("pending request was re-encrypted or changed: %.60q", got)
	}
	if err := PushGuard(a.repo, "main"); err != nil {
		t.Fatal(err)
	}
	if es, _ := ListJoins(a.repo); len(es) != 1 || es[0].Join.Label != "host-c" || es[0].Join.Recipient != cj.Recipient || !es[0].Valid {
		t.Errorf("host-c must remain a valid pending request: %+v", es)
	}
}

func TestAcceptTakesGroupedFingerprint(t *testing.T) {
	a, b := pair(t)
	RequestJoin(b.repo, "host-b", joinNow)
	b.repo.Run("push", "origin", "main")
	a.repo.Run("pull", "-q", "--rebase", "origin", "main")
	list, _ := ListJoins(a.repo)
	if err := AcceptJoin(a.repo, "host-b", GroupFingerprint(list[0].Join.Fingerprint), AcceptOpts{AllowUnlabelled: true}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyJoinPush(t *testing.T) {
	t.Run("exactly the request", func(t *testing.T) {
		_, b := pair(t)
		RequestJoin(b.repo, "host-b", joinNow)
		need, err := VerifyJoinPush(b.repo, "host-b")
		if err != nil || !need {
			t.Fatalf("%v %v", need, err)
		}
	})
	t.Run("nothing to push at origin's tip", func(t *testing.T) {
		_, b := pair(t)
		RequestJoin(b.repo, "host-b", joinNow)
		b.repo.Run("push", "origin", "main")
		b.repo.Run("fetch", "-q", "origin")
		need, err := VerifyJoinPush(b.repo, "host-b")
		if err != nil || need {
			t.Fatalf("%v %v", need, err)
		}
		if _, err := VerifyJoinPush(b.repo, "other"); err == nil {
			t.Error("a label with no request must not verify")
		}
	})
	t.Run("an extra local commit", func(t *testing.T) {
		_, b := pair(t)
		plant(t, b, "projects/x.md", "local work\n")
		RequestJoin(b.repo, "host-b", joinNow)
		if _, err := VerifyJoinPush(b.repo, "host-b"); err == nil {
			t.Fatal("two commits ahead must be refused")
		}
	})
	t.Run("request commit that also touches another path", func(t *testing.T) {
		_, b := pair(t)
		_, text := goodJoin(t, "host-b")
		b.write(t, "joins/host-b.txt", text)
		b.write(t, "notes.txt", "x\n")
		b.repo.Run("add", "-f", "joins/host-b.txt")
		plantAdd(t, b, "other/y.md", "plain\n")
		if _, err := VerifyJoinPush(b.repo, "host-b"); err == nil {
			t.Fatal("a commit with a second path must be refused")
		}
	})
	t.Run("parent is not origin's tip", func(t *testing.T) {
		a, b := pair(t)
		RequestJoin(b.repo, "host-b", joinNow)
		a.write(t, "MEMORY.md", "- moved on\n")
		a.commit(t, "memsync: more")
		a.repo.Run("push", "origin", "main")
		b.repo.Run("fetch", "-q", "origin")
		if _, err := VerifyJoinPush(b.repo, "host-b"); err == nil {
			t.Fatal("origin moved: the commit is not on its tip")
		}
	})
	t.Run("modifies an existing file", func(t *testing.T) {
		_, b := pair(t)
		plantAdd(t, b, "joins/host-b.txt", "label: host-b\n")
		if _, err := VerifyJoinPush(b.repo, "host-b"); err == nil {
			t.Fatal("an invalid request file must be refused")
		}
	})
}

// plantAdd is plant without the commit message, same plumbing.
func plantAdd(t *testing.T, h *host, path, content string) { plant(t, h, path, content) }

func TestVerifyJoinPushNeedsHeadOnTheBranch(t *testing.T) {
	_, b := pair(t)
	RequestJoin(b.repo, "host-b", joinNow)
	if need, err := VerifyJoinPush(b.repo, "host-b"); err != nil || !need {
		t.Fatalf("setup: %v %v", need, err)
	}
	// detached HEAD at the same commit: the ref that would be pushed is older
	b.repo.Run("checkout", "-q", "--detach")
	if _, err := VerifyJoinPush(b.repo, "host-b"); err == nil {
		t.Fatal("detached HEAD must be refused")
	}
	b.repo.Run("checkout", "-q", "main")
	if _, err := VerifyJoinPush(b.repo, "host-b"); err != nil {
		t.Fatalf("back on the branch: %v", err)
	}
}

func TestSyncFromOriginRefusals(t *testing.T) {
	t.Run("remote tracks config.json", func(t *testing.T) {
		a, b := pair(t)
		a.write(t, "config.json", `{"githubTokenCmd":["/bin/evil"]}`)
		a.repo.Run("add", "-f", "config.json")
		plantAdd(t, a, "denylist.txt", "x\n")
		a.repo.Run("push", "origin", "main")
		before := b.revHead(t)
		if err := SyncFromOrigin(b.repo); err == nil {
			t.Fatal("must refuse a remote that tracks per-PC files")
		}
		if b.revHead(t) != before {
			t.Error("must not merge")
		}
	})
	t.Run("diverged", func(t *testing.T) {
		a, b := pair(t)
		plantAdd(t, b, "projects/local.md", "l\n")
		a.write(t, "MEMORY.md", "- remote\n")
		a.commit(t, "r")
		a.repo.Run("push", "origin", "main")
		if err := SyncFromOrigin(b.repo); err == nil {
			t.Fatal("diverged branch must be an error")
		}
	})
	t.Run("fetch failure", func(t *testing.T) {
		_, b := pair(t)
		b.repo.Run("remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
		if err := SyncFromOrigin(b.repo); err == nil {
			t.Fatal("a failed fetch is an error")
		}
	})
}

func TestPendingRecipients(t *testing.T) {
	a, b := pair(t)
	RequestJoin(b.repo, "host-b", joinNow)
	b.repo.Run("push", "origin", "main")
	a.repo.Run("pull", "-q", "--rebase", "origin", "main")
	list, _ := ListJoins(a.repo)
	AcceptJoin(a.repo, "host-b", list[0].Join.Fingerprint, AcceptOpts{AllowUnlabelled: true})
	a.repo.Run("push", "origin", "main")
	b.repo.Run("pull", "-q", "--rebase", "origin", "main")
	got, removed, err := PendingRecipients(b.repo)
	if err != nil || removed != 0 || len(got) != 2 {
		t.Fatalf("%+v %d %v", got, removed, err)
	}
	var own, other int
	for _, r := range got {
		if r.Own {
			own++
			if !r.New || r.Key != b.rcpt {
				t.Errorf("own entry: %+v", r)
			}
		} else {
			other++
			if r.New || r.Key != a.rcpt {
				t.Errorf("other entry: %+v", r)
			}
		}
		if r.Fingerprint != JoinFingerprint(r.Key) {
			t.Error("fingerprint must be the join one")
		}
	}
	if own != 1 || other != 1 {
		t.Errorf("%+v", got)
	}
}

// An accept refuses a label already bound to a different key.
func TestAcceptRefusesLabelBoundToAnotherKey(t *testing.T) {
	a, b := pair(t)
	_, other := newIdentity(t, "other")
	if err := AddRecipientLabelled(a.repo, other, "host-b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RequestJoin(b.repo, "host-b", joinNow); err != nil {
		t.Fatal(err)
	}
	b.repo.Run("push", "origin", "main")
	if _, err := a.repo.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	list, _ := ListJoins(a.repo)
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	head := a.revHead(t)
	if err := AcceptJoin(a.repo, "host-b", list[0].Join.Fingerprint, AcceptOpts{AllowUnlabelled: true}); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("a label bound to another key must be refused: %v", err)
	}
	if a.revHead(t) != head {
		t.Error("a refused accept must not commit")
	}
}

func TestRequestsForLabel_SeesHeadAndTheRemoteTipSeparately(t *testing.T) {
	a, b := pair(t)
	if got := RequestsForLabel(a.repo, "host-b"); len(got) != 0 {
		t.Fatalf("no request yet: %v", got)
	}
	if got := RequestsForLabel(a.repo, "Bad Label"); got != nil {
		t.Fatalf("an invalid label has no requests: %v", got)
	}
	j, _, err := RequestJoin(b.repo, "host-b", joinNow)
	if err != nil {
		t.Fatal(err)
	}
	b.repo.Run("push", "origin", "main")
	a.repo.Run("fetch", "-q", "origin", "main") // the tip has it, HEAD does not
	got := RequestsForLabel(a.repo, "host-b")
	if len(got) != 1 || !got[0].Valid || got[0].Join.Fingerprint != j.Fingerprint {
		t.Fatalf("the tip's request must be seen: %+v", got)
	}
	a.repo.Run("merge", "-q", "--ff-only", "origin/main")
	if got := RequestsForLabel(a.repo, "host-b"); len(got) != 1 {
		t.Fatalf("the same request in HEAD and at the tip is one: %+v", got)
	}
	// Origin moves on with something else under the label: HEAD (behind) keeps
	// the honest request, the tip holds an unusable one: two versions.
	b.write(t, "joins/host-b.txt", "garbage\n")
	if _, err := b.repo.Run("add", "-f", "joins/host-b.txt"); err != nil {
		t.Fatal(err)
	}
	b.commit(t, "tamper")
	if _, err := b.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	a.repo.Run("fetch", "-q", "origin", "main")
	got = RequestsForLabel(a.repo, "host-b")
	if len(got) != 2 {
		t.Fatalf("want HEAD's and the tip's versions: %+v", got)
	}
	valid := 0
	for _, e := range got {
		if e.Valid {
			valid++
		}
	}
	if valid != 1 {
		t.Errorf("want exactly one valid version: %+v", got)
	}
}

// HEAD and the remote tip have diverged: the tip's request is still read.
func TestRequestsForLabel_ADivergedTipIsStillRead(t *testing.T) {
	a, b := pair(t)
	j, _, err := RequestJoin(b.repo, "host-b", joinNow)
	if err != nil {
		t.Fatal(err)
	}
	b.repo.Run("push", "origin", "main")
	a.write(t, "local.txt", "x\n")
	a.commit(t, "local only") // a diverges from origin, which now holds the request
	a.repo.Run("fetch", "-q", "origin", "main")
	got := RequestsForLabel(a.repo, "host-b")
	if len(got) != 1 || !got[0].Valid || got[0].Join.Fingerprint != j.Fingerprint {
		t.Fatalf("the diverged tip's request must be seen: %+v", got)
	}
	// ListJoins keeps its rule for list/status: a diverged tip is not read.
	if es, _ := ListJoins(a.repo); len(es) != 0 {
		t.Errorf("ListJoins read a diverged tip: %+v", es)
	}
}
