package gitdiff

import (
	"context"
	"crypto/sha1" //nolint:gosec // git's object id, not a security hash
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/workspacefiles"
	"agent-overflow/internal/workspacepath"
)

// Pin is what pins a diff's two sides for reads after the diff is closed:
// full object IDs of a commit or a tree. New is empty when the new side is
// the working tree, which no object pins; the diff's WorktreeBlobs pin its
// changed files instead.
type Pin struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// Pin answers the sides' pin. A worktree snapshot's tree lives only in its
// temporary object directory, so it is not part of the pin.
func (s *Sides) Pin() Pin {
	if s.worktree {
		return Pin{Old: s.old}
	}
	return Pin{Old: s.old, New: s.new}
}

// WorktreeBlob pins one changed file of a working-tree side: its path and
// the blob ID and size the snapshot recorded for it.
type WorktreeBlob struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

// WorktreeBlobs lists the regular files the diff changes on its
// working-tree side, from the snapshot the patch is read from, at most
// limit of them; truncated reports that more were left out. Sides whose
// new side is not the working tree answer nothing.
func (s *Sides) WorktreeBlobs(ctx context.Context, limit int) (blobs []WorktreeBlob, truncated bool, err error) {
	if !s.worktree || s.new == "" {
		return nil, false, nil
	}
	args := []string{"diff", "-z", "--raw", "--no-abbrev", "--no-renames", "--no-color", "--no-ext-diff",
		"--no-textconv", "--no-relative", s.old, s.new, "--"}
	if s.paths != nil {
		args = append(append([]string{"--literal-pathspecs"}, args...), s.paths...)
	}
	stdout, _, _, err := runGit(ctx, s.workspace, s.env, false, args...)
	if err != nil {
		return nil, false, fmt.Errorf("gitdiff: list worktree blobs: %w", err)
	}
	tokens := strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00")
	var oids []string
	for i := 0; i+1 < len(tokens); i += 2 {
		fields := strings.Fields(tokens[i])
		if len(fields) != 5 {
			return nil, false, fmt.Errorf("gitdiff: malformed raw record %q", tokens[i])
		}
		if !isFileMode(fields[1]) {
			continue
		}
		if len(blobs) == limit {
			truncated = true
			break
		}
		blobs = append(blobs, WorktreeBlob{Path: tokens[i+1], Blob: fields[3]})
		oids = append(oids, fields[3])
	}
	if len(oids) == 0 {
		return blobs, truncated, nil
	}
	sizes, err := objectSizes(ctx, s.workspace, s.env, oids)
	if err != nil {
		return nil, false, err
	}
	for i := range blobs {
		blobs[i].Size = sizes[blobs[i].Blob]
	}
	return blobs, truncated, nil
}

var fullOIDPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// IsFullOID reports whether s is a full SHA-1 or SHA-256 object ID.
func IsFullOID(s string) bool { return fullOIDPattern.MatchString(s) }

// PinnedSides reopens a diff's pinned sides in workspace for file reads,
// without a snapshot. Each pinned ID must be a full ID of a commit or a
// tree in this repository.
func PinnedSides(ctx context.Context, workspace string, pin Pin) (*Sides, error) {
	ids := []string{pin.Old}
	if pin.New != "" {
		ids = append(ids, pin.New)
	}
	for _, id := range ids {
		if !IsFullOID(id) {
			return nil, fmt.Errorf("gitdiff: %q is not a full object ID", id)
		}
	}
	stdout, _, _, err := runGitWithStdin(ctx, workspace, nil, []byte(strings.Join(ids, "\n")+"\n"), false,
		"cat-file", "--batch-check=%(objecttype)")
	if err != nil {
		return nil, fmt.Errorf("gitdiff: check pinned sides: %w", err)
	}
	types := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(types) != len(ids) {
		return nil, fmt.Errorf("gitdiff: check pinned sides: %d answers for %d objects", len(types), len(ids))
	}
	for i, kind := range types {
		if kind != "commit" && kind != "tree" {
			return nil, fmt.Errorf("gitdiff: %s is not a commit or a tree in this repository", ids[i])
		}
	}
	return &Sides{workspace: workspace, old: pin.Old, new: pin.New, worktree: pin.New == ""}, nil
}

// Entry is one path on one side of pinned sides.
type Entry struct {
	Exists bool
	// Mode is git's octal mode: 100644 or 100755 for a file, 120000 for a
	// symlink, 160000 for a submodule, 040000 for a directory.
	Mode string
	// OID is the blob ID; empty for a working-tree file, whose ID is known
	// only by reading it.
	OID  string
	Size int64
}

// IsFile reports whether the entry is a regular file's content.
func (e Entry) IsFile() bool { return e.Exists && isFileMode(e.Mode) }

func isFileMode(mode string) bool { return mode == "100644" || mode == "100755" }

// Bounds of one Entries call: the paths ride git's argument list.
const (
	MaxEntryPaths     = 200
	MaxEntryPathBytes = 64 << 10
)

// CleanPath validates one path a caller names on a side: relative, inside
// the repository, no NUL, no trailing separator. It answers the form git
// prints.
func CleanPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || strings.HasSuffix(path, "/") {
		return "", fmt.Errorf("gitdiff: invalid path %q", path)
	}
	clean, err := workspacepath.NormalizeRelative(path)
	if err != nil {
		return "", fmt.Errorf("gitdiff: invalid path %q: %w", path, err)
	}
	clean = filepath.ToSlash(clean)
	if clean != path {
		return "", fmt.Errorf("gitdiff: path %q is not in its canonical form %q", path, clean)
	}
	return clean, nil
}

// OldEntries answers each path's entry on the old side.
func (s *Sides) OldEntries(ctx context.Context, paths []string) (map[string]Entry, error) {
	return s.treeEntries(ctx, s.old, paths)
}

// NewEntries answers each path's entry on the new side: from its tree, or
// from the working tree, where a path reached through a symlinked
// directory, or leading outside the workspace, is refused.
func (s *Sides) NewEntries(ctx context.Context, paths []string) (map[string]Entry, error) {
	if !s.worktree {
		return s.treeEntries(ctx, s.new, paths)
	}
	if err := checkEntryPaths(paths); err != nil {
		return nil, err
	}
	entries := make(map[string]Entry, len(paths))
	for _, path := range paths {
		full, err := s.worktreePath(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			entries[path] = Entry{}
		case err != nil:
			return nil, fmt.Errorf("gitdiff: inspect %s: %w", path, err)
		case info.Mode().IsRegular():
			entries[path] = Entry{Exists: true, Mode: gitIndexMode(info.Mode()), Size: info.Size()}
		default:
			entries[path] = Entry{Exists: true, Mode: worktreeOtherMode(info.Mode())}
		}
	}
	return entries, nil
}

func worktreeOtherMode(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "120000"
	case mode.IsDir():
		return "040000"
	}
	return "000000"
}

// worktreePath joins path to the workspace after checking its directory
// resolves inside the workspace, so a symlinked directory cannot reach out.
func (s *Sides) worktreePath(path string) (string, error) {
	root, err := filepath.EvalSymlinks(s.workspace)
	if err != nil {
		return "", fmt.Errorf("gitdiff: resolve workspace: %w", err)
	}
	full := filepath.Join(s.workspace, filepath.FromSlash(path))
	dir, err := filepath.EvalSymlinks(filepath.Dir(full))
	if errors.Is(err, fs.ErrNotExist) {
		return full, nil
	}
	if err != nil {
		return "", fmt.Errorf("gitdiff: resolve %s: %w", path, err)
	}
	if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gitdiff: %s is outside the workspace", path)
	}
	if want := filepath.Join(root, filepath.Dir(filepath.FromSlash(path))); dir != want {
		return "", fmt.Errorf("gitdiff: %s is reached through a symlink", path)
	}
	return filepath.Join(dir, filepath.Base(full)), nil
}

func checkEntryPaths(paths []string) error {
	if len(paths) > MaxEntryPaths {
		return fmt.Errorf("gitdiff: %d paths exceeds %d", len(paths), MaxEntryPaths)
	}
	total := 0
	for _, path := range paths {
		if _, err := CleanPath(path); err != nil {
			return err
		}
		total += len(path)
	}
	if total > MaxEntryPathBytes {
		return fmt.Errorf("gitdiff: paths exceed %d bytes", MaxEntryPathBytes)
	}
	return nil
}

// treeEntries reads each literal path's own entry in a tree-ish: one
// `git ls-tree` that does not recurse, so a directory is its own entry and
// the answer has at most one line per path.
func (s *Sides) treeEntries(ctx context.Context, treeish string, paths []string) (map[string]Entry, error) {
	if err := checkEntryPaths(paths); err != nil {
		return nil, err
	}
	entries := make(map[string]Entry, len(paths))
	for _, path := range paths {
		entries[path] = Entry{}
	}
	if len(paths) == 0 {
		return entries, nil
	}
	args := append([]string{"ls-tree", "-z", "-l", "--full-tree", treeish, "--"}, paths...)
	stdout, _, _, err := runGit(ctx, s.workspace, []string{"GIT_LITERAL_PATHSPECS=1"}, false, args...)
	if err != nil {
		return nil, fmt.Errorf("gitdiff: read entries: %w", err)
	}
	for _, record := range strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("gitdiff: malformed ls-tree record %q", record)
		}
		if _, asked := entries[path]; !asked {
			continue
		}
		entry := Entry{Exists: true, Mode: fields[0], OID: fields[2]}
		if fields[1] == "blob" {
			if entry.Size, err = strconv.ParseInt(fields[3], 10, 64); err != nil {
				return nil, fmt.Errorf("gitdiff: malformed ls-tree size %q", record)
			}
		}
		entries[path] = entry
	}
	// ls-tree descends into a directory another requested path lies under
	// instead of listing it; a found entry beneath a path proves that path
	// is a directory.
	for path, entry := range entries {
		if entry.Exists {
			continue
		}
		for other, found := range entries {
			if found.Exists && strings.HasPrefix(other, path+"/") {
				entries[path] = Entry{Exists: true, Mode: "040000"}
				break
			}
		}
	}
	return entries, nil
}

// objectSizes reads the size of each object in one `git cat-file
// --batch-check`.
func objectSizes(ctx context.Context, workspace string, env, oids []string) (map[string]int64, error) {
	input := strings.Join(oids, "\n") + "\n"
	stdout, _, _, err := runGitWithStdin(ctx, workspace, env, []byte(input), false,
		"cat-file", "--batch-check=%(objectname) %(objectsize)")
	if err != nil {
		return nil, fmt.Errorf("gitdiff: read object sizes: %w", err)
	}
	sizes := make(map[string]int64, len(oids))
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		oid, size, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("gitdiff: malformed batch-check line %q", line)
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("gitdiff: object %s: %q", oid, line)
		}
		sizes[oid] = n
	}
	return sizes, nil
}

// ReadBlob fills buf with the blob oid, whose size the caller learned from
// its entry: exactly len(buf) bytes, or an error. git's output is read into
// buf and never past it.
func (s *Sides) ReadBlob(ctx context.Context, oid string, buf []byte) error {
	if !IsFullOID(oid) {
		return fmt.Errorf("gitdiff: %q is not a full object ID", oid)
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", oid)
	cmd.Dir = s.workspace
	cmd.Env = gitEnv(s.env)
	out := &fixedWriter{buf: buf}
	errOut := &boundedBuffer{limit: maxStreamStderrBytes}
	runErr := procutil.RunDrained(ctx, cmd, out, errOut, gitPipeWaitDelay)
	if _, err := gitResult(ctx, runErr, false, cmd.Args[1:], errOut.String()); err != nil {
		return fmt.Errorf("gitdiff: read blob %s: %w", oid, err)
	}
	if out.overflow || out.n != len(buf) {
		return fmt.Errorf("gitdiff: blob %s is not %d bytes", oid, len(buf))
	}
	return nil
}

// fixedWriter writes into a preallocated buffer and records an overflow
// instead of growing it.
type fixedWriter struct {
	buf      []byte
	n        int
	overflow bool
}

func (w *fixedWriter) Write(p []byte) (int, error) {
	copied := copy(w.buf[w.n:], p)
	w.n += copied
	if copied < len(p) {
		w.overflow = true
	}
	return len(p), nil
}

// ErrChangedSinceRead reports a working-tree file whose content is no
// longer the blob the diff pinned.
var ErrChangedSinceRead = errors.New("the file changed since the diff was read")

// ReadWorktreeFile fills buf with the working-tree file at path and
// checks it is the pinned blob: exactly len(buf) bytes (the size the caller
// reserved) whose git blob ID is blob. A file that is not that blob is
// ErrChangedSinceRead.
func (s *Sides) ReadWorktreeFile(path, blob string, buf []byte) error {
	if !s.worktree {
		return errors.New("gitdiff: the new side is not the working tree")
	}
	if !IsFullOID(blob) {
		return fmt.Errorf("gitdiff: %q is not a full object ID", blob)
	}
	full, err := s.worktreePath(path)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(full); err != nil || !info.Mode().IsRegular() {
		return ErrChangedSinceRead
	}
	file, _, err := workspacefiles.OpenRegular(full)
	if err != nil {
		return fmt.Errorf("gitdiff: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if _, err := io.ReadFull(file, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return ErrChangedSinceRead
		}
		return fmt.Errorf("gitdiff: read %s: %w", path, err)
	}
	if n, _ := file.Read(make([]byte, 1)); n != 0 {
		return ErrChangedSinceRead
	}
	if BlobID(buf, len(blob)) != blob {
		return ErrChangedSinceRead
	}
	return nil
}

// BlobID is git's object ID of a blob holding data, in the hash whose hex
// length is hexLen (40 for SHA-1, 64 for SHA-256).
func BlobID(data []byte, hexLen int) string {
	var h hash.Hash
	if hexLen == 64 {
		h = sha256.New()
	} else {
		h = sha1.New() //nolint:gosec // git's object id
	}
	// A hash.Hash's Write never returns an error.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}
