package git

import (
	"bytes"
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
	// Refused before anything is committed: recording the selection would
	// drop index entries it does not cover.
	around, err := c.readIndexAround(cwd, entries)
	if err != nil {
		return "", err
	}
	if err := around.displaced(entries); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
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
// the content listed before the hook ran. Each path is written exactly, as
// an entry or a removal; nothing under it or beside it is touched.
//
// A path the hook changed that was not selected takes the commit's entry
// only while the user's index still holds the parent's entry there: an
// entry the user staged differs from the parent, and is kept. An entry that
// would displace one the user's index keeps (a file where the index holds
// entries under that path, or the reverse) fails without writing anything.
func (c *Core) recordCommittedEntries(cwd, sha string, entries []indexEntry) error {
	changes, err := c.commitChanges(cwd, sha)
	if err != nil {
		return err
	}
	committed := make(map[string]indexEntry, len(changes))
	for _, change := range changes {
		committed[change.after.path] = change.after
	}
	selected := make(map[string]bool, len(entries))
	var paths, unchanged []string
	for _, entry := range entries {
		if selected[entry.path] {
			continue
		}
		selected[entry.path] = true
		paths = append(paths, entry.path)
		if _, ok := committed[entry.path]; !ok {
			unchanged = append(unchanged, entry.path)
		}
	}
	if len(unchanged) > 0 {
		// A selected path the commit did not change holds the parent's
		// entry: the hook undid the selected change.
		held, err := c.treeEntries(cwd, sha, unchanged)
		if err != nil {
			return err
		}
		for _, entry := range held {
			committed[entry.path] = entry
		}
	}
	record := make([]indexEntry, 0, len(paths))
	for _, path := range paths {
		record = append(record, committed[path])
	}
	var hooked []commitChange
	candidates := append([]indexEntry(nil), record...)
	for _, change := range changes {
		if !selected[change.after.path] {
			hooked = append(hooked, change)
			candidates = append(candidates, change.after)
		}
	}
	index, err := c.readIndexAround(cwd, candidates)
	if err != nil {
		return err
	}
	for _, change := range hooked {
		if index.entries[change.after.path] == change.before {
			record = append(record, change.after)
		}
	}
	if err := index.displaced(record); err != nil {
		return err
	}
	return c.writeIndexEntries(cwd, nil, record)
}

// commitChange is one path a commit changed against its parent: before is
// the parent's "mode oid" there, or empty when the parent had none, and
// after is the commit's entry, a removal when it has none.
type commitChange struct {
	before string
	after  indexEntry
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
		change := commitChange{after: indexEntry{path: fields[i+1]}}
		if strings.Trim(meta[0], "0") != "" {
			change.before = meta[0] + " " + meta[2]
		}
		if strings.Trim(meta[1], "0") != "" {
			change.after.mode, change.after.oid = meta[1], meta[3]
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// treeEntries returns what commit sha holds at exactly each of paths: a
// blob or gitlink, or a removal where it holds nothing or a directory. The
// listing does not recurse; an entry beside or under a path is not one of
// them.
func (c *Core) treeEntries(cwd, sha string, paths []string) ([]indexEntry, error) {
	args := append([]string{"--literal-pathspecs", "ls-tree", "--full-tree", "-z", sha, "--"}, paths...)
	stdout, _, err := c.Execute(cwd, args...)
	if err != nil {
		return nil, err
	}
	held := make(map[string]indexEntry, len(paths))
	for _, record := range strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		// "<mode> <type> <oid>\t<path>"
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("git ls-tree %s: malformed entry %q", sha, record)
		}
		if fields[1] != "tree" {
			held[path] = indexEntry{mode: fields[0], oid: fields[2], path: path}
		}
	}
	entries := make([]indexEntry, 0, len(paths))
	for _, path := range paths {
		entry, ok := held[path]
		if !ok {
			entry = indexEntry{path: path}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// indexAround is what the user's index holds around a set of candidate
// entries: the entry at each candidate path, and the entries a candidate
// file would displace, since a path cannot be a file and a directory at once.
type indexAround struct {
	// entries maps a candidate path to its stage-zero "mode oid", empty when
	// the index has none, or "conflict" when it holds only conflict stages.
	entries map[string]string
	// under maps a candidate file path to the index entries below it.
	under map[string][]string
	// files are index files at a directory of some candidate file.
	files []string
}

// readIndexAround reads the user's index once, streamed, keeping only what
// indexAround describes for candidates.
func (c *Core) readIndexAround(cwd string, candidates []indexEntry) (*indexAround, error) {
	index := &indexAround{entries: make(map[string]string, len(candidates)), under: make(map[string][]string)}
	isFile := make(map[string]bool)
	dirs := make(map[string]bool)
	for _, entry := range candidates {
		index.entries[entry.path] = ""
		if entry.mode == "" {
			continue
		}
		isFile[entry.path] = true
		addParentDirs(dirs, entry.path)
	}
	records := &nulRecords{each: func(record string) error {
		// "<mode> <oid> <stage>\t<path>"
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return fmt.Errorf("git ls-files: malformed entry %q", record)
		}
		if _, candidate := index.entries[path]; candidate {
			if fields[2] != "0" {
				index.entries[path] = "conflict"
			} else if index.entries[path] != "conflict" {
				index.entries[path] = fields[0] + " " + fields[1]
			}
		}
		if dirs[path] {
			index.files = append(index.files, path)
		}
		for slash := range len(path) {
			if path[slash] == '/' && isFile[path[:slash]] {
				index.under[path[:slash]] = append(index.under[path[:slash]], path)
			}
		}
		return nil
	}}
	if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, output: records, outputLimit: 1 << 40,
		args: []string{"ls-files", "--stage", "-z"}}); err != nil {
		return nil, err
	}
	if len(records.partial) > 0 {
		return nil, fmt.Errorf("git ls-files: output ended inside an entry")
	}
	return index, nil
}

// ErrCommitDisplacesIndexEntries is a commit of listed files refused, or its
// index update not made, because recording a file would remove index entries
// at or under the same path that nothing recorded covers.
var ErrCommitDisplacesIndexEntries = errors.New("the selected files would remove other staged entries from the index")

// displaced reports, as ErrCommitDisplacesIndexEntries naming them, the
// index entries recording would drop: entries under a recorded file, and a
// file at a directory of one, that record does not itself write or remove.
func (index *indexAround) displaced(record []indexEntry) error {
	recorded := make(map[string]bool, len(record))
	dirs := make(map[string]bool)
	for _, entry := range record {
		recorded[entry.path] = true
		if entry.mode != "" {
			addParentDirs(dirs, entry.path)
		}
	}
	var lost []string
	for _, entry := range record {
		if entry.mode == "" {
			continue
		}
		for _, path := range index.under[entry.path] {
			if !recorded[path] {
				lost = append(lost, path)
			}
		}
	}
	for _, path := range index.files {
		if dirs[path] && !recorded[path] {
			lost = append(lost, path)
		}
	}
	if len(lost) == 0 {
		return nil
	}
	named := strings.Join(lost[:min(len(lost), maxNamedChangedFiles)], ", ")
	if more := len(lost) - maxNamedChangedFiles; more > 0 {
		named = fmt.Sprintf("%s and %d more", named, more)
	}
	return fmt.Errorf("%w (%s): a path cannot be a file and a directory; select or unstage them", ErrCommitDisplacesIndexEntries, named)
}

// addParentDirs adds every directory above path to dirs.
func addParentDirs(dirs map[string]bool, path string) {
	for slash := range len(path) {
		if path[slash] == '/' {
			dirs[path[:slash]] = true
		}
	}
}

// nulRecords passes each NUL-terminated record written to it to each, and
// keeps an unterminated tail in partial.
type nulRecords struct {
	each    func(record string) error
	partial []byte
}

func (w *nulRecords) Write(p []byte) (int, error) {
	n := len(p)
	for {
		end := bytes.IndexByte(p, 0)
		if end < 0 {
			w.partial = append(w.partial, p...)
			return n, nil
		}
		record := string(append(w.partial, p[:end]...))
		w.partial = w.partial[:0]
		if err := w.each(record); err != nil {
			return 0, err
		}
		p = p[end+1:]
	}
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
