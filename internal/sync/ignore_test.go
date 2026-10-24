package sync

import "testing"

func TestShouldIgnoreFile(t *testing.T) {
	ignore := []string{
		"",                                // empty
		".hidden",                         // dot-prefixed
		".gitignore",                      // dot-prefixed
		"MEMORY.md.tmp",                   // trailing .tmp
		"MEMORY.md.tmp.7368.beeb3e905e2a", // <name>.tmp.<pid>.<hash>
		"feedback_mingw_kubectl.md.tmp.27244.fac4861e524", // same, real-world shape
		"release_process.md.tmp.47284.4cad.from-remote-1", // temp + conflict-backup suffix
	}
	for _, n := range ignore {
		if !shouldIgnoreFile(n, false) {
			t.Errorf("shouldIgnoreFile(%q) = false, want true", n)
		}
	}

	keep := []string{
		"MEMORY.md",
		"feedback_mingw_kubectl.md",
		"reference-meai-cached-tokens.md",
		"project_routing_log_missing_tokens.md",
	}
	for _, n := range keep {
		if shouldIgnoreFile(n, false) {
			t.Errorf("shouldIgnoreFile(%q) = true, want false", n)
		}
	}
}

// Conflict-backup names are ignored in remote mode only; path mode treats them
// as it did on main.
func TestShouldIgnoreFile_BackupNamesOnlyInRemoteMode(t *testing.T) {
	for _, n := range []string{"a.md.from-remote-1", "a.md.local-before-join-2", "notes.from-remote-team.md"} {
		if !shouldIgnoreFile(n, true) {
			t.Errorf("remote: shouldIgnoreFile(%q) = false, want true", n)
		}
		if shouldIgnoreFile(n, false) {
			t.Errorf("path: shouldIgnoreFile(%q) = true, want false (as on main)", n)
		}
	}
	if shouldIgnoreFile("MEMORY.md", true) {
		t.Error("remote: MEMORY.md must not be ignored")
	}
}
