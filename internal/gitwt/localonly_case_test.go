package gitwt

import "testing"

// The per-PC names are matched the way a case- and normalisation-insensitive
// file system resolves them, so a spelling that lands on the same file is
// still per-PC.
func TestLocalOnlyPath_FoldedSpellings(t *testing.T) {
	yes := []string{
		"Config.json", "CONFIG.JSON", "config.JSON",
		"Daemon.pid", "DENYLIST.TXT", "Denylist.txt",
		".State/x", ".STATE/a/b", "Distilled/DISTILLED.MD", "DISTILLED/distilled.md",
		"X.TMP", "x.Tmp.1", "p/.F.md.AB12.TMP", "p/N.FROM-REMOTE-0123456789ABCDEF",
		// canonically or case-wise equivalent non-ASCII spellings
		"denyli\u017ft.txt",             // long s folds to s
		"Konfig.json",                   // not config: guard against a false hit below
		"denylist.txt​",                 // ignorable code point
		"conf​ig.json",                  // ignorable code point inside the name
		"conﬁg.json"[:0] + "conﬁg.json", // fi ligature
		".ſtate/x",                      // long s
		// trailing dots and spaces are dropped by Windows
		"config.json.", "config.json ", "config.json. .", ".state./x", "denylist.txt  ",
		// 8.3 short names
		"CONFIG~1.JSO", "config~1.jso", "DENYLI~1.TXT", "DAEMON~1.PID", "STATE~1/x", "DISTIL~1/DISTIL~1.MD",
	}
	no := []string{
		"Konfig.json", // K-onfig is not config
		"conf-ig.json", "config.jsonl", "projects/p/config.json", "p/Config.json",
		"MEMORY.md", "notes/Denylist.txt", ".statex/a", "Distilled/other.md",
		"p/ideas.TMP.md", "p/N.from-remote-ABC", "p/N.from-remote-notes",
		"config~x.json",
	}
	for _, p := range yes {
		if p == "Konfig.json" {
			continue
		}
		if !LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = true, want false", p)
		}
	}
}

// Code points a file system (git's own HFS check among them) ignores in a name
// do not make a different name, and a name is per-PC only on an exact match
// after folding.
func TestLocalOnlyPath_IgnorableCodePointsAndExactMatch(t *testing.T) {
	yes := []string{
		"config⁪.json", "config⁯.json", "config­.json", // deprecated format, soft hyphen
		"CONFIG‍.JSON", "config͏.json", "config️.json", "conf\U000E0001ig.json",
		"­config.json", "config.json⁫",
		"dénylist.txt"[:0] + "denylist.txt­",
		".st⁣ate/x", "distilled/DIST‌ILLED.md",
	}
	no := []string{
		"configs.json", "config.json.bak", "config.json.d/x", "xconfig.json", "config_.json",
		"cońfig.json", // a real accent is a different name, in NFC or NFD
		"cónfig.json", "denylist.txt.old", "p/config­.json",
	}
	for _, p := range yes {
		if !LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = true, want false", p)
		}
	}
}

// A per-PC name is refused as the first component of any path, file or
// directory: git removes an ignored file to make room for a directory of the
// same name, and replaces a directory with a file.
func TestLocalOnlyPath_PrefixForms(t *testing.T) {
	yes := []string{
		"config.json/x", "Config.json/x/y", "Daemon.pid/x", "DENYLIST.TXT/a",
		".state", ".State", ".STATE/", "distilled/DISTILLED.md/x", "Distilled/distilled.md/x/y",
		// any 8.3-shaped component at the root or under distilled/, whatever the stem
		"CO1A2B~1.JSO", "AB~3", "distilled/ZZ1234~1.MD", "AB~3/x",
	}
	no := []string{
		"projects/p/config.json/x", "configs/x", "config.jsonl/x", "distilled/other.md/x",
		"projects/p/memory/ab~1.md", "notes-1/x",
	}
	for _, p := range yes {
		if !LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = true, want false", p)
		}
	}
}
