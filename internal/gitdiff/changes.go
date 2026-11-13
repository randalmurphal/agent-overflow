package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ChangedFile is one uncommitted change of a workspace against HEAD: what
// committing the file whole would record.
type ChangedFile struct {
	Path string `json:"path"`
	// OldPath is the path a renamed file had on HEAD. Committing the rename
	// takes both paths.
	OldPath string `json:"oldPath,omitempty"`
	// Status is git's change letter: A, M, D, R or T.
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	// Binary reports a change git does not count lines for.
	Binary bool `json:"binary,omitempty"`
	// Version identifies the working-tree side of the change (its mode and
	// blob). It differs whenever the file's uncommitted content does, line
	// counts aside.
	Version string `json:"version"`
	// PartlyStaged reports that the user's index holds some of this file's
	// change and the working tree holds more.
	PartlyStaged bool `json:"partlyStaged,omitempty"`
}

// WorktreeSnapshot is a workspace's uncommitted state at one moment: the
// change list and the patches read from it describe the same tree. It holds
// temporary objects under the caller's root until Close; diffs it opens end
// with it. The user's index is read, never written.
type WorktreeSnapshot struct {
	workspace string
	tree      syntheticWorktreeTree
	oldSide   string
}

// CaptureWorktree snapshots everything uncommitted in the workspace: tracked
// changes against HEAD plus untracked-not-ignored files.
func CaptureWorktree(ctx context.Context, workspace, tempRoot string) (*WorktreeSnapshot, error) {
	tree, err := captureSyntheticWorktreeTree(ctx, workspace, tempRoot)
	if err != nil {
		return nil, err
	}
	oldSide, err := tree.oldSide(ctx, workspace)
	if err != nil {
		return nil, errors.Join(err, tree.release())
	}
	return &WorktreeSnapshot{workspace: workspace, tree: tree, oldSide: oldSide}, nil
}

// Close removes the snapshot's temporary objects.
func (s *WorktreeSnapshot) Close() error {
	return s.tree.release()
}

// Changes lists the snapshot's changes ordered by path. A rename is one
// entry.
func (s *WorktreeSnapshot) Changes(ctx context.Context) ([]ChangedFile, error) {
	stdout, _, _, err := runGit(ctx, s.workspace, s.tree.env, false,
		"diff", "-z", "--raw", "--numstat", "--no-abbrev", "--no-color", "--no-ext-diff", "--no-textconv",
		"--find-renames", "--no-relative", s.oldSide, s.tree.oid, "--")
	if err != nil {
		return nil, fmt.Errorf("gitdiff: list worktree changes: %w", err)
	}
	files, err := parseRawNumstat(stdout)
	if err != nil || len(files) == 0 {
		return files, err
	}
	partly, err := partlyStagedPaths(ctx, s.workspace)
	if err != nil {
		return nil, err
	}
	for i := range files {
		_, files[i].PartlyStaged = partly[files[i].Path]
	}
	return files, nil
}

// Diff opens the snapshot's whole patch. Close it before the snapshot.
func (s *WorktreeSnapshot) Diff(opts Options) *Diff {
	return newDiff(s.workspace, s.tree.env, nil, opts.gitArgs("diff", s.oldSide, s.tree.oid, "--"))
}

// FilesDiff opens the snapshot's patch of the given paths, each matched
// literally. Pass both paths of a rename to read it as one. Close it before
// the snapshot.
func (s *WorktreeSnapshot) FilesDiff(paths []string, opts Options) (*Diff, error) {
	if err := validatePaths(paths); err != nil {
		return nil, err
	}
	args := append([]string{"--literal-pathspecs"}, opts.gitArgs("diff", s.oldSide, s.tree.oid, "--")...)
	return newDiff(s.workspace, s.tree.env, nil, append(args, paths...)), nil
}

// WorktreeChanges lists everything uncommitted in the workspace, from a
// snapshot it takes and removes.
func WorktreeChanges(ctx context.Context, workspace, tempRoot string) (files []ChangedFile, err error) {
	snapshot, err := CaptureWorktree(ctx, workspace, tempRoot)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	return snapshot.Changes(ctx)
}

// OpenWorktreeFilesDiff is OpenWorktreeDiff limited to the given paths, each
// matched literally (WorktreeFilesSides). The returned diff owns its
// snapshot.
func OpenWorktreeFilesDiff(ctx context.Context, workspace, tempRoot string, paths []string, opts Options) (*Diff, error) {
	sides, err := WorktreeFilesSides(ctx, workspace, tempRoot, paths)
	if err != nil {
		return nil, err
	}
	return sides.OpenDiff(opts), nil
}

func validatePaths(paths []string) error {
	if len(paths) == 0 {
		return errors.New("gitdiff: at least one path is required")
	}
	for _, p := range paths {
		if p == "" || strings.ContainsRune(p, 0) {
			return fmt.Errorf("gitdiff: invalid path %q", p)
		}
	}
	return nil
}

// oldSide is what a worktree diff compares the snapshot against: HEAD's
// commit, or the empty tree when HEAD has none.
func (s syntheticWorktreeTree) oldSide(ctx context.Context, workspace string) (string, error) {
	if s.head != "" {
		return s.head, nil
	}
	return emptyTreeOID(ctx, workspace, s.env)
}

// parseRawNumstat reads `git diff -z --raw --numstat`: every raw record
// (`:<modes> <oids> <status>` then one path, two for a rename), then every
// numstat record in the same order (`<adds>\t<dels>\t<path>`, or an empty
// path followed by the rename's two paths).
func parseRawNumstat(out string) ([]ChangedFile, error) {
	tokens := strings.Split(out, "\x00")
	if n := len(tokens); n > 0 && tokens[n-1] == "" {
		tokens = tokens[:n-1]
	}
	malformed := func(what string) ([]ChangedFile, error) {
		return nil, fmt.Errorf("gitdiff: malformed change list: %s", what)
	}
	var files []ChangedFile
	i := 0
	for i < len(tokens) && strings.HasPrefix(tokens[i], ":") {
		fields := strings.Fields(tokens[i])
		if len(fields) != 5 || fields[4] == "" {
			return malformed("raw record " + strconv.Quote(tokens[i]))
		}
		file := ChangedFile{Status: fields[4][:1], Version: fields[1] + ":" + fields[3]}
		paths := 1
		if file.Status == "R" || file.Status == "C" {
			paths = 2
		}
		if i+paths >= len(tokens) {
			return malformed("raw record without its path")
		}
		if paths == 2 {
			file.OldPath = tokens[i+1]
		}
		file.Path = tokens[i+paths]
		files = append(files, file)
		i += paths + 1
	}
	for n := range files {
		if i >= len(tokens) {
			return malformed("fewer line counts than files")
		}
		adds, rest, ok := strings.Cut(tokens[i], "\t")
		dels, path, ok2 := strings.Cut(rest, "\t")
		if !ok || !ok2 {
			return malformed("numstat record " + strconv.Quote(tokens[i]))
		}
		i++
		if path == "" {
			if i+1 >= len(tokens) {
				return malformed("renamed numstat record without its paths")
			}
			path = tokens[i+1]
			i += 2
		}
		if path != files[n].Path {
			return malformed("line counts for " + strconv.Quote(path) + " where " + strconv.Quote(files[n].Path) + " was listed")
		}
		if adds == "-" && dels == "-" {
			files[n].Binary = true
			continue
		}
		var err error
		if files[n].Additions, err = strconv.Atoi(adds); err != nil {
			return malformed("numstat record " + strconv.Quote(tokens[i-1]))
		}
		if files[n].Deletions, err = strconv.Atoi(dels); err != nil {
			return malformed("numstat record " + strconv.Quote(tokens[i-1]))
		}
	}
	if i != len(tokens) {
		return malformed("more line counts than files")
	}
	return files, nil
}

// partlyStagedPaths returns the paths whose change is split between the
// user's index and the working tree: staged against HEAD and changed again
// since. GIT_OPTIONAL_LOCKS=0 keeps both reads from refreshing that index.
func partlyStagedPaths(ctx context.Context, workspace string) (map[string]struct{}, error) {
	env := []string{"GIT_OPTIONAL_LOCKS=0"}
	names := func(args ...string) ([]string, error) {
		args = append([]string{"diff", "-z", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv"}, args...)
		stdout, _, _, err := runGit(ctx, workspace, env, false, args...)
		if err != nil {
			return nil, fmt.Errorf("gitdiff: list staged changes: %w", err)
		}
		return strings.Split(stdout, "\x00"), nil
	}
	staged, err := names("--cached")
	if err != nil {
		return nil, err
	}
	stagedSet := make(map[string]struct{}, len(staged))
	for _, p := range staged {
		if p != "" {
			stagedSet[p] = struct{}{}
		}
	}
	if len(stagedSet) == 0 {
		return nil, nil
	}
	unstaged, err := names()
	if err != nil {
		return nil, err
	}
	partly := map[string]struct{}{}
	for _, p := range unstaged {
		if _, ok := stagedSet[p]; ok {
			partly[p] = struct{}{}
		}
	}
	return partly, nil
}
