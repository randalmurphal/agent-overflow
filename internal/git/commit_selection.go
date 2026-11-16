package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/gitdiff"
)

// CommitSelection names what a commit covers: every uncommitted change, or
// exactly the listed files as the caller showed them. One of the two is
// set, so a caller that lost its files gets an error, not a commit of
// everything.
type CommitSelection struct {
	// All covers every uncommitted change, untracked files included.
	All bool
	// Files are changes as gitdiff listed them (WorktreeChanges). Each must
	// still be that change, its Version included, when the commit reads
	// it; the commit records the content it checked.
	Files []gitdiff.ChangedFile
	// SnapshotRoot is the directory the commit keeps its snapshot of the
	// files in while it runs. Required with Files.
	SnapshotRoot string
}

// ErrCommitFilesChanged is a commit of listed files refused because one of
// them is no longer the change that was listed.
var ErrCommitFilesChanged = errors.New("the selected files changed since they were listed")

// maxNamedChangedFiles bounds the paths a CommitFilesChangedError names.
const maxNamedChangedFiles = 3

// CommitFilesChangedError names the listed files that changed. It wraps
// ErrCommitFilesChanged.
type CommitFilesChangedError struct{ Paths []string }

func (e *CommitFilesChangedError) Error() string {
	named := strings.Join(e.Paths[:min(len(e.Paths), maxNamedChangedFiles)], ", ")
	if more := len(e.Paths) - maxNamedChangedFiles; more > 0 {
		named = fmt.Sprintf("%s and %d more", named, more)
	}
	return fmt.Sprintf("%v (%s); review them and commit again", ErrCommitFilesChanged, named)
}

func (e *CommitFilesChangedError) Unwrap() error { return ErrCommitFilesChanged }

// Validate reports a selection that names nothing, names both forms, or
// holds a path git cannot take.
func (s CommitSelection) Validate() error {
	if s.All {
		if len(s.Files) > 0 {
			return errors.New("commit selection: files given with all")
		}
		return nil
	}
	if len(s.Files) == 0 {
		return errors.New("commit selection: no files selected")
	}
	if strings.TrimSpace(s.SnapshotRoot) == "" {
		return errors.New("commit selection: snapshot directory is required")
	}
	for _, file := range s.Files {
		if file.Path == "" || strings.ContainsRune(file.Path, 0) || strings.ContainsRune(file.OldPath, 0) {
			return fmt.Errorf("commit selection: invalid path %q", file.Path)
		}
	}
	return nil
}

// CommitSelected commits what the selection covers and returns the new
// commit SHA. All stages everything first (`git add -A`). Files commits
// exactly those files and leaves every other change as it is, including
// whatever the user staged.
//
// A Files selection is refused while CommitsWholeOnly: git refuses it during
// a merge or cherry-pick, and during a revert it would record the files and
// end the revert with the rest of it left staged.
func (c *Core) CommitSelected(cwd, subject, body string, selection CommitSelection) (string, error) {
	if strings.TrimSpace(subject) == "" {
		return "", fmt.Errorf("git commit subject is required")
	}
	if err := selection.Validate(); err != nil {
		return "", err
	}
	if selection.All {
		if err := c.StageAll(cwd); err != nil {
			return "", err
		}
		return c.Commit(cwd, subject, body)
	}
	if op := c.pendingOperation(cwd); pendingOperationCommitsWhole(op) {
		return "", fmt.Errorf("git commit: a %s is in progress and records every change; selected files cannot be committed on their own", op)
	}
	return c.commitFiles(cwd, subject, body, selection)
}

// indexEntry is one path a commit of files records: a mode and object, or a
// removal when mode is empty.
type indexEntry struct {
	mode, oid, path string
}

// commitFiles commits the listed files as one snapshot read them, after
// checking that snapshot still holds each as it was listed, so the content
// checked is the content committed. It builds the commit in a temporary
// index (HEAD plus the files), as `git commit --only` does, and then sets the
// user's index at the paths the commit recorded to what it holds there
// (recordCommittedEntries), whose other entries stay as they were. A failure
// before the commit leaves the user's index untouched.
func (c *Core) commitFiles(cwd, subject, body string, selection CommitSelection) (sha string, err error) {
	ctx := context.Background()
	scratch, err := os.MkdirTemp(selection.SnapshotRoot, "commit-")
	if err != nil {
		return "", fmt.Errorf("git commit: create scratch directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(scratch); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("git commit: remove scratch directory: %w", removeErr))
		}
	}()
	snapshot, err := gitdiff.CaptureWorktree(ctx, cwd, scratch)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	entries, err := c.listedEntries(ctx, cwd, snapshot, selection.Files, scratch)
	if err != nil {
		return "", err
	}

	index := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "index")}
	head, err := c.hasHead(cwd)
	if err != nil {
		return "", err
	}
	readTree := []string{"read-tree", "--empty"}
	if head {
		readTree = []string{"read-tree", "HEAD"}
	}
	if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, extraEnv: index, args: readTree}); err != nil {
		return "", err
	}
	if err := c.writeIndexEntries(cwd, index, entries); err != nil {
		return "", err
	}
	if err := c.commit(cwd, index, subject, body); err != nil {
		return "", err
	}
	stdout, _, err := c.Execute(cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git commit: committed, but the index was not updated for the committed files: %w", err)
	}
	sha = strings.TrimSpace(stdout)
	if err := c.recordCommittedEntries(cwd, sha, entries); err != nil {
		return "", fmt.Errorf("git commit: committed, but the index was not updated for the committed files: %w", err)
	}
	return sha, nil
}

// recordCommittedEntries sets the user's index at the paths commit sha
// recorded to what sha holds there, leaving every other entry as it was.
// Those paths are the selected ones (entries) and any other path a
// pre-commit hook changed in the temporary index, so a hook that formats and
// stages a file leaves the index agreeing with the commit rather than with
// the content listed before the hook ran.
//
// A path the hook changed that was not selected takes the commit's entry
// only while the user's index still holds the parent's entry there: an
// entry the user staged differs from the parent, and is kept.
func (c *Core) recordCommittedEntries(cwd, sha string, entries []indexEntry) error {
	selected := make(map[string]bool, len(entries))
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !selected[entry.path] {
			selected[entry.path] = true
			paths = append(paths, entry.path)
		}
	}
	changes, err := c.commitChanges(cwd, sha)
	if err != nil {
		return err
	}
	parent := make(map[string]string)
	var hooked []string
	for _, change := range changes {
		if !selected[change.path] {
			hooked = append(hooked, change.path)
			parent[change.path] = change.before
		}
	}
	if len(hooked) > 0 {
		staged, err := c.indexStageZero(cwd, hooked)
		if err != nil {
			return err
		}
		for _, path := range hooked {
			if current, ok := staged[path]; ok && current == parent[path] {
				paths = append(paths, path)
			}
		}
	}
	if len(paths) == 0 {
		// An empty pathspec list would reset the whole index.
		return errors.New("git commit: no committed paths to record in the index")
	}
	var pathspecs strings.Builder
	for _, path := range paths {
		pathspecs.WriteString(path)
		pathspecs.WriteByte(0)
	}
	_, _, err = c.executeSpec(commandSpec{binary: "git", cwd: cwd, stdin: pathspecs.String(),
		args: []string{"--literal-pathspecs", "reset", "--quiet", sha, "--pathspec-from-file=-", "--pathspec-file-nul"}})
	return err
}

// commitChange is one path a commit changed against its parent; before is
// the parent's "mode oid" there, or empty when the parent had no entry.
type commitChange struct {
	path, before string
}

// commitChanges lists the paths commit sha changed against its parent, or
// against nothing for a root commit, renames reported as removal and
// addition.
func (c *Core) commitChanges(cwd, sha string) ([]commitChange, error) {
	stdout, _, err := c.Execute(cwd, "diff-tree", "-r", "-z", "--no-renames", "--root", "--no-commit-id", sha)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("git diff-tree %s: malformed output", sha)
	}
	changes := make([]commitChange, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		// ":<old mode> <new mode> <old oid> <new oid> <status>"
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("git diff-tree %s: malformed entry %q", sha, fields[i])
		}
		change := commitChange{path: fields[i+1]}
		if strings.Trim(meta[0], "0") != "" {
			change.before = meta[0] + " " + meta[2]
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// indexStageZero returns the user's index entry, as "mode oid", at each of
// paths that has one. A path present only as a conflict stage maps to a
// value no tree entry has, and a missing path to empty.
func (c *Core) indexStageZero(cwd string, paths []string) (map[string]string, error) {
	args := append([]string{"--literal-pathspecs", "ls-files", "--stage", "-z", "--"}, paths...)
	stdout, _, err := c.Execute(cwd, args...)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(paths))
	staged := make(map[string]string, len(paths))
	for _, path := range paths {
		want[path] = true
		staged[path] = ""
	}
	for _, record := range strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		// "<mode> <oid> <stage>\t<path>"
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("git ls-files: malformed entry %q", record)
		}
		if !want[path] {
			continue // Under a directory the pathspec named.
		}
		if fields[2] != "0" {
			staged[path] = "conflict"
			continue
		}
		staged[path] = fields[0] + " " + fields[1]
	}
	return staged, nil
}

// listedEntries checks each listed file against the snapshot and returns
// the index entries committing them records, with each blob written to the
// repository's object store. A file that is not the listed change any more
// fails with *CommitFilesChangedError.
func (c *Core) listedEntries(ctx context.Context, cwd string, snapshot *gitdiff.WorktreeSnapshot, files []gitdiff.ChangedFile, scratch string) ([]indexEntry, error) {
	current, err := snapshot.Changes(ctx)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]gitdiff.ChangedFile, len(current))
	for _, file := range current {
		byPath[file.Path] = file
	}
	var changed []string
	var entries []indexEntry
	for _, file := range files {
		now, listed := byPath[file.Path]
		if !listed || now.OldPath != file.OldPath || now.Status != file.Status || now.Version != file.Version {
			changed = append(changed, file.Path)
			continue
		}
		if file.Status == "R" {
			entries = append(entries, indexEntry{path: file.OldPath})
		}
		if file.Status == "D" {
			entries = append(entries, indexEntry{path: file.Path})
			continue
		}
		mode, oid, ok := strings.Cut(file.Version, ":")
		if !ok || mode == "" || oid == "" {
			return nil, fmt.Errorf("git commit: malformed version %q of %s", file.Version, file.Path)
		}
		entries = append(entries, indexEntry{mode: mode, oid: oid, path: file.Path})
	}
	if len(changed) > 0 {
		return nil, &CommitFilesChangedError{Paths: changed}
	}
	for i, entry := range entries {
		if entry.mode == "" || entry.mode == "160000" {
			continue // A removal, or a gitlink naming a commit of its own repository.
		}
		oid, err := c.publishSnapshotBlob(cwd, snapshot.ObjectEnv(), entry, scratch)
		if err != nil {
			return nil, err
		}
		entries[i].oid = oid
	}
	return entries, nil
}

// publishSnapshotBlob writes the snapshot's blob of entry to the
// repository's object store as `git add` would store it, and returns its
// id. The snapshot hashed the file's bytes without clean filters (gitdiff
// never runs a repository's filter commands on its own reads); a commit the
// person asked for applies them, as `git commit` does, to those same bytes.
// A symbolic link's target is stored as it is.
func (c *Core) publishSnapshotBlob(cwd string, snapshotEnv []string, entry indexEntry, scratch string) (oid string, err error) {
	content, err := os.CreateTemp(scratch, "blob-")
	if err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	defer func() {
		if closeErr := content.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("git commit: %w", closeErr))
		}
	}()
	if _, _, err := c.executeSpec(commandSpec{
		binary: "git", cwd: cwd, extraEnv: snapshotEnv,
		output: content, outputLimit: 1 << 40,
		args: []string{"cat-file", "blob", entry.oid},
	}); err != nil {
		return "", fmt.Errorf("git commit: read the checked content of %s: %w", entry.path, err)
	}
	if _, err := content.Seek(0, 0); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	args := []string{"hash-object", "-w", "--stdin", "--path=" + entry.path}
	if entry.mode == "120000" {
		args = []string{"hash-object", "-w", "--stdin", "--no-filters"}
	}
	stdout, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, input: content, args: args})
	if err != nil {
		return "", fmt.Errorf("git commit: store %s: %w", entry.path, err)
	}
	return strings.TrimSpace(stdout), nil
}

// writeIndexEntries records entries in the index env names (the user's
// when env is nil): each blob or gitlink at its path, and each removal.
func (c *Core) writeIndexEntries(cwd string, env []string, entries []indexEntry) error {
	var info, removals strings.Builder
	for _, entry := range entries {
		if entry.mode == "" {
			removals.WriteString(entry.path)
			removals.WriteByte(0)
			continue
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", entry.mode, entry.oid, entry.path)
	}
	if removals.Len() > 0 {
		if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, extraEnv: env, stdin: removals.String(),
			args: []string{"update-index", "-z", "--force-remove", "--stdin"}}); err != nil {
			return err
		}
	}
	if info.Len() > 0 {
		if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, extraEnv: env, stdin: info.String(),
			args: []string{"update-index", "-z", "--index-info"}}); err != nil {
			return err
		}
	}
	return nil
}

// hasHead reports whether the repository at cwd has a commit checked out.
func (c *Core) hasHead(cwd string) (bool, error) {
	result, err := c.run(cwd, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return false, err
	}
	return result.exitCode == 0, nil
}

// commit runs `git commit` over the index env names (the user's when env is
// nil), with the time a hook or signing prompt needs (HookTimeout). It
// commits the index as it is, which git does not hold locked while hooks
// run, so a commit stopped at that bound leaves no index lock; the stop
// reaches its hooks too, so none keeps running after it.
func (c *Core) commit(cwd string, env []string, subject, body string) error {
	_, _, err := c.executeSpec(commandSpec{
		binary: "git", cwd: cwd, extraEnv: env, runsHooks: true,
		args: commitArgs(subject, body),
	})
	return err
}
