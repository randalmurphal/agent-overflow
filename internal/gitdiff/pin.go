package gitdiff

import (
	"bytes"
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
	"strconv"
	"strings"

	"agent-overflow/internal/procutil"
)

// Pin is what pins a diff's two sides for reads after the diff is closed:
// full object IDs of a commit or a tree. New is empty when the new side is
// the working tree, which no object pins; the diff's WorktreeBlobs pin its
// changed files instead. A Pin comes from Sides.Pin, never from a client.
type Pin struct {
	Old string
	New string
}

// Pin answers the sides' pin. A worktree snapshot's tree lives only in its
// temporary object directory, so it is not part of the pin.
func (s *Sides) Pin() Pin {
	if s.worktree {
		return Pin{Old: s.old}
	}
	return Pin{Old: s.old, New: s.new}
}

// Workspace answers the repository the sides were opened in.
func (s *Sides) Workspace() string { return s.workspace }

// OpenedSides reopens sides an earlier open pinned, for file reads.
func OpenedSides(workspace string, pin Pin) *Sides {
	return &Sides{workspace: workspace, old: pin.Old, new: pin.New, worktree: pin.New == ""}
}

// WorktreeBlob pins one path a working-tree side changes, as the
// snapshot recorded it: Mode is git's mode there (000000 when the path is
// gone), and for a regular file Blob and Size are its blob ID and size.
// The snapshot hashes a changed file's bytes as they are on disk, without
// filters (stageWorktreeNoFilters), so Blob is BlobID of those bytes.
type WorktreeBlob struct {
	Path string
	Mode string
	Blob string
	Size int64
}

// IsFile reports whether the path is a regular file on the working-tree
// side.
func (b WorktreeBlob) IsFile() bool { return isFileMode(b.Mode) }

// maxRawToken bounds one NUL-separated token of `git diff --raw -z`: a
// record's metadata or one path.
const maxRawToken = 64 << 10

// WorktreeBlobs lists the paths the diff changes on its working-tree
// side, from the snapshot the patch is read from, at most limit of them;
// truncated reports that more were left out. git's output
// is parsed as it arrives and git is stopped at the bound. Sides whose new
// side is not the working tree answer nothing.
func (s *Sides) WorktreeBlobs(ctx context.Context, limit int) (blobs []WorktreeBlob, truncated bool, err error) {
	if !s.worktree || s.new == "" {
		return nil, false, nil
	}
	args := []string{"diff", "-z", "--raw", "--no-abbrev", "--no-renames", "--no-color", "--no-ext-diff",
		"--no-textconv", "--no-relative", s.old, s.new, "--"}
	if s.paths != nil {
		args = append(append([]string{"--literal-pathspecs"}, args...), s.paths...)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	records := &rawRecords{limit: limit, stop: stop}
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = s.workspace
	cmd.Env = gitEnv(s.env)
	errOut := &boundedBuffer{limit: maxStreamStderrBytes}
	runErr := procutil.RunDrained(runCtx, cmd, records, errOut, gitPipeWaitDelay)
	switch {
	case records.err != nil:
		return nil, false, records.err
	case records.truncated:
		// git was stopped on purpose; its exit says nothing.
	case ctx.Err() != nil:
		return nil, false, fmt.Errorf("gitdiff: list worktree blobs: %w", ctx.Err())
	default:
		if _, err := gitResult(runCtx, runErr, false, cmd.Args[1:], errOut.String()); err != nil {
			return nil, false, fmt.Errorf("gitdiff: list worktree blobs: %w", err)
		}
		if len(records.partial) > 0 || records.haveMeta {
			return nil, false, errors.New("gitdiff: list worktree blobs: truncated raw output")
		}
	}
	blobs = records.blobs
	if len(blobs) == 0 {
		return blobs, records.truncated, nil
	}
	var oids []string
	for _, blob := range blobs {
		if blob.IsFile() {
			oids = append(oids, blob.Blob)
		}
	}
	if len(oids) == 0 {
		return blobs, records.truncated, nil
	}
	sizes, err := objectSizes(ctx, s.workspace, s.env, oids)
	if err != nil {
		return nil, false, err
	}
	for i := range blobs {
		if blobs[i].IsFile() {
			blobs[i].Size = sizes[blobs[i].Blob]
		}
	}
	return blobs, records.truncated, nil
}

// rawRecords parses `git diff --raw -z` as git writes it, keeping at most
// limit records and one partial token; at the record past
// limit it calls stop and drops the rest.
type rawRecords struct {
	limit     int
	stop      func()
	partial   []byte
	meta      string
	haveMeta  bool
	blobs     []WorktreeBlob
	truncated bool
	err       error
}

func (r *rawRecords) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !r.truncated && r.err == nil {
		end := bytes.IndexByte(p, 0)
		if end < 0 {
			if len(r.partial)+len(p) > maxRawToken {
				r.fail(fmt.Errorf("gitdiff: raw record token exceeds %d bytes", maxRawToken))
				break
			}
			r.partial = append(r.partial, p...)
			break
		}
		token := string(r.partial) + string(p[:end])
		r.partial = r.partial[:0]
		p = p[end+1:]
		r.token(token)
	}
	return n, nil
}

func (r *rawRecords) token(token string) {
	if !r.haveMeta {
		r.meta, r.haveMeta = token, true
		return
	}
	r.haveMeta = false
	fields := strings.Fields(r.meta)
	if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") {
		r.fail(fmt.Errorf("gitdiff: malformed raw record %q", r.meta))
		return
	}
	if len(r.blobs) == r.limit {
		r.truncated = true
		r.stop()
		return
	}
	r.blobs = append(r.blobs, WorktreeBlob{Path: token, Mode: fields[1], Blob: fields[3]})
}

func (r *rawRecords) fail(err error) {
	r.err = err
	r.stop()
}

// IsFullOID reports whether s is a full SHA-1 or SHA-256 object ID.
func IsFullOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Entry is one path on one committed side.
type Entry struct {
	Exists bool
	// Mode is git's octal mode: 100644 or 100755 for a file, 120000 for a
	// symlink, 160000 for a submodule, 040000 for a directory.
	Mode string
	// OID is the object ID; empty for a directory implied by a deeper
	// entry (treeEntries).
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
	maxPathBytes      = 4096
)

// CleanPath validates one repository path as git prints it, unchanged:
// relative, '/'-separated, no empty, "." or ".." segment, no backslash and
// no NUL. Leading and trailing spaces are part of a name.
func CleanPath(path string) (string, error) {
	switch {
	case path == "":
		return "", errors.New("gitdiff: empty path")
	case len(path) > maxPathBytes:
		return "", fmt.Errorf("gitdiff: path exceeds %d bytes", maxPathBytes)
	case strings.ContainsRune(path, 0):
		return "", fmt.Errorf("gitdiff: invalid path %q: NUL", path)
	case strings.ContainsRune(path, '\\'):
		return "", fmt.Errorf("gitdiff: invalid path %q: separators are '/'", path)
	}
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "":
			return "", fmt.Errorf("gitdiff: invalid path %q: empty segment", path)
		case ".", "..":
			return "", fmt.Errorf("gitdiff: invalid path %q: %q segment", path, segment)
		}
	}
	return path, nil
}

// OldEntries answers each path's entry on the old side.
func (s *Sides) OldEntries(ctx context.Context, paths []string) (map[string]Entry, error) {
	return s.treeEntries(ctx, s.old, paths)
}

// NewEntries answers each path's entry on a committed new side. A
// working-tree side has no entries to read: its files are the open's
// WorktreeBlobs.
func (s *Sides) NewEntries(ctx context.Context, paths []string) (map[string]Entry, error) {
	if s.worktree {
		return nil, errors.New("gitdiff: the new side is the working tree")
	}
	return s.treeEntries(ctx, s.new, paths)
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

// ReadWorktreeFile fills buf with the working-tree file at path and checks
// it is the pinned blob: exactly len(buf) bytes (the blob's size) whose git
// blob ID, over the bytes as they are on disk, is blob. The file is opened
// through the workspace as a root, so neither a symlinked parent nor a
// symlinked leaf can lead outside it whenever it was swapped in; its type
// and size are read from the opened file. A file that is not that blob is
// ErrChangedSinceRead.
func (s *Sides) ReadWorktreeFile(path, blob string, buf []byte) error {
	if !s.worktree {
		return errors.New("gitdiff: the new side is not the working tree")
	}
	if !IsFullOID(blob) {
		return fmt.Errorf("gitdiff: %q is not a full object ID", blob)
	}
	if _, err := CleanPath(path); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.workspace)
	if err != nil {
		return fmt.Errorf("gitdiff: open workspace: %w", err)
	}
	defer func() { _ = root.Close() }()
	if s.beforeWorktreeOpen != nil {
		s.beforeWorktreeOpen(path)
	}
	file, err := root.Open(filepath.FromSlash(path))
	if errors.Is(err, fs.ErrNotExist) {
		return ErrChangedSinceRead
	}
	if err != nil {
		return fmt.Errorf("gitdiff: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("gitdiff: inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(buf)) {
		return ErrChangedSinceRead
	}
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

// HasFilter reports whether path has a `filter` attribute in the working
// tree, so its bytes on disk are not the bytes git stores for it.
func (s *Sides) HasFilter(ctx context.Context, path string) (bool, error) {
	if _, err := CleanPath(path); err != nil {
		return false, err
	}
	stdout, _, _, err := runGit(ctx, s.workspace, nil, false, "check-attr", "-z", "filter", "--", path)
	if err != nil {
		return false, fmt.Errorf("gitdiff: read attributes of %s: %w", path, err)
	}
	// <path> NUL filter NUL <value> NUL
	fields := strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00")
	if len(fields) != 3 {
		return false, fmt.Errorf("gitdiff: malformed check-attr output %q", stdout)
	}
	switch fields[2] {
	case "unspecified", "unset":
		return false, nil
	}
	return true, nil
}

// lfsPointerPrefix opens every Git LFS pointer file
// (https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md).
const lfsPointerPrefix = "version https://git-lfs.github.com/spec/v1\n"

// MaxLFSPointerBytes is the size a Git LFS pointer stays under.
const MaxLFSPointerBytes = 1024

// IsLFSPointer reports whether data is a Git LFS pointer: what a repository
// stores for a file whose content lives on an LFS server.
func IsLFSPointer(data []byte) bool {
	return len(data) < MaxLFSPointerBytes && bytes.HasPrefix(data, []byte(lfsPointerPrefix))
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
