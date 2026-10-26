package agecrypt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Join requests let a host that is not yet an age recipient ask an existing
// member to admit it. The request is a small plaintext file, joins/<label>.txt,
// holding only a public key. Nothing here ever accepts one without a person
// supplying the fingerprint the requester printed; no daemon code calls
// AcceptJoin.

// JoinsDir is the directory join requests live in.
const JoinsDir = "joins"

var (
	joinPathRE  = regexp.MustCompile(`^joins/[a-z0-9-]+\.txt$`)
	joinLabelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	joinAgeRE   = regexp.MustCompile(`^(age1[a-z0-9]{58}|age1pq1[a-z0-9]+)$`)
	joinFpRE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// maxJoinBytes bounds a request: a hybrid recipient alone is about 1.96 KiB.
const maxJoinBytes = 4096

// IsJoinPath reports whether a repo path may hold a plaintext join request.
func IsJoinPath(p string) bool { return joinPathRE.MatchString(p) }

// JoinPath is the repo path of a label's request.
func JoinPath(label string) string { return JoinsDir + "/" + label + ".txt" }

// ValidJoinLabel reports whether label is acceptable.
func ValidJoinLabel(label string) bool { return joinLabelRE.MatchString(label) }

// Join is one parsed request.
type Join struct {
	Label       string
	Recipient   string
	Fingerprint string
	Requested   time.Time
}

// JoinFingerprint is the fingerprint of a recipient: the first 16 bytes (128
// bits) of its SHA-256, as 32 lowercase hex characters.
func JoinFingerprint(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:16])
}

// GroupFingerprint shows a fingerprint in groups of four ("a1b2 c3d4 ..."),
// the form people read and type.
func GroupFingerprint(fp string) string {
	var parts []string
	for len(fp) > 4 {
		parts = append(parts, fp[:4])
		fp = fp[4:]
	}
	return strings.Join(append(parts, fp), " ")
}

// NormalizeFingerprint drops the spaces of the grouped form. Nothing else is
// forgiven: case and every character still have to match exactly.
func NormalizeFingerprint(s string) string { return strings.ReplaceAll(s, " ", "") }

// Format renders the request in the one accepted form.
func (j Join) Format() string {
	return fmt.Sprintf("label: %s\nrecipient: %s\nfingerprint: %s\nrequested: %s\n",
		j.Label, j.Recipient, j.Fingerprint, j.Requested.UTC().Format(time.RFC3339))
}

// ParseJoin parses a request strictly: exactly the four lines Format writes,
// in order, nothing else.
func ParseJoin(b []byte) (Join, error) {
	var j Join
	bad := errors.New("not a valid join request")
	if len(b) == 0 || len(b) > maxJoinBytes || b[len(b)-1] != '\n' {
		return j, bad
	}
	lines := strings.Split(string(b[:len(b)-1]), "\n")
	if len(lines) != 4 {
		return j, bad
	}
	val := func(i int, key string) (string, bool) {
		v, ok := strings.CutPrefix(lines[i], key+": ")
		return v, ok && v != "" && strings.TrimSpace(v) == v && !strings.ContainsAny(v, "\r\t ")
	}
	var ok bool
	var err error
	if j.Label, ok = val(0, "label"); !ok || !joinLabelRE.MatchString(j.Label) {
		return j, bad
	}
	if j.Recipient, ok = val(1, "recipient"); !ok || !joinAgeRE.MatchString(j.Recipient) {
		return j, bad
	}
	if strings.HasPrefix(j.Recipient, hybridRecipientPrefix) {
		_, err = age.ParseHybridRecipient(j.Recipient)
	} else {
		_, err = age.ParseX25519Recipient(j.Recipient)
	}
	if err != nil {
		return j, bad
	}
	if j.Fingerprint, ok = val(2, "fingerprint"); !ok || !joinFpRE.MatchString(j.Fingerprint) || j.Fingerprint != JoinFingerprint(j.Recipient) {
		return j, bad
	}
	ts, ok := val(3, "requested")
	if !ok || !strings.HasSuffix(ts, "Z") {
		return j, bad
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil || t.UTC().Format(time.RFC3339) != ts {
		return j, bad
	}
	j.Requested = t.UTC()
	return j, nil
}

// ValidJoinBlob reports whether content is an acceptable plaintext blob at
// path: a join path, a valid request, and a label equal to the file name.
func ValidJoinBlob(path string, content []byte) bool {
	if !IsJoinPath(path) {
		return false
	}
	j, err := ParseJoin(content)
	return err == nil && JoinPath(j.Label) == path
}

// WriteInfoAttributes refreshes $GIT_DIR/info/attributes with the current
// rules (including the joins/ exemption). Idempotent.
func WriteInfoAttributes(repo *gitwt.Repo) error { return writeInfoAttributes(repo) }

func branchOf(repo *gitwt.Repo) string {
	b, err := repo.CurrentBranch()
	if err != nil || b == "" || b == "HEAD" {
		return "main"
	}
	return b
}

// joinRefs lists the revisions to read requests from: HEAD, and the
// remote-tracking tip when there is one (nothing is fetched here).
func joinRefs(repo *gitwt.Repo) []string {
	refs := []string{"HEAD"}
	r := "refs/remotes/origin/" + branchOf(repo)
	if _, err := repo.Run("rev-parse", "--verify", "--quiet", r); err == nil {
		// The tip counts only while HEAD is behind it: a HEAD that is ahead of
		// (or has moved past) the tip has the newer state, e.g. an accept that
		// removed a request the stale tip still lists.
		if behind, err := repo.IsAncestor("HEAD", r); err == nil && behind {
			refs = append(refs, r)
		}
	}
	return refs
}

func blobAtRev(repo *gitwt.Repo, rev, path string) ([]byte, bool) {
	out, err := repo.Run("cat-file", "blob", rev+":"+path)
	if err != nil {
		return nil, false
	}
	return []byte(out), true
}

func recipientsAt(repo *gitwt.Repo, rev string) *Recipients {
	b, ok := blobAtRev(repo, rev, RecipientsFile)
	if !ok {
		return nil
	}
	r, err := ParseRecipients(b)
	if err != nil {
		return nil
	}
	return r
}

func hasRecipient(r *Recipients, key string) bool {
	if r == nil {
		return false
	}
	for _, c := range r.canon {
		if k, _ := splitCanon(c); k == key { // the reviewed label is not part of the key
			return true
		}
	}
	return false
}

// SyncFromOrigin fetches and, when the local branch is behind, fast-forwards
// it, so a request or an accept sees the latest remote state. A keyless clone
// can do this too: its ciphertext is checked out as it is. It refuses a remote
// tree that tracks per-PC files (config.json and the rest), and a local branch
// that has diverged from the remote. Any error means the clone was not brought
// up to date.
func SyncFromOrigin(repo *gitwt.Repo) error {
	br := branchOf(repo)
	if _, err := repo.Run("fetch", "-q", "origin", br); err != nil {
		return err
	}
	tip := "refs/remotes/origin/" + br
	if _, err := repo.Run("rev-parse", "--verify", "--quiet", tip); err != nil {
		return nil
	}
	if bad, err := repo.TrackedLocalOnly(tip); err != nil {
		return err
	} else if len(bad) > 0 {
		return errors.New("pull refused: the remote tracks per-PC files (" + strings.Join(bad, ", ") + ")")
	}
	head, _ := repo.Run("rev-parse", "HEAD")
	t, _ := repo.Run("rev-parse", tip)
	if strings.TrimSpace(head) == strings.TrimSpace(t) {
		return nil
	}
	if anc, err := repo.IsAncestor(tip, "HEAD"); err == nil && anc {
		return nil // ahead of the remote
	}
	if anc, err := repo.IsAncestor("HEAD", tip); err != nil || !anc {
		return errors.New("local branch has diverged from the remote; sync it first")
	}
	_, err := repo.Run("merge", "--ff-only", "-q", tip)
	return err
}

// VerifyJoinPush says whether HEAD holds something to push for a join request
// and checks that it is exactly the request and nothing else: one new commit
// on top of origin's tip (its only parent), whose only change adds
// joins/<label>.txt, a valid request. needPush is false when HEAD is origin's
// tip. Anything else is an error, and nothing should be pushed.
func VerifyJoinPush(repo *gitwt.Repo, label string) (needPush bool, err error) {
	br := branchOf(repo)
	tip, err := repo.Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+br)
	if err != nil {
		return false, errors.New("origin has no such branch yet: nothing to join")
	}
	tip = strings.TrimSpace(tip)
	head, err := repo.Run("rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	head = strings.TrimSpace(head)
	// What gets pushed is the branch ref, so HEAD must be that ref: on the
	// branch (not detached) and the branch at HEAD.
	if sym, err := repo.Run("symbolic-ref", "-q", "HEAD"); err != nil || strings.TrimSpace(sym) != "refs/heads/"+br {
		return false, errors.New("refusing to push: HEAD is not on branch " + br)
	}
	if ref, err := repo.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+br); err != nil || strings.TrimSpace(ref) != head {
		return false, errors.New("refusing to push: branch " + br + " is not at HEAD")
	}
	path := JoinPath(label)
	if head == tip {
		if _, ok := blobAtRev(repo, "HEAD", path); !ok {
			return false, errors.New("no join request for this label on origin")
		}
		return false, nil
	}
	refuse := errors.New("refusing to push: the local branch is not origin's tip plus exactly the join request commit")
	line, err := repo.Run("rev-list", "--parents", "-n1", "HEAD")
	if err != nil {
		return false, err
	}
	if f := strings.Fields(line); len(f) != 2 || f[1] != tip {
		return false, refuse
	}
	out, err := repo.Run("diff-tree", "-r", "--no-renames", "--raw", "--no-abbrev", "-z", tip, head)
	if err != nil {
		return false, err
	}
	f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(f) != 2 || !strings.HasPrefix(f[0], ":000000 100644 ") || !strings.HasSuffix(f[0], " A") || f[1] != path {
		return false, refuse
	}
	if b, ok := blobAtRev(repo, "HEAD", path); !ok || !ValidJoinBlob(path, b) {
		return false, refuse
	}
	return true, nil
}

// PendingRecipient is one key of the work-tree's .age-recipients that this
// host has not pinned yet.
type PendingRecipient struct {
	Key         string
	Fingerprint string // JoinFingerprint of the key, as join list shows it
	Label       string // host label on the line, if any
	New         bool   // not in the pinned set
	Own         bool   // this host's key
	Desc        string // "host:<label>", "recovery" or "unlabelled", as the work-tree's line says
}

// DescribeLabel says what a recipients line is: "host:<label>", "recovery" or
// "unlabelled".
func DescribeLabel(line string) string {
	if l := LabelOf(line); l != "" {
		return "host:" + l
	}
	if strings.HasSuffix(strings.TrimSpace(line), "# recovery") {
		return "recovery"
	}
	return "unlabelled"
}

// descByKey maps each key of the work-tree's recipients file to DescribeLabel.
func descByKey(dir string) map[string]string {
	m := map[string]string{}
	lines, err := ListRecipients(dir)
	if err != nil {
		return m
	}
	for _, l := range lines {
		m[canonLine(l)] = DescribeLabel(l)
	}
	return m
}

// OwnKeyInfo is this host's own recipient key as `join accept` reports it: the
// grouped fingerprint and the label of its line. ok is false when the host's
// key is not a recipient (or its identity cannot be read).
func OwnKeyInfo(repo *gitwt.Repo) (fingerprint, desc string, ok bool) {
	own, err := IdentityRecipient(IdentityPath(repo))
	if err != nil {
		return "", "", false
	}
	d, found := descByKey(repo.Dir)[own]
	if !found {
		return "", "", false
	}
	return GroupFingerprint(JoinFingerprint(own)), d, true
}

// PendingRecipients lists the work-tree's recipient set against the pin, so a
// person can compare it with `join list` / `recipients list` on a member host
// before accepting. removed counts pinned keys that are gone.
func PendingRecipients(repo *gitwt.Repo) (list []PendingRecipient, removed int, err error) {
	pin, err := readPin(repo)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(filepath.Join(repo.Dir, RecipientsFile))
	if err != nil {
		return nil, 0, err
	}
	cur, err := ParseRecipients(data)
	if err != nil {
		return nil, 0, err
	}
	own, _ := IdentityRecipient(IdentityPath(repo))
	descs := descByKey(repo.Dir)
	pinned := map[string]bool{}
	for _, c := range pin.canon {
		pinned[c] = true
	}
	in := map[string]bool{}
	for _, c := range cur.canon {
		in[c] = true
		k, l := splitCanon(c)
		list = append(list, PendingRecipient{Key: k, Label: l, Fingerprint: JoinFingerprint(k), New: !pinned[c], Own: k == own, Desc: descs[k]})
	}
	for c := range pinned {
		if !in[c] {
			removed++
		}
	}
	return list, removed, nil
}

// Join request outcomes.
type JoinResult string

const (
	JoinRequested     JoinResult = "requested"
	JoinExists        JoinResult = "exists"
	JoinAlreadyMember JoinResult = "member"
)

// RequestJoin commits joins/<label>.txt for this host's key, on top of HEAD,
// and nothing else. It is the one commit a keyless host may make, so it does
// not go through the keyless check; the commit is built from HEAD's tree and
// that single new blob in a private index, so whatever is staged in the real
// index is never committed. It encrypts nothing, so it does not need the
// recipients to be reviewed (push it with PushGuardJoinRequest). It does not push. Calling it again for a key that
// already has a request changes nothing.
func RequestJoin(repo *gitwt.Repo, label string, now time.Time) (Join, JoinResult, error) {
	var j Join
	if !ValidJoinLabel(label) {
		return j, "", fmt.Errorf("label must match %s", joinLabelRE)
	}
	if !Enabled(repo) {
		return j, "", errors.New("the sync repo is not encrypted")
	}
	if !repo.HasHead() {
		return j, "", errors.New("the repo has no commits yet; nothing to join")
	}
	own, err := IdentityRecipient(IdentityPath(repo))
	if err != nil {
		return j, "", err
	}
	if !joinAgeRE.MatchString(own) {
		return j, "", errors.New("this host's identity is not an age key")
	}
	j = Join{Label: label, Recipient: own, Fingerprint: JoinFingerprint(own), Requested: now.UTC().Truncate(time.Second)}
	if hasRecipient(recipientsAt(repo, "HEAD"), own) {
		return j, JoinAlreadyMember, nil
	}
	// An existing request for this key (under any label) is the same request.
	for _, r := range joinRefs(repo) {
		if hasRecipient(recipientsAt(repo, r), own) {
			return j, JoinAlreadyMember, nil
		}
		for _, e := range listJoinsAt(repo, r) {
			if e.Valid && e.Join.Recipient == own {
				return e.Join, JoinExists, nil
			}
		}
		if _, ok := blobAtRev(repo, r, JoinPath(label)); ok {
			return j, "", fmt.Errorf("label %q is already used by another key", label)
		}
	}
	fp := filepath.Join(repo.Dir, filepath.FromSlash(JoinPath(label)))
	if _, err := os.Lstat(fp); err == nil {
		return j, "", fmt.Errorf("%s exists in the work-tree but not in the repo; move it away", JoinPath(label))
	}
	if err := writeInfoAttributes(repo); err != nil {
		return j, "", err
	}
	oldRev, err := repo.Run("rev-parse", "HEAD")
	if err != nil {
		return j, "", err
	}
	oid, err := repo.RunStdin([]byte(j.Format()), "hash-object", "-w", "--no-filters", "--stdin")
	if err != nil {
		return j, "", err
	}
	info := fmt.Sprintf("100644 %s\t%s\x00", strings.TrimSpace(oid), JoinPath(label))
	if err := commitInfo(repo, strings.TrimSpace(oldRev), "memsync: join request", info, map[string]string{}, nil); err != nil {
		return j, "", err
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o700); err == nil {
		_ = os.WriteFile(fp, []byte(j.Format()), 0o600)
	}
	return j, JoinRequested, nil
}

// JoinEntry is one request found in the repo.
type JoinEntry struct {
	Path  string
	Join  Join
	Valid bool
}

func listJoinsAt(repo *gitwt.Repo, rev string) []JoinEntry {
	out, err := repo.Run("ls-tree", "-r", "-z", "--name-only", rev, "--", JoinsDir)
	if err != nil {
		return nil
	}
	var res []JoinEntry
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		e := JoinEntry{Path: p}
		if IsJoinPath(p) {
			if b, ok := blobAtRev(repo, rev, p); ok {
				if j, err := ParseJoin(b); err == nil && JoinPath(j.Label) == p {
					e.Join, e.Valid = j, true
				}
			}
		}
		if !e.Valid {
			e.Join.Label = strings.TrimSuffix(filepath.Base(p), ".txt")
		}
		res = append(res, e)
	}
	return res
}

// ListJoins lists the requests in HEAD and the remote-tracking tip, by path,
// invalid ones flagged. Nothing is fetched.
func ListJoins(repo *gitwt.Repo) ([]JoinEntry, error) {
	if !repo.IsRepo() {
		return nil, errors.New("not a git repo")
	}
	seen := map[string]bool{}
	var res []JoinEntry
	for _, r := range joinRefs(repo) {
		for _, e := range listJoinsAt(repo, r) {
			if !seen[e.Path] {
				seen[e.Path] = true
				res = append(res, e)
			}
		}
	}
	sort.Slice(res, func(i, k int) bool { return res[i].Path < res[k].Path })
	return res, nil
}

// PendingJoins counts the valid requests whose key is not yet a recipient.
func PendingJoins(repo *gitwt.Repo) int {
	es, err := ListJoins(repo)
	if err != nil {
		return 0
	}
	cur := recipientsAt(repo, "HEAD")
	n := 0
	for _, e := range es {
		if e.Valid && !hasRecipient(cur, e.Join.Recipient) {
			n++
		}
	}
	return n
}

// AcceptJoin admits the requester of label. The fingerprint (as the requesting
// host printed it) must equal the one in the file exactly; an empty or wrong
// one is refused. The recipient is added, every file re-encrypted, and the
// request removed, all in one commit. Nothing is pushed.
func AcceptJoin(repo *gitwt.Repo, label, fingerprint string, opts AcceptOpts) error {
	if !ValidJoinLabel(label) {
		return fmt.Errorf("label must match %s", joinLabelRE)
	}
	if NormalizeFingerprint(fingerprint) == "" {
		return errors.New("refusing to accept without --fingerprint: compare it with the one the requesting host printed")
	}
	if err := CheckKeyed(repo); err != nil {
		return err
	}
	if u, _ := Unreviewed(repo); u {
		return ErrRecipientsUnreviewed
	}
	path := JoinPath(label)
	b, ok := blobAtRev(repo, "HEAD", path)
	if !ok {
		return fmt.Errorf("no join request %q in this clone (has it been pulled?)", label)
	}
	j, err := ParseJoin(b)
	if err != nil || JoinPath(j.Label) != path {
		return fmt.Errorf("join request %q is malformed: refusing (reject it)", label)
	}
	if NormalizeFingerprint(fingerprint) != j.Fingerprint {
		return errors.New("fingerprint does not match the request: not accepted")
	}
	if opts.OwnLabel != "" && j.Label == opts.OwnLabel {
		return fmt.Errorf("label %q is this host's own label: not accepted (a label names exactly one host)", j.Label)
	}
	if bound, perr := PinnedKeysForLabel(repo, j.Label); perr != nil && !errors.Is(perr, ErrLabelAmbiguous) {
		return perr
	} else if len(bound) > 0 || perr != nil {
		return fmt.Errorf("label %q is already bound to a key in the pinned recipients: not accepted", j.Label)
	}
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return err
	}
	if !opts.AllowUnlabelled {
		for _, l := range cur {
			if LabelOf(l) == "" && !isRecoveryKey(l, opts.RecoveryKeys) {
				return fmt.Errorf("a recipient (key %s) has no host label, so its host could not be retired by name: run `claude-memsync recipients label self --host <label>` on that host first, or pass --allow-unlabelled", keyPrint(canonLine(l)))
			}
		}
	}
	// The requester's host label is recorded on its line ("# host:<label>"),
	// the same as `recipients add --host`, so the pin binds it to the key and
	// `host retire <label>` can find the host.
	for _, l := range cur {
		if LabelOf(l) == j.Label && canonLine(l) != j.Recipient {
			return fmt.Errorf("host label %q is already bound to another recipient", j.Label)
		}
	}
	keys := make([]string, 0, len(cur)+1)
	have := false
	for _, l := range cur {
		if canonLine(l) == j.Recipient {
			have = true
			if LabelOf(l) == "" {
				l = strings.TrimSpace(l) + hostComment + j.Label
			}
		}
		keys = append(keys, l)
	}
	if !have {
		keys = append(keys, j.Recipient+hostComment+j.Label)
	}
	if err := CheckKeysForScheme(SchemeOf(repo), keys); err != nil {
		return err
	}
	if err := WriteRecipients(repo.Dir, keys); err != nil {
		return err
	}
	if err := PinCurrent(repo); err != nil {
		return err
	}
	return reencryptHeadDrop(repo, "memsync: recipients updated", []string{path})
}

// AcceptOpts are the extra refusals of AcceptJoin. OwnLabel is this host's own
// hostLabel (a requester may not take it). Unless AllowUnlabelled, an accept is
// refused while any recipient other than a configured recovery key has no host
// label.
type AcceptOpts struct {
	OwnLabel        string
	AllowUnlabelled bool
	// RecoveryKeys are the recovery recipients configured on this host
	// (config recoveryRecipients). Only a line whose key is one of them is
	// exempt from the unlabelled check; a "# recovery" comment alone is not.
	RecoveryKeys []string
}

// isRecoveryKey reports whether the line's key is one of the configured
// recovery recipients.
func isRecoveryKey(l string, keys []string) bool {
	k := canonLine(l)
	for _, r := range keys {
		if k != "" && k == canonLine(r) {
			return true
		}
	}
	return false
}

// RejectJoin removes a request without admitting anyone: one commit that
// deletes the file. Nothing is pushed.
func RejectJoin(repo *gitwt.Repo, label string) error {
	if !ValidJoinLabel(label) {
		return fmt.Errorf("label must match %s", joinLabelRE)
	}
	if !Enabled(repo) {
		return errors.New("the sync repo is not encrypted")
	}
	if u, _ := Unreviewed(repo); u {
		return ErrRecipientsUnreviewed
	}
	path := JoinPath(label)
	if _, ok := blobAtRev(repo, "HEAD", path); !ok {
		return fmt.Errorf("no join request %q in this clone", label)
	}
	oldRev, err := repo.Run("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	var info strings.Builder
	headOID := map[string]string{}
	if err := addDrops(repo, &info, headOID, []string{path}); err != nil {
		return err
	}
	return commitInfo(repo, strings.TrimSpace(oldRev), "memsync: join request removed", info.String(), headOID, []string{path})
}

// Join states for the requesting host.
const (
	JoinStateNone     = "none"
	JoinStatePending  = "pending"
	JoinStateAccepted = "accepted"
)

// JoinStatusInfo is what `join status` reports.
type JoinStatusInfo struct {
	State string
	Join  Join
}

// JoinStatus reports, for the requesting host, whether its key has a pending
// request or is already among the recipients (HEAD or remote-tracking tip).
func JoinStatus(repo *gitwt.Repo) (JoinStatusInfo, error) {
	own, err := IdentityRecipient(IdentityPath(repo))
	if err != nil {
		return JoinStatusInfo{State: JoinStateNone}, err
	}
	st := JoinStatusInfo{State: JoinStateNone, Join: Join{Recipient: own, Fingerprint: JoinFingerprint(own)}}
	for _, r := range joinRefs(repo) {
		if hasRecipient(recipientsAt(repo, r), own) {
			st.State = JoinStateAccepted
			return st, nil
		}
	}
	for _, r := range joinRefs(repo) {
		for _, e := range listJoinsAt(repo, r) {
			if e.Valid && e.Join.Recipient == own {
				st.State, st.Join = JoinStatePending, e.Join
				return st, nil
			}
		}
	}
	return st, nil
}
