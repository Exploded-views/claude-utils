package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/merge"
	"github.com/MarimerLLC/claude-utils/internal/version"
)

// claude-memmerge is a git custom merge driver for MEMORY.md files.
//
// Git invokes it as: claude-memmerge %O %A %B [%L [%P]]
//   %O = ancestor (base) version
//   %A = current branch / "ours" — must be overwritten with merged result
//   %B = "theirs"
//   %L = conflict marker length (optional)
//   %P = pathname of file in working tree (optional)
//
// Exit 0 = clean merge (file at %A path now contains merged result).
// Exit non-zero = conflict; file at %A path may contain conflict markers.
//
// See: https://git-scm.com/docs/gitattributes#_defining_a_custom_merge_driver

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version.Strings("claude-memmerge"))
			return
		case "help", "--help", "-h":
			usage()
			return
		}
	}

	args := os.Args[1:]
	textMode, unionMode := false, false
	if len(args) > 0 && args[0] == "--text" {
		textMode = true
		args = args[1:]
	} else if len(args) > 0 && args[0] == "--union" {
		unionMode = true
		args = args[1:]
	}
	if len(args) < 3 {
		usage()
		os.Exit(2)
	}

	basePath := args[0]
	oursPath := args[1]
	theirsPath := args[2]
	markerSize := 0
	if len(args) >= 4 {
		markerSize, _ = strconv.Atoi(args[3])
	}

	base, err := os.ReadFile(basePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read base:", err)
		os.Exit(2)
	}
	ours, err := os.ReadFile(oursPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read ours:", err)
		os.Exit(2)
	}
	theirs, err := os.ReadFile(theirsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read theirs:", err)
		os.Exit(2)
	}

	// In an age-encrypted repo git hands the driver ciphertext: decrypt the
	// three versions, merge, and write ciphertext back. Plaintext inputs (an
	// unencrypted repo) are merged and written as before.
	var mctx *agecrypt.MergeContext
	if agecrypt.IsCiphertext(ours) || agecrypt.IsCiphertext(theirs) || agecrypt.IsCiphertext(base) {
		var err error
		if mctx, err = agecrypt.OpenMergeContext("."); err != nil {
			fmt.Fprintln(os.Stderr, "memmerge: cannot decrypt:", err)
			os.Exit(2)
		}
	}
	fn := agecrypt.MergeFunc(func(b, o, t []byte) ([]byte, bool, error) {
		m, c := merge.Merge(string(b), string(o), string(t))
		return []byte(m), c, nil
	})
	if textMode {
		fn = agecrypt.TextMerge(markerSize)
	} else if unionMode {
		fn = agecrypt.UnionMerge()
	}
	merged, conflict, err := mctx.Merge(fn, base, ours, theirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memmerge:", err)
		os.Exit(2)
	}

	if err := os.WriteFile(oursPath, merged, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write ours:", err)
		os.Exit(2)
	}

	if conflict {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `claude-memmerge — git custom merge driver for MEMORY.md

Usage:
  claude-memmerge [--text|--union] <base> <ours> <theirs> [marker-len] [pathname]

--text merges plain lines (git merge-file) instead of MEMORY.md sections;
--union does the same but keeps both sides' lines and never conflicts.
In an age-encrypted repo both modes decrypt the inputs and write ciphertext.

This binary is invoked by git via merge.<name>.driver configuration; not
typically run by hand.`)
}
