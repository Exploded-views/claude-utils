package gitwt

import (
	"os"
	"path/filepath"
	"testing"
)

// The fold must catch every spelling the file system treats as the same file,
// and only those (plus the documented over-approximation). Candidates are NFD
// forms, combining marks, compatibility forms (fullwidth, the U+FB01 ligature)
// and the case-fold aliases. On a case- and normalisation-insensitive file
// system (APFS) each candidate is also stat-checked against the real file, so
// "the FS aliases it" is a fact there, not an assumption.
func TestFoldPath_CatchesExactlyTheSpellingsTheFileSystemAliases(t *testing.T) {
	type cand struct {
		name  string
		alias bool // APFS resolves it to the per-PC file (the 39 aliases and plain case)
	}
	cases := map[string][]cand{
		"config.json": {
			{"CONFIG.JSON", true}, {"Config.Json", true},
			{"conﬁg.json", true},        // fi ligature: full case folding gives "fi"
			{"ｃonfig.json", false},      // fullwidth c: compatibility only, APFS keeps it apart
			{"cónfig.json", false},     // o + combining acute (NFD of an o-acute): a different name
			{"cónfig.json", false},      // the NFC form of the same
			{"confiǵ.json", false},     // a combining mark on g
			{"config.jsｏn", false},      // fullwidth o
			{"cǆnfig.json", false},      // dz digraph letter
			{"conﬂg.json", false},       // fl ligature: not in this name ("g" does not follow "fl")
			{"config.j\u017fon", true},  // long s inside json
			{"config.json.", false},     // trailing dot: an alias on Windows only
			{"config.jsoń", false},     // trailing combining mark
			{"K" + "onfig.json", false}, // Kelvin sign is k, not c
		},
		"denylist.txt": {
			{"denyliſt.txt", true}, // long s
			{"DENYLIſT.TXT", true},
			{"denylıst.txt", false}, // dotless i: not an alias on APFS (the fold over-approximates)
			{"dénylist.txt", false},
			{"ｄenylist.txt", false},
		},
		".state": {
			{".\ufb05ate", true}, {".\ufb06ate", true}, // the two st ligatures
			{".STATE", true},
			{".ｓtate", false},
		},
		"daemon.pid": {{"DAEMON.PID", true}, {"daemon.pıd", false}, {"dáemon.pid", false}},
	}
	// Spellings the fold deliberately over-approximates: refused although APFS
	// keeps them apart. Listed so the test says so instead of hiding it.
	over := map[string]bool{"config.json.": true, "denyl\u0131st.txt": true, "daemon.p\u0131d": true}

	dir := t.TempDir()
	for perPC := range cases {
		if perPC == ".state" {
			os.MkdirAll(filepath.Join(dir, perPC), 0o700)
		} else {
			os.WriteFile(filepath.Join(dir, perPC), nil, 0o600)
		}
	}
	for perPC, cs := range cases {
		if !LocalOnlyPath(perPC) {
			t.Fatalf("%q itself is not per-PC", perPC)
		}
		for _, c := range cs {
			got := LocalOnlyPath(c.name)
			_, err := os.Stat(filepath.Join(dir, c.name))
			onFS := err == nil && c.name != perPC
			switch {
			case onFS && !got:
				t.Errorf("%q resolves to the per-PC file on this file system but the fold misses it", c.name)
			case c.alias && !got:
				t.Errorf("%q is an APFS alias of %q but the fold misses it", c.name, perPC)
			case !c.alias && got && !over[c.name]:
				t.Errorf("%q is a different file from %q but the fold refuses it", c.name, perPC)
			}
		}
	}
}
