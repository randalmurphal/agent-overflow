package git

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/gitdiff"
)

// churnDiffArgs is the diff whose numstat is the workspace's tracked
// churn: the working tree against commit with the panel's line-count
// flags (gitdiff.LineCountFlags), so the per-file insertion and deletion
// counts equal the '+' and '-' content lines the panel parses from its
// patch. No pathspec diffs the whole working tree.
func churnDiffArgs(commit string, pathspecs []string) []string {
	flags := gitdiff.LineCountFlags()
	args := make([]string, 0, 5+len(flags)+len(pathspecs))
	args = append(args, "diff", "--numstat", "-z")
	args = append(args, flags...)
	args = append(args, commit, "--")
	return append(args, pathspecs...)
}

// churnPathspecMagic names a path from the working tree's root, as status
// prints it, with no glob characters read from it.
const churnPathspecMagic = ":(top,literal)"

// maxChurnPathspecBytes bounds the pathspecs one churn diff names. Past
// it the diff runs over the whole working tree: an argument list has a
// platform limit, and the paths of a rename group cannot be split
// across two diffs.
const maxChurnPathspecBytes = 64 * 1024

// trackedFileKey identifies everything one in-place file's diff against
// HEAD reads: the attribute files above it (attributeChains), HEAD's
// blob, and the working tree file by the index blob when status reports
// it equal to the index, else by its stat.
type trackedFileKey struct {
	attributes [sha256.Size]byte
	head       objectID
	index      objectID
	stat       worktreeStat
}

// trackedFileChurn is one in-place file's stored counts. A memo outlives
// the status output its scan read, so it holds no part of one: path is
// the memo's own copy and the key holds ids by value.
type trackedFileChurn struct {
	path                  string
	key                   trackedFileKey
	insertions, deletions int
}

// trackedGroupChurn is the churn of every changed path that is not a
// regular file modified in place: added, deleted, renamed, unmerged,
// type-changed and submodule paths. Rename detection pairs any deleted
// path with any added one, so their counts depend on each other and are
// held as one result under one key.
type trackedGroupChurn struct {
	key                   [sha256.Size]byte
	insertions, deletions int
	// extraFiles counts paths the diff reports that status lists under
	// another path: the old side of a rename status paired and the diff
	// did not.
	extraFiles int
}

// trackedChurnMemo is what one churn scan of a workspace leaves for the
// next: its results, and what the next scan needs to key its own.
type trackedChurnMemo struct {
	// context keys what every diff reads besides the paths' own inputs
	// (churnContext). A scan under another context replays nothing.
	context [sha256.Size]byte
	// warm reports that the scan could key its results: the next scan has
	// them to replay, or nothing to diff.
	warm  bool
	files map[string]trackedFileChurn
	// group is nil when the scan could not key the group.
	group *trackedGroupChurn

	root    worktreeRootProof
	sources attributeSources
}

type trackedChurnTotals struct {
	insertions, deletions, files int
}

// trackedChurn returns the insertions, deletions and file count of the
// changed tracked paths status lists: the totals of churnDiffArgs over the
// whole working tree against the commit status read. in holds what the
// scan read before and beside status; trackedChurn waits for it.
//
// gitwatch asks on every debounce edge, and that diff reads every changed
// file, so its cost follows the size of the whole change rather than what
// moved since the last edge. A file's counts depend only on the two
// contents compared, the attribute files above it and the diff's context
// (churnContext), so they are memoized under a key naming all of them
// (trackedFileKey), and each scan diffs only the paths whose key is new.
// Paths a rename can involve are keyed and diffed together
// (trackedGroupChurn). A scan in which no key changed runs no diff.
//
// A result is stored only when its key reads the same after the diff as
// before it, so the diff saw the working tree file the key names, and
// when the diff read the index status read (indexSnapshot). Nothing is
// keyed when the context cannot be, and a file written within
// statRacyWindow is diffed on every scan.
func (c *Core) trackedChurn(cwd string, status porcelainStatus, in *churnInputs) (trackedChurnTotals, error) {
	in.wait()
	entries := status.entries
	totals := trackedChurnTotals{files: len(entries)}
	next := trackedChurnMemo{root: in.root, sources: in.prev.sources}
	// Before the first commit there is nothing to diff against: the
	// churn is zero, which is what the panel shows there.
	if len(entries) == 0 || !status.hasCommit() {
		next.warm = true
		c.trackedChurnMemos.store(cwd, next, in.start)
		return totals, nil
	}
	next.context, next.sources, next.warm = c.churnContext(cwd, in)
	prev := in.prev
	if !next.warm || prev.context != next.context {
		prev.files, prev.group = nil, nil
	}
	next.files = make(map[string]trackedFileChurn, len(prev.files))
	var attributes *attributeChains
	if next.warm {
		attributes = newAttributeChains(cwd, entries, in.start)
	}

	// pending holds the in-place entries this scan diffs, each with the
	// key its result is stored under and what the diff printed for it.
	type pendingFile struct {
		entry                 *statusEntry
		key                   trackedFileKey
		keyed                 bool
		insertions, deletions int
	}
	pending := make(map[string]pendingFile)
	var pathspecs []string
	pathspecBytes := 0
	name := func(path string) {
		pathspecs = append(pathspecs, churnPathspecMagic+path)
		pathspecBytes += len(churnPathspecMagic) + len(path) + 1
	}

	// groupPaths holds every path of the group; true marks a path status
	// lists as its own entry, false an entry's rename source.
	groupPaths := make(map[string]bool)
	var groupOrder []string
	var groupEntries []*statusEntry
	for i := range entries {
		entry := &entries[i]
		if entry.regular {
			file := pendingFile{entry: entry}
			if attributes != nil {
				file.key, file.keyed = trackedFileKeyOf(cwd, entry, in.start, attributes)
			}
			if hit, ok := prev.files[entry.path]; ok && file.keyed && hit.key == file.key {
				next.files[hit.path] = hit
				totals.insertions += hit.insertions
				totals.deletions += hit.deletions
				continue
			}
			pending[entry.path] = file
			name(entry.path)
			continue
		}
		groupEntries = append(groupEntries, entry)
		if _, listed := groupPaths[entry.path]; !listed {
			groupOrder = append(groupOrder, entry.path)
		}
		groupPaths[entry.path] = true
		if entry.origPath != "" {
			if _, listed := groupPaths[entry.origPath]; !listed {
				groupPaths[entry.origPath] = false
				groupOrder = append(groupOrder, entry.origPath)
			}
		}
	}

	var groupKey [sha256.Size]byte
	groupKeyed := false
	if attributes != nil {
		groupKey, groupKeyed = trackedGroupKeyOf(cwd, status.oid, groupEntries, in.start, attributes)
	}
	groupPending := len(groupEntries) > 0
	if groupPending && groupKeyed && prev.group != nil && prev.group.key == groupKey {
		next.group = prev.group
		totals.insertions += prev.group.insertions
		totals.deletions += prev.group.deletions
		totals.files += prev.group.extraFiles
		groupPending = false
	}
	if groupPending {
		for _, path := range groupOrder {
			name(path)
		}
	}
	if len(pathspecs) == 0 {
		c.trackedChurnMemos.store(cwd, next, in.start)
		return totals, nil
	}

	var result commandResult
	if in.early.usableFor(status.oid) {
		result = in.early.result
	} else {
		if pathspecBytes > maxChurnPathspecBytes {
			pathspecs = nil
		}
		var err error
		result, err = c.runSpec(commandSpec{binary: "git", cwd: cwd, maxBytes: statusOutputLimit, args: churnDiffArgs(status.oid, pathspecs)})
		if err != nil {
			return trackedChurnTotals{}, err
		}
		if result.exitCode != 0 {
			return trackedChurnTotals{}, fmt.Errorf("%s failed: %s", formatCommand("git", churnDiffArgs(status.oid, nil)...), strings.TrimSpace(result.stderr))
		}
	}

	group := trackedGroupChurn{key: groupKey}
	extraFiles := make(map[string]struct{})
	for record := range numstatRecords(result.stdout) {
		if file, ok := pending[record.path]; ok && record.oldPath == "" {
			totals.insertions += record.insertions
			totals.deletions += record.deletions
			file.insertions, file.deletions = record.insertions, record.deletions
			pending[record.path] = file
			continue
		}
		if _, replayed := next.files[record.path]; replayed && record.oldPath == "" {
			continue // a whole-tree diff repeats a file this scan took from the memo
		}
		_, inGroup := groupPaths[record.path]
		if !inGroup && record.oldPath != "" {
			_, inGroup = groupPaths[record.oldPath]
		}
		if inGroup {
			// A whole-tree diff repeats a group this scan took from the
			// memo.
			if groupPending {
				group.insertions += record.insertions
				group.deletions += record.deletions
				if !groupPaths[record.path] {
					extraFiles[record.path] = struct{}{}
				}
			}
			continue
		}
		// A path that changed after status was read. Count it; the
		// event that changed it brings the scan that keys it.
		totals.insertions += record.insertions
		totals.deletions += record.deletions
		totals.files++
	}
	sameIndex := in.index.unchanged(in.start)
	for _, file := range pending {
		if !file.keyed || !sameIndex {
			continue
		}
		// A file written while the diff ran may be the one the diff read.
		if after, ok := trackedFileKeyOf(cwd, file.entry, in.start, attributes); !ok || after != file.key {
			continue
		}
		// An in-place path the diff did not print equals HEAD. The memo
		// holds its own copy of the path status printed.
		path := strings.Clone(file.entry.path)
		next.files[path] = trackedFileChurn{path: path, key: file.key, insertions: file.insertions, deletions: file.deletions}
	}
	if groupPending {
		group.extraFiles = len(extraFiles)
		totals.insertions += group.insertions
		totals.deletions += group.deletions
		totals.files += group.extraFiles
		if sameIndex && groupKeyed {
			if after, ok := trackedGroupKeyOf(cwd, status.oid, groupEntries, in.start, attributes); ok && after == groupKey {
				next.group = &group
			}
		}
	}
	c.trackedChurnMemos.store(cwd, next, in.start)
	return totals, nil
}

// trackedFileKeyOf returns the key of an in-place entry, and false when
// its working tree file or an attribute file above it has not been left
// alone since before the scan began.
func trackedFileKeyOf(cwd string, entry *statusEntry, scanStart time.Time, attributes *attributeChains) (trackedFileKey, bool) {
	chain := attributes.of(entry.path)
	stat, state := statWorktreeFile(filepath.Join(cwd, filepath.FromSlash(entry.path)), scanStart, false)
	head, headOK := parseObjectID(entry.head)
	if !chain.keyed || state != worktreeFileSettled || !headOK {
		return trackedFileKey{}, false
	}
	key := trackedFileKey{attributes: chain.key, head: head}
	if entry.xy[1] == '.' {
		// Status compared the file to the index and found it equal, and
		// it has not been written since.
		var indexOK bool
		key.index, indexOK = parseObjectID(entry.index)
		return key, indexOK
	}
	key.stat = stat
	return key, true
}

// trackedGroupKeyOf keys both sides of every group entry's diff against
// commit, and returns false when one of them cannot be identified. An
// unmerged entry's record names its stages and not the commit's blob, so
// the commit is part of the key.
func trackedGroupKeyOf(cwd string, commit string, entries []*statusEntry, scanStart time.Time, attributes *attributeChains) ([sha256.Size]byte, bool) {
	h := sha256.New()
	_, _ = io.WriteString(h, commit)
	keyed := true
	for _, entry := range entries {
		if !hashGroupEntry(h, cwd, entry, scanStart, attributes) {
			keyed = false
		}
	}
	return [sha256.Size]byte(h.Sum(nil)), keyed
}

// hashGroupEntry writes what identifies both sides of a group entry's
// diff into h, and returns false when the entry cannot be identified.
func hashGroupEntry(h hash.Hash, cwd string, entry *statusEntry, scanStart time.Time, attributes *attributeChains) bool {
	// The record names HEAD's and the index's blobs and the entry's
	// state; a NUL cannot occur inside either field.
	_, _ = io.WriteString(h, entry.record)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, entry.origPath)
	_, _ = h.Write([]byte{0})
	if entry.submodule {
		// A submodule's checked-out commit is not in its record.
		return false
	}
	for _, path := range []string{entry.path, entry.origPath} {
		if path == "" {
			continue
		}
		chain := attributes.of(path)
		if !chain.keyed {
			return false
		}
		_, _ = h.Write(chain.key[:])
	}
	return hashWorktreeFile(h, filepath.Join(cwd, filepath.FromSlash(entry.path)), scanStart, false) != worktreeFileUnsettled
}
