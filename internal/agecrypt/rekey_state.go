package agecrypt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// RekeyStateFile is, in the git dir, what `rekey-pq --apply` records before it
// changes anything, so a rerun can finish an interrupted migration and
// `rekey-pq --rollback` can put the classic values back. It holds public values
// only (paths, public keys, labels, commit ids), mode 0600.
const RekeyStateFile = "memsync-pre-pq.json"

// PriorConfig is the part of config.json that a rekey changes.
type PriorConfig struct {
	EncryptionScheme   string   `json:"encryptionScheme"`
	HostLabel          string   `json:"hostLabel"`
	RecoveryRecipients []string `json:"recoveryRecipients"`
}

// PreRekeyState is the restore record.
type PreRekeyState struct {
	Tip             string      `json:"tip"`             // HEAD before the rekey (the backup ref's commit)
	ClassicIdentity string      `json:"classicIdentity"` // memage.identity before
	NewIdentity     string      `json:"newIdentity"`
	NewRecipient    string      `json:"newRecipient"` // this host's hybrid public key
	Label           string      `json:"label"`
	Recovery        string      `json:"recovery"`
	FreshRoot       bool        `json:"freshRoot"`
	PriorScheme     string      `json:"priorScheme"`
	PriorSchemeSet  bool        `json:"priorSchemeSet"`
	Config          PriorConfig `json:"config"`
	// NewCommit is the migrated commit, recorded before HEAD moves to it. A
	// rerun that finds it an ancestor of origin/main knows the migration was
	// published and has since been synced on top of.
	NewCommit string `json:"newCommit,omitempty"`
	// Stage is how far the apply got (the RekeyRec* values). A record from an
	// older build has none.
	Stage string `json:"stage,omitempty"`
}

// The Stage values of the restore record, in the order an apply passes them.
const (
	RekeyRecStarted   = "started"   // the record is written; nothing is committed yet
	RekeyRecCommitted = "committed" // the migrated commit exists (HEAD moves to it next)
	RekeyRecMigrated  = "migrated"  // identity, scheme and pin are repointed; not pushed
	RekeyRecPublished = "published" // the remote holds it and config.json follows
	RekeyRecDone      = "done"
)

// Finished reports whether the record says the migration was published.
func (st PreRekeyState) Finished() bool {
	return st.Stage == RekeyRecPublished || st.Stage == RekeyRecDone
}

func rekeyStatePath(repo *gitwt.Repo) (string, error) {
	d, err := gitCommonDir(repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, RekeyStateFile), nil
}

func writeRekeyState(repo *gitwt.Repo, st PreRekeyState) error {
	p, err := rekeyStatePath(repo)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurably(p, append(b, '\n'))
}

// writeFileDurably writes data to path through a temp file that is fsynced,
// renamed over path, and then the directory is fsynced, so a power loss leaves
// the old record or the whole new one, never a torn file.
func writeFileDurably(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Best effort: a directory cannot be opened or synced on every platform.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// ErrRecordUnreadable marks a restore record that exists but cannot be parsed.
var ErrRecordUnreadable = errors.New("the restore record is unreadable")

func unreadableRecord(err error) error {
	return fmt.Errorf("%w (%s: %v): the pre-migration history is still at %s, so do NOT drop the backup; inspect the repo, then finish or undo the migration by hand (see the rekey section of docs/setup.md)", ErrRecordUnreadable, RekeyStateFile, err, BackupRef)
}

// ReadRekeyState reads the restore record; os.ErrNotExist when there is none.
func ReadRekeyState(repo *gitwt.Repo) (PreRekeyState, error) {
	var st PreRekeyState
	p, err := rekeyStatePath(repo)
	if err != nil {
		return st, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, unreadableRecord(err)
	}
	return st, nil
}

// UpdateRekeyState rewrites the restore record with fn applied.
func UpdateRekeyState(repo *gitwt.Repo, fn func(*PreRekeyState)) error {
	st, err := ReadRekeyState(repo)
	if err != nil {
		return err
	}
	fn(&st)
	return writeRekeyState(repo, st)
}

func removeRekeyState(repo *gitwt.Repo) {
	if p, err := rekeyStatePath(repo); err == nil {
		_ = os.Remove(p)
	}
}

// RekeyBlocksSync says whether the daemon must not sync repo now, and why: a
// rekey-pq restore record that is not finished (its stage is not published or
// done), one whose repo is not post-quantum yet, or one that cannot be read.
// A record from an older build (no stage) blocks only while the repo is not pq.
func RekeyBlocksSync(repo *gitwt.Repo) (string, bool) {
	st, err := ReadRekeyStateAt(repo.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		return "the rekey-pq restore record is unreadable; inspect it (the pre-migration history is at " + BackupRef + ")", true
	}
	scheme, _ := rawScheme(repo)
	switch {
	case scheme != SchemePQ:
		return "an unfinished rekey-pq left this repo before the post-quantum scheme (stage " + stageName(st) + "); finish it with `recipients rekey-pq --apply` or undo it with `--rollback`", true
	case st.Stage != "" && !st.Finished():
		return "an unfinished rekey-pq (stage " + stageName(st) + "); finish it with `recipients rekey-pq --apply` or undo it with `--rollback`", true
	}
	return "", false
}

func stageName(st PreRekeyState) string {
	if st.Stage == "" {
		return "unknown"
	}
	return st.Stage
}

// RekeyStage says how far an earlier rekey-pq --apply got.
type RekeyStage int

const (
	// StageFresh: nothing done; run the whole migration.
	StageFresh RekeyStage = iota
	// StageFinishLocal: HEAD moved, but identity/scheme/pin were not repointed.
	StageFinishLocal
	// StageRetryPush: migrated locally; the remote is still at the backup tip.
	StageRetryPush
	// StagePublished: migrated and the remote holds the migrated tip.
	StagePublished
	// StageDone: already post-quantum and no backup: nothing to do.
	StageDone
	// StageSynced: published, and the repo has moved on since (the daemon
	// synced on top of it): only the identity retirement is left.
	StageSynced
)

// rawScheme is memage.scheme as recorded, without SchemeOf's fallback.
func rawScheme(repo *gitwt.Repo) (string, bool) {
	out, err := repo.Run("config", "--get", SchemeConfigKey)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func revOf(repo *gitwt.Repo, rev string) (string, bool) {
	out, err := repo.Run("rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// DetectRekey classifies the repo for a rerun of rekey-pq --apply with opts o.
// A leftover of an attempt that stopped before it committed anything is
// cleaned up (StageFresh). It fetches origin/main when the stage depends on the
// remote. Flags must match those of the interrupted run.
func DetectRekey(repo *gitwt.Repo, o RekeyOpts) (RekeyStage, PreRekeyState, error) {
	var st PreRekeyState
	tip, hasBackup := revOf(repo, BackupRef)
	scheme, _ := rawScheme(repo)
	if !hasBackup {
		if scheme == SchemePQ {
			return StageDone, st, nil
		}
		return StageFresh, st, nil
	}
	st, err := ReadRekeyState(repo)
	if errors.Is(err, ErrRecordUnreadable) {
		return StageFresh, st, err
	}
	if err != nil {
		return StageFresh, st, fmt.Errorf("a backup ref %s exists but its restore record %s does not (%v); inspect the repo, then drop the backup with `recipients rekey-pq --drop-backup` or delete the ref", BackupRef, RekeyStateFile, err)
	}
	if st.Tip != tip {
		return StageFresh, st, fmt.Errorf("%s does not match the restore record; inspect the repo by hand", BackupRef)
	}
	if scheme == SchemePQ && st.Finished() {
		return StageSynced, st, nil
	}
	if scheme == SchemePQ && st.Stage == "" && st.NewCommit == "" {
		// A record from an older build: it never said which commit was the
		// migrated one. If origin/main has moved off the backup, work it out;
		// that commit being on origin/main means the migration was published.
		if _, err := repo.Run("fetch", "-q", "origin", "main"); err != nil {
			return StageFresh, st, fmt.Errorf("cannot fetch origin: %w", err)
		}
		if remote, _ := revOf(repo, "refs/remotes/origin/main"); remote != "" && remote != tip {
			if mc, ok := InferMigrated(repo, st, "refs/remotes/origin/main"); ok {
				st.NewCommit, st.Stage = mc, RekeyRecPublished
				if err := writeRekeyState(repo, st); err != nil {
					return StageFresh, st, err
				}
				return StageSynced, st, nil
			}
		}
	}
	head, _ := revOf(repo, "HEAD")
	if scheme != SchemePQ && head == tip {
		// An attempt that died before it committed: put the recipients file
		// back and start over.
		if _, err := repo.Run("checkout", "HEAD", "--", RecipientsFile); err != nil {
			return StageFresh, st, err
		}
		_, _ = repo.Run("update-ref", "-d", BackupRef)
		removeRekeyState(repo)
		return StageFresh, PreRekeyState{}, nil
	}
	if o.FreshRoot != st.FreshRoot || strings.TrimSpace(o.Label) != st.Label || strings.TrimSpace(o.Recovery) != st.Recovery {
		return StageFresh, st, errors.New("an earlier rekey-pq --apply was interrupted; rerun it with the same --host, --recovery-recipient and --fresh-root (or undo it with --rollback)")
	}
	if scheme != SchemePQ {
		if err := checkMigratedRecipients(repo, st); err != nil {
			return StageFresh, st, err
		}
		return StageFinishLocal, st, nil
	}
	if _, err := repo.Run("fetch", "-q", "origin", "main"); err != nil {
		return StageFresh, st, fmt.Errorf("cannot fetch origin: %w", err)
	}
	remote, _ := revOf(repo, "refs/remotes/origin/main")
	if remote == tip {
		// Every stage that goes on to push repoints first (FinishRekeyLocal),
		// which re-pins the work-tree's recipients: they must be the migrated set.
		if err := checkMigratedRecipients(repo, st); err != nil {
			return StageFresh, st, err
		}
		return StageRetryPush, st, nil
	}
	if remote == head && (st.NewCommit == "" || head == st.NewCommit) {
		if err := checkMigratedRecipients(repo, st); err != nil {
			return StageFresh, st, err
		}
		return StagePublished, st, nil
	}
	// The migrated commit is on the remote, which has moved on since (the
	// daemon synced): the migration is done.
	if st.NewCommit != "" {
		if _, err := repo.Run("merge-base", "--is-ancestor", st.NewCommit, "refs/remotes/origin/main"); err == nil {
			return StageSynced, st, nil
		}
	}
	if remote == head {
		if err := checkMigratedRecipients(repo, st); err != nil {
			return StageFresh, st, err
		}
		return StagePublished, st, nil
	}
	return StageFresh, st, errors.New("origin/main is neither the backup tip nor the migrated tip (someone pushed since?); not touching it. Inspect it, or drop the backup if the migration is complete")
}

// InferMigrated names the migrated commit of a record that does not say (one
// from an older build), as found in the history of target (origin/main or
// HEAD). With a fresh root it is the single root of target when that differs
// from the roots of the backup tip; otherwise the first commit after the tip on
// target's ancestry path, which must be a child of the tip.
func InferMigrated(repo *gitwt.Repo, st PreRekeyState, target string) (string, bool) {
	if st.NewCommit != "" {
		return st.NewCommit, true
	}
	if st.Tip == "" {
		return "", false
	}
	lines := func(args ...string) []string {
		out, err := repo.Run(args...)
		if err != nil {
			return nil
		}
		return strings.Fields(out)
	}
	if st.FreshRoot {
		roots := lines("rev-list", "--max-parents=0", target)
		if len(roots) != 1 {
			return "", false
		}
		for _, r := range lines("rev-list", "--max-parents=0", st.Tip) {
			if r == roots[0] {
				return "", false
			}
		}
		return roots[0], true
	}
	l := lines("rev-list", "--reverse", "--ancestry-path", st.Tip+".."+target)
	if len(l) == 0 {
		return "", false
	}
	if parent, ok := revOf(repo, l[0]+"^"); !ok || parent != st.Tip {
		return "", false
	}
	return l[0], true
}

// checkMigratedRecipients says whether .age-recipients is the migrated set
// (this host's new key and the recovery key, nothing else).
func checkMigratedRecipients(repo *gitwt.Repo, st PreRekeyState) error {
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return err
	}
	want := canonSet([]string{st.NewRecipient, st.Recovery})
	have := canonSet(cur)
	bad := errors.New("an earlier rekey-pq left the repo half-migrated and .age-recipients is not the migrated set; inspect it by hand")
	if len(have) != len(want) {
		return bad
	}
	for k := range want {
		if !have[k] {
			return bad
		}
	}
	return nil
}

// FinishRekeyLocal completes a migration whose HEAD moved but whose identity,
// scheme and pin were not repointed (a crash in between). It is idempotent and
// runs on every resume that goes on to push.
func FinishRekeyLocal(repo *gitwt.Repo, st PreRekeyState) (RekeyPlan, error) {
	if err := resetIndexToHead(repo); err != nil {
		return RekeyPlan{}, err
	}
	if err := repointLocal(repo, st.NewIdentity); err != nil {
		return RekeyPlan{}, err
	}
	return PlanFromState(repo, st), nil
}

// resetIndexToHead puts the index back on HEAD's tree when they differ. A
// crash between the HEAD move and the index update of the rekey commit leaves
// the classic blob ids in the index; nothing else is staged then (the apply
// required a clean work-tree and the daemon is stopped).
func resetIndexToHead(repo *gitwt.Repo) error {
	if _, err := repo.Run("diff-index", "--cached", "--quiet", "HEAD"); err == nil {
		return nil
	}
	if _, err := repo.Run("read-tree", "HEAD"); err != nil {
		return fmt.Errorf("reset the index to HEAD: %w", err)
	}
	return nil
}

func repointLocal(repo *gitwt.Repo, newIdentity string) error {
	if err := repo.ConfigSet(IdentityConfigKey, filepath.ToSlash(newIdentity)); err != nil {
		return err
	}
	if err := SetScheme(repo, SchemePQ); err != nil {
		return err
	}
	return PinCurrent(repo)
}

// PlanFromState describes a migration that is already done locally.
func PlanFromState(repo *gitwt.Repo, st PreRekeyState) RekeyPlan {
	head, _ := revOf(repo, "HEAD")
	return RekeyPlan{Tip: st.Tip, NewCommit: head, NewRecipient: st.NewRecipient, IdentityPath: st.ClassicIdentity}
}

// RetiredSuffix is what a retired classic identity file is renamed to.
const RetiredSuffix = ".classic-retired"

// restoreClassicIdentityFile makes sure the classic identity a rollback
// restores can be loaded, renaming <id>.classic-retired back if that is where
// it went. Without it the rolled-back repo could not be read by anyone.
func restoreClassicIdentityFile(path string) error {
	if path == "" {
		return errors.New("the restore record names no classic identity; refusing to roll back")
	}
	p := filepath.FromSlash(path)
	if _, err := os.Stat(p); err != nil {
		if _, rerr := os.Stat(p + RetiredSuffix); rerr != nil {
			return fmt.Errorf("the classic identity %s is missing, and so is %s: a rollback would leave the classic repo unreadable; put the classic identity file back first (from your offline backup), then rerun", path, filepath.Base(p)+RetiredSuffix)
		}
		if err := os.Rename(p+RetiredSuffix, p); err != nil {
			return fmt.Errorf("cannot rename %s back: %w", filepath.Base(p)+RetiredSuffix, err)
		}
	}
	if _, err := LoadIdentities(p); err != nil {
		return fmt.Errorf("the classic identity %s cannot be loaded: %w", path, err)
	}
	return nil
}

// ReadRekeyStateAt reads the restore record of the clone at dir (its .git
// directory); os.ErrNotExist when there is none.
func ReadRekeyStateAt(dir string) (PreRekeyState, error) {
	var st PreRekeyState
	b, err := os.ReadFile(filepath.Join(dir, ".git", RekeyStateFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, unreadableRecord(err)
	}
	return st, nil
}

// RollbackOpts says what a rollback may do beyond the plain undo.
type RollbackOpts struct {
	// DiscardLocalCommits lets the rollback drop commits made after the
	// rekey (HEAD past the migrated commit). Without it they are refused.
	DiscardLocalCommits bool
}

// maxListedSubjects bounds the commit subjects a refusal prints.
const maxListedSubjects = 10

// localCommitsError lists the commits a rollback would drop: their count and
// subjects, nothing else.
func localCommitsError(repo *gitwt.Repo, st PreRekeyState) error {
	if st.NewCommit == "" {
		return errors.New("the restore record does not name the migrated commit (an older build wrote it), so commits made after the rekey cannot be told apart; if there are none, rerun with --discard-local-commits")
	}
	out, _ := repo.Run("log", "--format=%s", st.NewCommit+"..HEAD")
	var subs []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			subs = append(subs, strings.Map(func(r rune) rune {
				if r < 0x20 || r == 0x7f {
					return -1
				}
				return r
			}, l))
		}
	}
	noun := "commits"
	if len(subs) == 1 {
		noun = "commit"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HEAD is not the migrated commit: %d %s made since the rekey would be dropped by a rollback:", len(subs), noun)
	for i, l := range subs {
		if i == maxListedSubjects {
			fmt.Fprintf(&b, "\n  ... and %d more", len(subs)-maxListedSubjects)
			break
		}
		fmt.Fprintf(&b, "\n  - %s", l)
	}
	b.WriteString("\nrerun with --discard-local-commits to drop them anyway")
	return errors.New(b.String())
}

// RollbackRekey puts a repo back that rekey-pq migrated locally but never
// published: the branch returns to the backup, and memage.identity,
// memage.scheme and the pin to their classic values. restoreConfig puts
// config.json back from the recorded values. It refuses when origin/main is no
// longer the backup tip. It returns the restore record.
func RollbackRekey(repo *gitwt.Repo, opts RollbackOpts, restoreConfig func(PriorConfig) error) (PreRekeyState, error) {
	st, err := ReadRekeyState(repo)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, fmt.Errorf("nothing to roll back: no restore record (%s)", RekeyStateFile)
		}
		return st, err
	}
	tip, ok := revOf(repo, BackupRef)
	if !ok || tip != st.Tip {
		return st, fmt.Errorf("nothing to roll back: %s is missing or does not match the restore record", BackupRef)
	}
	if cur, err := repo.CurrentBranch(); err != nil || cur != "main" {
		return st, errors.New("rollback works on the main branch")
	}
	if _, err := repo.Run("fetch", "-q", "origin", "main"); err != nil {
		return st, fmt.Errorf("cannot fetch origin to confirm the remote is unchanged: %w", err)
	}
	if remote, _ := revOf(repo, "refs/remotes/origin/main"); remote != tip {
		return st, errors.New("the remote has moved off the backup (the migrated history was pushed, or someone else pushed); a local rollback would diverge from it, so it is refused")
	}
	if head, _ := revOf(repo, "HEAD"); (st.NewCommit == "" || head != st.NewCommit) && !opts.DiscardLocalCommits {
		return st, localCommitsError(repo, st)
	}
	if repo.MergeInProgress() || repo.RebaseInProgress() || repo.HasUnmerged() {
		return st, errors.New("a merge or rebase is in progress")
	}
	if out, err := repo.Run("status", "--porcelain"); err != nil {
		return st, err
	} else if strings.TrimSpace(out) != "" {
		return st, errors.New("the work-tree has uncommitted changes; a rollback would discard them (refusing)")
	}
	if err := restoreClassicIdentityFile(st.ClassicIdentity); err != nil {
		return st, err
	}
	// The classic identity first: the reset below checks files out through the filter.
	if err := repo.ConfigSet(IdentityConfigKey, filepath.ToSlash(st.ClassicIdentity)); err != nil {
		return st, err
	}
	if st.PriorSchemeSet {
		if err := repo.ConfigSet(SchemeConfigKey, st.PriorScheme); err != nil {
			return st, err
		}
	} else {
		_, _ = repo.Run("config", "--unset", SchemeConfigKey)
	}
	if _, err := repo.Run("reset", "--hard", "-q", BackupRef); err != nil {
		return st, err
	}
	if err := PinCurrent(repo); err != nil {
		return st, err
	}
	if restoreConfig != nil {
		if err := restoreConfig(st.Config); err != nil {
			return st, err
		}
	}
	if _, err := repo.Run("update-ref", "-d", BackupRef); err != nil {
		return st, err
	}
	removeRekeyState(repo)
	return st, nil
}
