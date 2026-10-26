package agecrypt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// BackupRef is the local ref `rekey-pq --apply` keeps at the pre-migration tip,
// so the classic history can be rolled back to until it is dropped.
const BackupRef = "refs/memsync/pre-pq"

// RekeyOpts says how a classic single-member repo becomes post-quantum.
type RekeyOpts struct {
	Label string // this host's label, recorded as "# host:<label>"
	// Recovery is the hybrid recovery public key (age1pq1...), recorded as
	// "# recovery".
	Recovery string
	// ConfiguredRecovery are the classic recovery keys the old config names;
	// they may be recipients of the old set without counting as another member.
	ConfiguredRecovery []string
	FreshRoot          bool
	// NewIdentityPath is where the new hybrid identity lives (created if
	// missing, reused if it already is a hybrid identity file).
	NewIdentityPath string
	// Prior is the config.json state a rollback restores (recorded, public values).
	Prior PriorConfig
}

// RekeyPlan is what a rekey would do (or did), counts only.
type RekeyPlan struct {
	Tip          string // HEAD before the rekey
	Recipients   int    // recipients of the current set
	Blobs        int    // non-exempt tracked files to re-encrypt
	Commits      int    // commits in the current history
	JoinRequests int    // plaintext join requests, dropped
	OwnClassic   string // this host's classic public key
	NewCommit    string // after Apply: the new HEAD
	NewRecipient string // after Apply: this host's new hybrid public key
	IdentityPath string // the identity in use before the rekey
}

func canonSet(keys []string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		if c := canonLine(k); c != "" {
			m[c] = true
		}
	}
	return m
}

// PlanRekeyPQ checks that the repo may be rekeyed and describes it. It writes
// nothing. fetch brings in the remote-tracking ref first (apply does; a dry
// run does not, so it changes nothing at all).
func PlanRekeyPQ(repo *gitwt.Repo, o RekeyOpts, fetch bool) (RekeyPlan, error) {
	var p RekeyPlan
	if !repo.IsRepo() || !Enabled(repo) {
		return p, errors.New("the sync repo is not encrypted")
	}
	if SchemeOf(repo) == SchemePQ {
		return p, errors.New("already post-quantum (encryptionScheme pq); nothing to do")
	}
	if !ValidJoinLabel(o.Label) {
		return p, fmt.Errorf("--host: label must match %s", joinLabelRE)
	}
	if !IsHybridRecipient(o.Recovery) {
		return p, errors.New("--recovery-recipient must be an age1pq1... hybrid public key")
	}
	if err := CheckKeysForScheme(SchemePQ, []string{o.Recovery}); err != nil {
		return p, err
	}
	if !repo.HasHead() {
		return p, errors.New("the repo has no commits yet; init it with the pq scheme instead")
	}
	br := branchOf(repo)
	if cur, err := repo.CurrentBranch(); err != nil || cur != br || br != "main" {
		return p, errors.New("rekey-pq works on the main branch")
	}
	if _, err := repo.Run("rev-parse", "--verify", "--quiet", BackupRef); err == nil {
		return p, fmt.Errorf("a backup ref %s already exists; drop it first with `recipients rekey-pq --drop-backup`", BackupRef)
	}
	if err := CheckKeyed(repo); err != nil {
		return p, err
	}
	if u, _ := Unreviewed(repo); u {
		return p, ErrRecipientsUnreviewed
	}
	if repo.MergeInProgress() || repo.RebaseInProgress() || repo.HasUnmerged() {
		return p, errors.New("a merge or rebase is in progress")
	}
	// An unfinished park or pull belongs to the daemon's recovery, which
	// must run before the history is rewritten under it.
	for _, m := range []struct{ file, what string }{
		{"parked.json", "files held back by a secret scan are still parked"},
		{"pull-running", "a pull was interrupted"},
	} {
		if _, err := os.Stat(filepath.Join(repo.Dir, ".state", m.file)); err == nil {
			return p, fmt.Errorf(".state/%s exists: %s. Start the daemon (`claude-memsync start`) and let one cycle finish so it recovers (the file goes away), stop it, then rerun this command", m.file, m.what)
		}
	}
	// Dirty or unpushed state would be lost or published by a rewrite.
	if out, err := repo.Run("status", "--porcelain"); err != nil {
		return p, err
	} else if strings.TrimSpace(out) != "" {
		return p, errors.New("the work-tree has uncommitted changes; let the daemon sync them first (refusing)")
	}
	if fetch {
		if _, err := repo.Run("fetch", "-q", "origin", br); err != nil {
			return p, fmt.Errorf("cannot fetch origin: %w", err)
		}
	}
	tip, err := repo.Run("rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	p.Tip = strings.TrimSpace(tip)
	rt, err := repo.Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+br)
	switch {
	case err != nil:
		return p, errors.New("the repo has never been pushed (no origin/main); refusing")
	case strings.TrimSpace(rt) != p.Tip:
		return p, errors.New("the local branch differs from origin/main (unpushed or behind); sync first (refusing)")
	}

	idPath := IdentityPath(repo)
	p.IdentityPath = idPath
	own, err := IdentityRecipient(idPath)
	if err != nil {
		return p, err
	}
	if IsHybridRecipient(own) {
		return p, errors.New("this host's identity is already hybrid but the repo is classic; refusing")
	}
	p.OwnClassic = own
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return p, err
	}
	p.Recipients = len(cur)
	allowed := canonSet(o.ConfiguredRecovery)
	allowed[own] = true
	for _, l := range cur {
		if !allowed[canonLine(l)] {
			return p, errors.New("the recipients include a key that is neither this host's nor a configured recovery key: another member exists; rekeying would lock it out (refusing)")
		}
	}
	if !canonSet(cur)[own] {
		return p, errors.New("this host's key is not a recipient of the repo")
	}
	out, err := repo.Run("ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return p, err
	}
	for _, rec := range strings.Split(out, "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		path := rec[tab+1:]
		if len(f) != 3 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") {
			continue
		}
		if IsJoinPath(path) {
			p.JoinRequests++
		} else if !Exempt(path) {
			p.Blobs++
		}
	}
	if n, err := repo.Run("rev-list", "--count", "HEAD"); err == nil {
		fmt.Sscanf(strings.TrimSpace(n), "%d", &p.Commits)
	}
	return p, nil
}

// ApplyRekeyPQ migrates the repo locally: a new hybrid identity, the recipient
// set replaced by exactly {this host, the recovery key}, every file at HEAD
// re-encrypted to it in one new commit (an orphan root with FreshRoot), the
// scheme recorded and the pin moved. The pre-migration tip is kept at
// BackupRef. It pushes nothing; the caller holds the daemon lock and pushes.
//
// Everything is prepared (identity file, encrypted blobs) before the first
// change to a ref or the recipients file, and those two are put back if the
// commit fails. The push guard looks at the built commit before HEAD moves. A
// restore record (RekeyStateFile) is written first, for a resume or a rollback.
func ApplyRekeyPQ(repo *gitwt.Repo, o RekeyOpts) (RekeyPlan, error) {
	p, err := PlanRekeyPQ(repo, o, true)
	if err != nil {
		return p, err
	}
	if o.NewIdentityPath == "" || o.NewIdentityPath == p.IdentityPath {
		return p, errors.New("no separate path for the new identity")
	}
	oldIDs, err := LoadIdentities(p.IdentityPath)
	if err != nil {
		return p, err
	}
	var newID *age.HybridIdentity
	if _, serr := os.Stat(o.NewIdentityPath); serr == nil {
		ids, lerr := LoadIdentities(o.NewIdentityPath)
		if lerr != nil {
			return p, lerr
		}
		for _, id := range ids {
			if h, ok := id.(*age.HybridIdentity); ok {
				newID = h
			}
		}
		if newID == nil {
			return p, fmt.Errorf("%s exists but holds no hybrid identity", filepath.Base(o.NewIdentityPath))
		}
	} else {
		if newID, err = age.GenerateHybridIdentity(); err != nil {
			return p, err
		}
		if _, err := writeHybridIdentity(o.NewIdentityPath, newID); err != nil {
			return p, err
		}
	}
	p.NewRecipient = newID.Recipient().String()
	keys := []string{p.NewRecipient + hostComment + o.Label, strings.TrimSpace(o.Recovery) + " # recovery"}
	data, err := recipientsBytes(keys)
	if err != nil {
		return p, err
	}
	set, err := ParseRecipients(data)
	if err != nil {
		return p, err
	}
	var drop []string
	for _, e := range listJoinsAt(repo, "HEAD") {
		drop = append(drop, e.Path)
	}
	info, headOID, err := encryptHeadTree(repo, append(oldIDs, newID), set, data, drop)
	if err != nil {
		return p, err
	}

	recPath := filepath.Join(repo.Dir, RecipientsFile)
	oldData, _ := os.ReadFile(recPath)
	st := PreRekeyState{
		Tip: p.Tip, ClassicIdentity: p.IdentityPath, NewIdentity: filepath.ToSlash(o.NewIdentityPath),
		NewRecipient: p.NewRecipient, Label: o.Label, Recovery: strings.TrimSpace(o.Recovery), FreshRoot: o.FreshRoot,
		Config: o.Prior, Stage: RekeyRecStarted,
	}
	st.PriorScheme, st.PriorSchemeSet = rawScheme(repo)
	if err := writeRekeyState(repo, st); err != nil {
		return p, err
	}
	undo := func() {
		removeRekeyState(repo)
		_, _ = repo.Run("update-ref", "-d", BackupRef)
		if oldData != nil {
			_ = os.WriteFile(recPath, oldData, 0o600)
			_ = PinCurrent(repo)
		}
	}
	if _, err := repo.Run("update-ref", BackupRef, p.Tip); err != nil {
		removeRekeyState(repo)
		return p, err
	}
	if err := os.WriteFile(recPath, data, 0o600); err != nil {
		undo()
		return p, err
	}
	if err := commitInfoMode(repo, p.Tip, "memsync: recipients updated", info, headOID, drop, o.FreshRoot,
		func(commit string) error {
			if err := PushGuardCommit(repo, "main", commit); err != nil {
				return err
			}
			// Recorded before HEAD moves, so a crash cannot lose it.
			return UpdateRekeyState(repo, func(s *PreRekeyState) { s.NewCommit, s.Stage = commit, RekeyRecCommitted })
		}); err != nil {
		undo()
		return p, err
	}
	if err := repointLocal(repo, o.NewIdentityPath); err != nil {
		return p, err
	}
	if err := UpdateRekeyState(repo, func(s *PreRekeyState) { s.Stage = RekeyRecMigrated }); err != nil {
		return p, err
	}
	head, err := repo.Run("rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	p.NewCommit = strings.TrimSpace(head)
	return p, nil
}

// VerifyHybridRemote checks, after a push, that origin/<branch> holds
// wantCommits commits (0: do not check) and that every non-exempt blob at its
// tip is hybrid ciphertext. It returns the counts.
func VerifyHybridRemote(repo *gitwt.Repo, branch string, wantCommits int) (commits, blobs int, err error) {
	if _, err = repo.Run("fetch", "-q", "origin", branch); err != nil {
		return 0, 0, err
	}
	tip := "refs/remotes/origin/" + branch
	n, err := repo.Run("rev-list", "--count", tip)
	if err != nil {
		return 0, 0, err
	}
	fmt.Sscanf(strings.TrimSpace(n), "%d", &commits)
	if wantCommits > 0 && commits != wantCommits {
		return commits, 0, fmt.Errorf("origin/%s has %d commits, expected %d", branch, commits, wantCommits)
	}
	out, err := repo.Run("ls-tree", "-r", "-z", tip)
	if err != nil {
		return commits, 0, err
	}
	var oids []string
	for _, rec := range strings.Split(out, "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		if len(f) == 3 && f[1] == "blob" && !Exempt(rec[tab+1:]) {
			oids = append(oids, f[2])
		}
	}
	bl, err := repo.BlobContents(oids)
	if err != nil {
		return commits, 0, err
	}
	bad := 0
	for _, b := range bl {
		if !IsHybridCiphertext(b) {
			bad++
		}
	}
	if bad > 0 {
		return commits, len(bl), fmt.Errorf("%d of %d blobs at origin/%s are not hybrid ciphertext", bad, len(bl), branch)
	}
	return commits, len(bl), nil
}

// DropBackup removes the pre-migration backup ref and purges what only it kept
// alive: the reflogs of HEAD, main, origin/main and the backup are expired and
// the object store pruned now. Other reflogs (the stash) are left alone.
func DropBackup(repo *gitwt.Repo) error {
	if SchemeOf(repo) != SchemePQ {
		return errors.New("this repo is not post-quantum; there is no backup to drop")
	}
	if _, err := repo.Run("rev-parse", "--verify", "--quiet", BackupRef); err != nil {
		return errors.New("no backup ref " + BackupRef)
	}
	if err := checkDropAllowed(repo); err != nil {
		return err
	}
	// Only the refs that held the old history: `--all` would also expire the
	// stash reflog (the daemon's autostash) and lose its entries.
	for _, ref := range []string{"HEAD", "refs/heads/main", "refs/remotes/origin/main"} {
		if _, err := repo.Run("reflog", "expire", "--expire=now", "--expire-unreachable=now", ref); err != nil {
			return err
		}
	}
	if _, err := repo.Run("update-ref", "-d", BackupRef); err != nil {
		return err
	}
	removeRekeyState(repo)
	_, err := repo.Run("gc", "-q", "--prune=now")
	return err
}
