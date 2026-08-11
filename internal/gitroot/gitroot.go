package gitroot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// gitDirName is the one directory entry every git working tree carries: a
// directory in a primary checkout, a file holding a `gitdir:` pointer in a
// linked worktree or a submodule.
const gitDirName = ".git"

// pointerFileLimit bounds the three tiny files this package reads — a linked
// worktree's `.git` pointer, the `commondir` beside its private gitdir, and a
// registration's `gitdir`. Each holds one short path; anything larger is
// malformed, and the bound is what keeps a resolver that runs once per
// session cwd from reading whatever happens to sit under that name.
const pointerFileLimit = 4 << 10

// MainRoot returns the working-tree root of the MAIN checkout of the
// repository dir belongs to — git's `--git-common-dir` semantics, not
// `--show-toplevel`'s.
//
// The distinction is the whole point: `git rev-parse --show-toplevel` run
// inside a LINKED WORKTREE answers with the worktree's own root, so resolving
// a project that way gives every worktree a project of its own. A project is
// the repository; a worktree is a place the repository is checked out (root
// AGENTS.md, core principle 7), and this function answers the first question.
//
// ok is false when dir is not inside a repository, and when dir does not
// exist. A path that is not there cannot be walked, and walking up from it
// lexically would attribute a deleted worktree to whatever repository happens
// to contain its parent — a dotfiles repo in $HOME is enough to make that
// wrong. Callers that need a deleted worktree resolved ask
// RegisteredWorktrees instead, which answers from the repository side.
func MainRoot(dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", false
	}
	current := canonical(dir)
	for {
		if root, ok := rootAt(current); ok {
			return root, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
		current = parent
	}
}

// RegisteredWorktrees returns the working-tree paths git has registered
// against the repository at root, read out of `<commonDir>/worktrees/*/gitdir`.
//
// This is the only way to place a DELETED worktree: the directory is gone, so
// nothing can be walked up from it, but the repository still holds its
// registration until someone prunes it.
//
// The returned paths are cleaned but NOT symlink-resolved — they are usually
// gone, so there is nothing to resolve, and the lexical form is what both git
// and the provider that ran there recorded. A repository with no worktrees
// (and a root that is not a checkout at all) yields no paths and no error;
// only a registry that exists and cannot be read is an error.
func RegisteredWorktrees(root string) ([]string, error) {
	commonDir, ok := commonDirOfCheckout(strings.TrimSpace(root))
	if !ok {
		return nil, nil
	}
	dir := filepath.Join(commonDir, "worktrees")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gitroot: read %s: %w", dir, err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		registration := filepath.Join(dir, entry.Name())
		pointer, ok := readPointer(filepath.Join(registration, "gitdir"))
		if !ok {
			continue
		}
		pointer = strings.TrimSpace(pointer)
		if pointer == "" {
			continue
		}
		// git ≥2.48 can write the registration RELATIVE to the directory
		// holding it (`worktree add --relative-paths`,
		// `worktree.useRelativePaths`) — the same rule the `.git` pointer and
		// `commondir` files follow. Left relative it could never match an
		// absolute session cwd, so every deleted worktree of such a repository
		// would silently lose its grouping.
		if !filepath.IsAbs(pointer) {
			pointer = filepath.Join(registration, pointer)
		}
		// The file holds the path of the worktree's own `.git` FILE, so the
		// worktree is its parent. A registration naming anything else is
		// corrupt and is skipped rather than guessed at.
		if filepath.Base(pointer) != gitDirName {
			continue
		}
		paths = append(paths, filepath.Clean(filepath.Dir(pointer)))
	}
	return paths, nil
}

// rootAt answers "is current a working-tree root, and if so, whose main one".
// ok is false only when current holds no `.git` entry at all; every other
// answer ends the walk here.
func rootAt(current string) (string, bool) {
	gitDir, status := gitDirAt(current)
	if status == gitDirAbsent {
		return "", false
	}
	if status == gitDirDirectory {
		// A primary checkout: `.git` IS the common dir, so this is the main
		// working tree.
		return current, true
	}
	if status == gitDirPointer {
		if root, ok := mainRootFromGitDir(current, gitDir); ok {
			return root, true
		}
	}
	// Four layouts land here and the checkout in hand is the root of all of
	// them. A submodule's gitdir (`<super>/.git/modules/<name>`) is a
	// repository in its own right and carries no commondir; a worktree of a
	// bare or submodule repository has no main working tree to name; a gitdir
	// the repository never confirmed is not a worktree of anything (see
	// registrationNamesWorkTree). In those three `git rev-parse
	// --show-toplevel` answers the same. The fourth is an UNUSABLE `.git`
	// entry, where git itself stops and errors rather than searching upward —
	// walking past it is the false attribution the missing-path refusal exists
	// to prevent, since the next ancestor holding a repository (a dotfiles
	// repo in $HOME) would adopt the checkout.
	return current, true
}

// gitDirStatus is what the `.git` entry of a candidate checkout turned out to
// be. The two failure states are deliberately distinct: an ABSENT entry means
// the walk continues upward, an UNUSABLE one means it stops.
type gitDirStatus int

const (
	gitDirAbsent gitDirStatus = iota
	gitDirUnusable
	gitDirDirectory
	gitDirPointer
)

// gitDirAt resolves the git directory of the checkout at workTree.
func gitDirAt(workTree string) (string, gitDirStatus) {
	path := filepath.Join(workTree, gitDirName)
	// Stat, not Lstat: a `.git` symlink to a real git directory is still a
	// primary checkout.
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", gitDirAbsent
	}
	if err != nil {
		// Something is there and cannot be read. "No repository here" would be
		// a guess, and the ancestor it hands the checkout to would be wrong.
		return "", gitDirUnusable
	}
	if info.IsDir() {
		return path, gitDirDirectory
	}
	if !info.Mode().IsRegular() {
		return "", gitDirUnusable
	}
	gitDir, ok := gitDirFromPointer(workTree, path)
	if !ok {
		return "", gitDirUnusable
	}
	return gitDir, gitDirPointer
}

// gitDirFromPointer reads the `gitdir: <path>` line out of a linked
// worktree's (or submodule's) `.git` file. A relative pointer is resolved
// against the working tree that holds it, which is how git writes it.
func gitDirFromPointer(workTree, pointerPath string) (string, bool) {
	body, ok := readPointer(pointerPath)
	if !ok {
		return "", false
	}
	for line := range strings.SplitSeq(body, "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		if !found {
			continue
		}
		if value = strings.TrimSpace(value); value == "" {
			return "", false
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(workTree, value)
		}
		return filepath.Clean(value), true
	}
	return "", false
}

// mainRootFromGitDir turns a linked worktree's private gitdir into the main
// checkout's working-tree root. ok is false when the layout names no main
// working tree, which the caller reads as "this checkout is its own root".
//
// It resolves nothing until the repository has CONFIRMED workTree. Both
// resolutions below run on content anybody who can write a directory can
// author — a `.git` pointer plus a `commondir` is enough to name an arbitrary
// path as a main root, and the pre-2.13 fallback needs only the `.git` pointer
// — and the answer becomes an auto-created project row at that path.
func mainRootFromGitDir(workTree, gitDir string) (string, bool) {
	if !registrationNamesWorkTree(gitDir, workTree) {
		return "", false
	}
	if commonDir, ok := commonDirBeside(gitDir); ok {
		if !commonDirHoldsGitDir(commonDir, gitDir) {
			return "", false
		}
		// The parent of a `.git` DIRECTORY is its working tree. A common dir
		// spelled anything else is a bare repository (`repo.git`) or a
		// submodule's own git dir (`<super>/.git/modules/<name>`); neither
		// has a main working tree, and its parent is not one.
		if filepath.Base(commonDir) == gitDirName {
			return canonical(filepath.Dir(commonDir)), true
		}
		return "", false
	}
	// Git before 2.13 wrote no commondir file. A linked worktree's private
	// gitdir is `<mainRoot>/.git/worktrees/<name>`, so the segment before
	// `/.git/worktrees/` is the main root.
	sep := string(filepath.Separator)
	marker := sep + gitDirName + sep + "worktrees" + sep
	if idx := strings.Index(gitDir, marker); idx > 0 {
		return canonical(gitDir[:idx]), true
	}
	return "", false
}

// registrationNamesWorkTree reports whether the repository holding gitDir
// registered workTree. Git maintains the link in BOTH directions: the
// worktree's `.git` file names its private gitdir, and the registration
// carries a `gitdir` file naming that same worktree's `.git` file back. Only
// the repository writes the second half, so a private gitdir whose
// back-pointer names something else was never registered by the repository it
// claims to belong to.
//
// A missing or mismatched back-pointer is not an error: the caller reads it as
// "this checkout is its own root", the same answer a bare or submodule layout
// gets. That is also the honest answer for a worktree someone moved by hand,
// which is precisely the state `git worktree repair` exists to fix.
func registrationNamesWorkTree(gitDir, workTree string) bool {
	body, ok := readPointer(filepath.Join(gitDir, "gitdir"))
	if !ok {
		return false
	}
	value := strings.TrimSpace(body)
	if value == "" || filepath.Base(value) != gitDirName {
		return false
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(gitDir, value)
	}
	// Both sides canonicalized: the recorded path and the walked one are two
	// spellings of one directory whenever a symlink is in play (macOS `/tmp`
	// is `/private/tmp`), and a lexical comparison would refuse every real
	// worktree under one.
	return canonical(filepath.Dir(value)) == canonical(workTree)
}

// commonDirHoldsGitDir reports whether commonDir is the shared git directory
// that physically holds gitDir. Every layout git writes keeps a linked
// worktree's private gitdir at `<commonDir>/worktrees/<name>` (the commondir
// file is literally `../..`), so a commondir naming anywhere else was not
// written by git — it is the one redirect the back-pointer check cannot see,
// because an author who controls the registration directory controls both the
// back-pointer AND the commondir beside it. Bound this way, a resolved main
// root can only ever be a path that physically contains the registration, and
// whoever can write there owns that path already.
//
// Canonical on both sides: the recorded value and the physical location are
// two spellings of one directory whenever a symlink is in play.
func commonDirHoldsGitDir(commonDir, gitDir string) bool {
	return canonical(commonDir) == canonical(filepath.Dir(filepath.Dir(gitDir)))
}

// commonDirBeside reads the `commondir` file git writes next to a linked
// worktree's private gitdir. Its content points at the repository's shared
// git directory, relative to the gitdir itself in every git that writes it.
func commonDirBeside(gitDir string) (string, bool) {
	body, ok := readPointer(filepath.Join(gitDir, "commondir"))
	if !ok {
		return "", false
	}
	value := strings.TrimSpace(body)
	if value == "" {
		return "", false
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(gitDir, value)
	}
	return filepath.Clean(value), true
}

// commonDirOfCheckout resolves the SHARED git directory of the checkout at
// root — the one holding `worktrees/`. Primary checkouts answer with their
// own `.git`; a root that is itself a linked worktree answers with the main
// repository's, so a project row pointing at a worktree still enumerates the
// repository's registrations.
func commonDirOfCheckout(root string) (string, bool) {
	if root == "" {
		return "", false
	}
	gitDir, status := gitDirAt(root)
	if status == gitDirAbsent || status == gitDirUnusable {
		return "", false
	}
	if status == gitDirDirectory {
		return gitDir, true
	}
	// Same guards rootAt applies, for the same reason: a `commondir` beside an
	// unconfirmed gitdir (or one naming a directory that does not hold it)
	// points at whichever repository its author chose, and this answer decides
	// whose registrations are enumerated. Unconfirmed, the private gitdir
	// stands for itself — it holds no `worktrees/`, so the caller sees no
	// registrations, which is also what a pre-2.13 layout with no commondir
	// gets.
	if registrationNamesWorkTree(gitDir, root) {
		if commonDir, ok := commonDirBeside(gitDir); ok && commonDirHoldsGitDir(commonDir, gitDir) {
			return commonDir, true
		}
	}
	return gitDir, true
}

// readPointer reads one of the small pointer files above. An unreadable or
// absent file is a legitimate answer here — "this layout does not have one" —
// so it reports ok=false rather than an error; every caller has a defined
// behaviour for the absent case.
func readPointer(path string) (string, bool) {
	// Stat, not Lstat, and the same regular-file screen gitDirAt applies to
	// the `.git` entry: a symlink to a real file is still one of these
	// pointers, while a FIFO named `commondir` or `gitdir` would block the
	// open forever — and this resolver runs inside a session-import scan with
	// no cancel path.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, pointerFileLimit))
	if err != nil {
		return "", false
	}
	return string(body), true
}

// canonical resolves symlinks when it can and falls back to the lexically
// cleaned absolute path. Resolution matters because AO's project rows are
// stamped with canonical roots (macOS `/tmp` is `/private/tmp`), and the
// fallback matters because a path derived from a pointer file may name a
// directory that no longer exists.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}
