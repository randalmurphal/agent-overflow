package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/transferfiles"
)

const maxTransferIndexBytes int64 = 32 << 20
const maxTransferIndexEntries = 250_000
const maxTransferHaveCommits = 128

// TransferIndexEntry preserves staging independently of the working files.
// Git's binary index (stat cache, split-index and platform extensions) never
// crosses computers. Gitlinks name another repository and are not blob inputs.
type TransferIndexEntry struct {
	Mode string `json:"mode"`
	OID  string `json:"oid"`
	Path string `json:"path"`
}

func validTransferOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, ch := range oid {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func validateTransferIndex(entries []TransferIndexEntry) error {
	if len(entries) > maxTransferIndexEntries {
		return errors.New("transfer: repository index exceeds the entry limit")
	}
	seen := make(map[string]struct{}, len(entries))
	var total int64
	for _, entry := range entries {
		if !validTransferOID(entry.OID) || !transferfiles.ValidName(entry.Path) ||
			(entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000" && entry.Mode != "160000") {
			return errors.New("transfer: repository index contains an unsupported mode, object or path")
		}
		folded := strings.ToLower(entry.Path)
		if _, ok := seen[folded]; ok {
			return errors.New("transfer: repository paths collide on a case-insensitive computer")
		}
		seen[folded] = struct{}{}
		total += int64(len(entry.Path) + len(entry.OID) + len(entry.Mode) + 4)
		if total > maxTransferIndexBytes {
			return errors.New("transfer: repository index exceeds the metadata limit")
		}
	}
	// The index may come from a peer. Git would otherwise replace a file with
	// a directory entry implicitly, hiding an inconsistent snapshot.
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		prefix := path + "/"
		if at := sort.SearchStrings(paths, prefix); at < len(paths) && strings.HasPrefix(paths[at], prefix) {
			return errors.New("transfer: repository index contains overlapping file paths")
		}
	}
	return nil
}

func (c *Core) ReadTransferIndex(ctx context.Context, cwd string) ([]TransferIndexEntry, error) {
	output, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, maxBytes: maxTransferIndexBytes,
		args: []string{"ls-files", "--stage", "-z"}})
	if err != nil {
		return nil, err
	}
	var entries []TransferIndexEntry
	for raw := range strings.SplitSeq(output, "\x00") {
		if raw == "" {
			continue
		}
		prefix, path, found := strings.Cut(raw, "\t")
		fields := strings.Fields(prefix)
		if !found || len(fields) != 3 {
			return nil, errors.New("transfer: invalid repository index response")
		}
		if fields[2] != "0" {
			return nil, errors.New("Resolve the repository's merge conflicts before transferring its changes.")
		}
		if len(entries) >= maxTransferIndexEntries {
			return nil, errors.New("transfer: repository index exceeds the entry limit")
		}
		entries = append(entries, TransferIndexEntry{Mode: fields[0], OID: fields[1], Path: path})
	}
	if err := validateTransferIndex(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// WriteTransferObjects sends HEAD history and staged blobs absent from the
// destination's advertised commits. It creates no refs, commits, index writes
// or checkout changes on the source. A non-thin pack is independently verifiable.
func (c *Core) WriteTransferObjects(ctx context.Context, cwd, head string, index []TransferIndexEntry, have []string, output io.Writer) error {
	if !validTransferOID(head) || output == nil || len(have) > maxTransferHaveCommits {
		return errors.New("transfer: invalid repository object request")
	}
	if err := validateTransferIndex(index); err != nil {
		return err
	}
	for _, oid := range have {
		if !validTransferOID(oid) || len(oid) != len(head) {
			return errors.New("transfer: invalid destination commit")
		}
	}
	var revisions strings.Builder
	revisions.WriteString(head + "\n")
	seen := make(map[string]bool, len(index))
	for _, entry := range index {
		if len(entry.OID) != len(head) {
			return errors.New("transfer: repository object formats do not match")
		}
		if entry.Mode == "160000" || seen[entry.OID] {
			continue
		}
		seen[entry.OID] = true
		revisions.WriteString(entry.OID + "\n")
	}
	if len(have) != 0 {
		var query strings.Builder
		for _, oid := range have {
			query.WriteString(oid + "^{commit}\n")
		}
		known, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, stdin: query.String(),
			args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}})
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(known, "\n") {
			oid, kind, _ := strings.Cut(line, " ")
			if kind == "commit" && validTransferOID(oid) {
				revisions.WriteString("^" + oid + "\n")
			}
		}
	}
	_, stderr, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, stdin: revisions.String(),
		output: output, outputLimit: transferfiles.MaxFileBytes, timeout: 2 * time.Minute,
		args: []string{"pack-objects", "--stdout", "--revs", "--delta-base-offset", "--threads=2", "--window-memory=32m", "--compression=1"}})
	if err != nil {
		return fmt.Errorf("transfer: export repository objects: %w %s", err, strings.TrimSpace(stderr))
	}
	return nil
}

// ImportTransferObjects adds immutable objects only. An inert registered
// worktree's HEAD/index retain them before preparation is acknowledged.
func (c *Core) ImportTransferObjects(ctx context.Context, cwd string, input io.Reader) error {
	if input == nil {
		return errors.New("transfer: missing repository objects")
	}
	_, stderr, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx,
		input: input, timeout: 2 * time.Minute, args: []string{"-c", "core.fsync=all", "-c", "core.fsyncMethod=fsync", "-c", "core.deltaBaseCacheLimit=32m", "index-pack", "--stdin", "--strict", "--threads=2", fmt.Sprintf("--max-input-size=%d", transferfiles.MaxFileBytes)}})
	if err != nil {
		return fmt.Errorf("transfer: import repository objects: %w %s", err, strings.TrimSpace(stderr))
	}
	return nil
}

// RestoreTransferIndex writes only the caller's private preparation index.
// The fresh index contains exactly these paths, including staged deletions.
// Working-tree materialization is a separate step, preserving unstaged edits.
func (c *Core) RestoreTransferIndex(ctx context.Context, cwd string, entries []TransferIndexEntry) error {
	if err := validateTransferIndex(entries); err != nil {
		return err
	}
	_, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, args: []string{"read-tree", "--empty"}})
	if err != nil {
		return err
	}
	// Index-info is NUL-delimited. Spaces and non-ASCII names are not
	// interpreted as shell syntax; portable path validation precedes every write.
	ordered := append([]TransferIndexEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var input strings.Builder
	for _, entry := range ordered {
		fmt.Fprintf(&input, "%s %s 0\t%s\x00", entry.Mode, entry.OID, entry.Path)
	}
	_, stderr, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, stdin: input.String(), args: []string{"update-index", "-z", "--index-info"}})
	if err != nil {
		return fmt.Errorf("transfer: restore repository staging: %w %s", err, strings.TrimSpace(stderr))
	}
	return nil
}
