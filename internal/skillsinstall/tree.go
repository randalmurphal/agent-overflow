// Package skillsinstall installs a shipped skills tree into a provider home
// and removes it again (docs/specs/standing-agents.md, "Skills library").
// Each skill directory `<tree>/<name>` is copied to `<home>/skills/<name>`,
// flattened and never linked, and a manifest in `<home>/skills` names every
// file and skill directory Install wrote. Update replaces what the manifest
// names and Remove deletes only that, so a file the user put beside the
// skills is never touched.
package skillsinstall

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Tree is a shipped skills tree: its files, the skills they make up, and the
// digest that tells an installed copy whether it is current.
type Tree struct {
	fsys fs.FS
	// Version is the upstream version the tree was vendored at, and
	// Commit the upstream commit, both read from its UPSTREAM.md.
	Version string
	Commit  string
	// Digest covers every file's path and content, which decides its
	// mode. The port edits files without moving the upstream pin, so the
	// version alone cannot say whether an installed copy is the shipped one.
	Digest string
	Skills []string
	files  []treeFile
}

type treeFile struct {
	path       string // slash-separated, relative to the tree root
	executable bool
}

// skippedDir is a dependency directory a script's tooling creates beside the
// tree in a checkout. It is never part of what ships.
const skippedDir = "node_modules"

var (
	upstreamCommit  = regexp.MustCompile("at commit\\s+`([0-9a-f]{40})`")
	upstreamVersion = regexp.MustCompile(`\(version ([0-9][0-9A-Za-z.\-]*)\)`)
)

// LoadTree reads the tree rooted at fsys, whose top-level directories are the
// skills, and the upstream pin from upstream (UPSTREAM.md). A file is
// executable when it starts with `#!`: an embedded tree carries no modes.
func LoadTree(fsys fs.FS, upstream []byte) (Tree, error) {
	tree := Tree{fsys: fsys}
	commit := upstreamCommit.FindSubmatch(upstream)
	version := upstreamVersion.FindSubmatch(upstream)
	if commit == nil || version == nil {
		return Tree{}, errors.New("skills tree: UPSTREAM.md names no upstream commit and version")
	}
	tree.Commit, tree.Version = string(commit[1]), string(version[1])

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return Tree{}, fmt.Errorf("skills tree: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			tree.Skills = append(tree.Skills, entry.Name())
		}
	}
	sum := sha256.New()
	err = fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == skippedDir {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.Contains(name, "/") {
			return nil // only files inside a skill directory ship
		}
		executable, err := hashFile(fsys, name, sum)
		if err != nil {
			return err
		}
		tree.files = append(tree.files, treeFile{path: name, executable: executable})
		return nil
	})
	if err != nil {
		return Tree{}, fmt.Errorf("skills tree: %w", err)
	}
	if len(tree.files) == 0 {
		return Tree{}, errors.New("skills tree: no skill files")
	}
	tree.Digest = hex.EncodeToString(sum.Sum(nil))
	return tree, nil
}

// hashFile adds one file's path and content to sum and reports whether it
// is executable. WalkDir visits names in lexical order, so the digest does
// not depend on the filesystem's order.
func hashFile(fsys fs.FS, name string, sum io.Writer) (executable bool, err error) {
	file, err := fsys.Open(name)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	reader := bufio.NewReader(file)
	head, err := reader.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	executable = string(head) == "#!"
	if _, err := fmt.Fprintf(sum, "%s\x00", name); err != nil {
		return false, err
	}
	if _, err := io.Copy(sum, reader); err != nil {
		return false, err
	}
	_, err = sum.Write([]byte{0})
	return executable, err
}

// Files lists the tree's file paths, slash-separated and sorted.
func (t Tree) Files() []string {
	out := make([]string, len(t.files))
	for i, file := range t.files {
		out[i] = file.path
	}
	sort.Strings(out)
	return out
}

// skillOf is the skill directory a tree-relative path belongs to.
func skillOf(name string) string {
	skill, _, _ := strings.Cut(name, "/")
	return skill
}

// parentDirs lists every directory between a file and its skill directory,
// the skill directory included, deepest first.
func parentDirs(name string) []string {
	var dirs []string
	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		dirs = append(dirs, dir)
	}
	return dirs
}
