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
	joinAgeRE   = regexp.MustCompile(`^age1[a-z0-9]{58}$`)
	joinFpRE    = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

const maxJoinBytes = 1024

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

// JoinFingerprint is the short fingerprint of a recipient: the first 8 bytes
// of its SHA-256, in hex.
func JoinFingerprint(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:8])
}

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
	if j.Label, ok = val(0, "label"); !ok || !joinLabelRE.MatchString(j.Label) {
		return j, bad
	}
	if j.Recipient, ok = val(1, "recipient"); !ok || !joinAgeRE.MatchString(j.Recipient) {
		return j, bad
	}
	if _, err := age.ParseX25519Recipient(j.Recipient); err != nil {
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
		refs = append(refs, r)
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
		if c == key {
			return true
		}
	}
	return false
}

// SyncFromOrigin fetches and, when the local branch is behind, fast-forwards
// it, so a request or an accept sees the latest remote state. A keyless clone
// can do this too: its ciphertext is checked out as it is. Failures are
// returned for the caller to treat as "work from what is local".
func SyncFromOrigin(repo *gitwt.Repo) error {
	br := branchOf(repo)
	if _, err := repo.Run("fetch", "-q", "origin", br); err != nil {
		return err
	}
	tip := "refs/remotes/origin/" + br
	if _, err := repo.Run("rev-parse", "--verify", "--quiet", tip); err != nil {
		return nil
	}
	if same, _ := repo.Run("rev-parse", "HEAD"); same != "" {
		t, _ := repo.Run("rev-parse", tip)
		if strings.TrimSpace(same) == strings.TrimSpace(t) {
			return nil
		}
	}
	if anc, err := repo.IsAncestor("HEAD", tip); err != nil || !anc {
		return errors.New("local branch is not behind the remote; sync first")
	}
	_, err := repo.Run("merge", "--ff-only", "-q", tip)
	return err
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
func AcceptJoin(repo *gitwt.Repo, label, fingerprint string) error {
	if !ValidJoinLabel(label) {
		return fmt.Errorf("label must match %s", joinLabelRE)
	}
	if fingerprint == "" {
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
	if fingerprint != j.Fingerprint {
		return errors.New("fingerprint does not match the request: not accepted")
	}
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return err
	}
	keys := cur
	have := false
	for _, l := range cur {
		if canonLine(l) == j.Recipient {
			have = true
		}
	}
	if !have {
		keys = append(append([]string(nil), cur...), j.Recipient)
	}
	if err := WriteRecipients(repo.Dir, keys); err != nil {
		return err
	}
	if err := PinCurrent(repo); err != nil {
		return err
	}
	return reencryptHeadDrop(repo, "memsync: recipients updated", []string{path})
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
