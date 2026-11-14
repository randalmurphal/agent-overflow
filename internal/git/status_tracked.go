package git

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// churnDiffArgs is the diff whose numstat is the workspace's tracked
// churn: the working tree against commit with the options of the panel's
// gitdiff.OpenWorktreeDiff (--minimal --no-ext-diff --no-textconv
// --find-renames --no-relative), so the per-file insertion and deletion
// counts equal the '+' and '-' content lines the panel parses from
// --patch. No pathspec diffs the whole working tree.
func churnDiffArgs(commit string, pathspecs []string) []string {
	args := make([]string, 0, 10+len(pathspecs))
	args = append(args, "diff", "--numstat", "-z", "--minimal", "--no-ext-diff", "--no-textconv", "--find-renames", "--no-relative", commit, "--")
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

// objectID is a git object id's bytes, zero-padded.
type objectID [sha256.Size]byte

// parseObjectID reads a hexadecimal object id.
func parseObjectID(hexID string) (objectID, bool) {
	var id objectID
	if hexID == "" || len(hexID)%2 != 0 || len(hexID) > 2*len(id) {
		return objectID{}, false
	}
	for i := 0; i < len(hexID); i += 2 {
		high, low := hexDigit(hexID[i]), hexDigit(hexID[i+1])
		if high < 0 || low < 0 {
			return objectID{}, false
		}
		id[i/2] = byte(high<<4 | low)
	}
	return id, true
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return -1
	}
}

// trackedFileKey identifies both sides of one in-place file's diff
// against HEAD: HEAD's blob, and the working tree file by the index blob
// when status reports it equal to the index, else by its stat.
type trackedFileKey struct {
	head  objectID
	index objectID
	stat  worktreeStat
}

// trackedFileChurn is one in-place file's stored counts. A memo outlives
// the status output its scan read, so it holds no part of one: path is
// the memo's own copy and the key holds ids by value.
type trackedFileChurn struct {
	path                  string
	key                   trackedFileKey
	insertions, deletions int
}

// newTrackedFileChurn returns the entry stored for path, which may be
// part of a git output.
func newTrackedFileChurn(path string, key trackedFileKey, insertions, deletions int) trackedFileChurn {
	return trackedFileChurn{path: strings.Clone(path), key: key, insertions: insertions, deletions: deletions}
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

// trackedChurnMemo is what one churn scan leaves for the next.
type trackedChurnMemo struct {
	// context keys everything a diff reads besides the two contents
	// (churnContext). A scan under another context starts empty.
	context [sha256.Size]byte
	// warm reports that the scan could key its results: the next scan has
	// them to replay, or nothing to diff.
	warm  bool
	files map[string]trackedFileChurn
	// group is nil when the scan could not key the group.
	group *trackedGroupChurn
}

// earlyChurnDiff is a whole-tree churn diff started beside the status
// read.
type earlyChurnDiff struct {
	done chan struct{}
	// commit is what the diff ran against: a commit id, or "HEAD" under a
	// git whose results cannot be keyed.
	commit string
	// config is the effective config read before the diff ran, when its
	// results can be keyed.
	config     string
	configRead bool
	result     commandResult
	err        error
}

// startEarlyChurnDiff starts the whole-tree diff of a scan that has no
// stored results to choose paths by, so that scan costs the longer of the
// status read and the diff, not their sum. It returns nil for a workspace
// whose last scan was warm. The diff is pinned to the commit HEAD names
// now; trackedChurn uses it only when status read the same commit. The
// config its context is keyed by is read before it runs, so a config
// written while it ran keys the next scan differently.
func (c *Core) startEarlyChurnDiff(cwd string) *earlyChurnDiff {
	unkeyable := c.attributeFilesUnnamed.Load()
	if memo, ok := c.trackedChurnMemos.snapshot(cwd); ok && memo.warm && !unkeyable {
		return nil
	}
	early := &earlyChurnDiff{done: make(chan struct{}), commit: "HEAD"}
	go func() {
		defer close(early.done)
		if !unkeyable {
			head, err := c.run(cwd, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
			if err != nil || head.exitCode != 0 {
				early.err = errors.New("no commit to diff against")
				return
			}
			early.commit = strings.TrimSpace(head.stdout)
			early.config, early.configRead = c.effectiveConfig(cwd)
		}
		early.result, early.err = c.runSpec(commandSpec{binary: "git", cwd: cwd, maxBytes: statusOutputLimit, args: churnDiffArgs(early.commit, nil)})
	}()
	return early
}

// wait blocks until the diff has ended. A nil diff has.
func (d *earlyChurnDiff) wait() {
	if d != nil {
		<-d.done
	}
}

// usableFor reports whether the diff ran against commit and succeeded.
func (d *earlyChurnDiff) usableFor(commit string) bool {
	return d != nil && d.err == nil && d.result.exitCode == 0 && (d.commit == commit || d.commit == "HEAD")
}

// effectiveConfig returns the config the diff was preceded by, and false
// when it read none.
func (d *earlyChurnDiff) effectiveConfig() (string, bool) {
	if d == nil {
		return "", false
	}
	return d.config, d.configRead
}

// indexSnapshot is the index file's stat as a scan began. Status and the
// churn diff each read the index, and what a path's diff prints depends
// on whether the index lists it. A scan stores its results only when both
// read the same index: the file had been left alone since before the scan
// began, and is the same file after the diff.
type indexSnapshot struct {
	// path is empty when git did not name the index.
	path  string
	stat  worktreeStat
	state worktreeFileState
}

// snapshotIndex reads the stat of the index of cwd's working tree.
func (c *Core) snapshotIndex(cwd string, scanStart time.Time) indexSnapshot {
	path := c.worktreeRoot(cwd, scanStart).indexPath
	if path == "" {
		return indexSnapshot{}
	}
	stat, state := statWorktreeFile(path, scanStart, true)
	return indexSnapshot{path: path, stat: stat, state: state}
}

// unchanged reports whether the index is still the file the scan began
// with. A repository with no index file has the same one while it has
// none.
func (s indexSnapshot) unchanged(scanStart time.Time) bool {
	if s.path == "" || s.state == worktreeFileUnsettled {
		return false
	}
	stat, state := statWorktreeFile(s.path, scanStart, true)
	return state == s.state && stat == s.stat
}

type trackedChurnTotals struct {
	insertions, deletions, files int
}

// trackedChurn returns the insertions, deletions and file count of the
// changed tracked paths status lists: the totals of churnDiffArgs over the
// whole working tree against the commit status read. scanStart is a time
// before status ran, index the index file's stat then, and early the diff
// startEarlyChurnDiff began then.
//
// gitwatch asks on every debounce edge, and that diff reads every changed
// file, so its cost follows the size of the whole change rather than what
// moved since the last edge. A file's counts depend only on the two
// contents compared and on the diff's context (churnContext), so they are
// memoized under a key naming both contents (trackedFileKey), and each
// scan diffs only the paths whose key is new. Paths a rename can involve
// are keyed and diffed together (trackedGroupChurn). A scan in which no
// key changed runs no diff.
//
// A result is stored only when its key reads the same after the diff as
// before it, so the diff saw the working tree file the key names, and
// when the diff read the index status read (indexSnapshot). Nothing is
// keyed when the context cannot be (churnContext), and a file written
// within statRacyWindow is diffed on every scan.
func (c *Core) trackedChurn(cwd string, status porcelainStatus, scanStart time.Time, index indexSnapshot, early *earlyChurnDiff) (trackedChurnTotals, error) {
	early.wait()
	entries := status.entries
	totals := trackedChurnTotals{files: len(entries)}
	// Before the first commit there is nothing to diff against: the
	// churn is zero, which is what the panel shows there.
	if len(entries) == 0 || !status.hasCommit() {
		c.trackedChurnMemos.store(cwd, trackedChurnMemo{warm: true}, scanStart)
		return totals, nil
	}
	context, keyed := c.churnContext(cwd, status, scanStart, early)
	prev, _ := c.trackedChurnMemos.snapshot(cwd)
	if !keyed || prev.context != context {
		prev = trackedChurnMemo{}
	}
	next := trackedChurnMemo{context: context, warm: keyed, files: make(map[string]trackedFileChurn, len(prev.files))}

	// pending holds the in-place entries this scan diffs, each with the
	// key its result is stored under.
	type pendingFile struct {
		entry *statusEntry
		key   trackedFileKey
		keyed bool
		// What the diff printed for it, when it printed anything.
		insertions, deletions int
		printed               bool
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
			if keyed {
				file.key, file.keyed = trackedFileKeyOf(cwd, entry, scanStart)
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
	if keyed {
		groupKey, groupKeyed = trackedGroupKeyOf(cwd, status.oid, groupEntries, scanStart)
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
		c.trackedChurnMemos.store(cwd, next, scanStart)
		return totals, nil
	}

	var result commandResult
	if early.usableFor(status.oid) {
		result = early.result
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
			if file.keyed {
				file.insertions, file.deletions, file.printed = record.insertions, record.deletions, true
				pending[record.path] = file
			}
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
			// Summed for nothing when a whole-tree diff repeats a group
			// this scan took from the memo.
			group.insertions += record.insertions
			group.deletions += record.deletions
			if !groupPaths[record.path] {
				extraFiles[record.path] = struct{}{}
			}
			continue
		}
		// A path that changed after status was read. Count it; the
		// event that changed it brings the scan that keys it.
		totals.insertions += record.insertions
		totals.deletions += record.deletions
		totals.files++
	}
	sameIndex := index.unchanged(scanStart)
	for path, file := range pending {
		if !file.keyed || !sameIndex {
			continue
		}
		// A file written while the diff ran may be the one the diff read.
		if after, ok := trackedFileKeyOf(cwd, file.entry, scanStart); !ok || after != file.key {
			continue
		}
		// An in-place path the diff did not print equals HEAD.
		stored := newTrackedFileChurn(path, file.key, file.insertions, file.deletions)
		next.files[stored.path] = stored
	}
	if groupPending {
		group.extraFiles = len(extraFiles)
		totals.insertions += group.insertions
		totals.deletions += group.deletions
		totals.files += group.extraFiles
		if after, ok := trackedGroupKeyOf(cwd, status.oid, groupEntries, scanStart); sameIndex && groupKeyed && ok && after == groupKey {
			next.group = &group
		}
	}
	c.trackedChurnMemos.store(cwd, next, scanStart)
	return totals, nil
}

// trackedFileKeyOf returns the key of an in-place entry, and false when
// its working tree file has not been left alone since before the scan
// began.
func trackedFileKeyOf(cwd string, entry *statusEntry, scanStart time.Time) (trackedFileKey, bool) {
	stat, state := statWorktreeFile(filepath.Join(cwd, filepath.FromSlash(entry.path)), scanStart, false)
	head, headOK := parseObjectID(entry.head)
	if state != worktreeFileSettled || !headOK {
		return trackedFileKey{}, false
	}
	if entry.xy[1] == '.' {
		// Status compared the file to the index and found it equal, and
		// it has not been written since.
		index, indexOK := parseObjectID(entry.index)
		return trackedFileKey{head: head, index: index}, indexOK
	}
	return trackedFileKey{head: head, stat: stat}, true
}

// trackedGroupKeyOf keys both sides of every group entry's diff against
// commit, and returns false when one of them cannot be identified. An
// unmerged entry's record names its stages and not the commit's blob, so
// the commit is part of the key.
func trackedGroupKeyOf(cwd string, commit string, entries []*statusEntry, scanStart time.Time) ([sha256.Size]byte, bool) {
	h := sha256.New()
	_, _ = io.WriteString(h, commit)
	keyed := true
	for _, entry := range entries {
		if !hashGroupEntry(h, cwd, entry, scanStart) {
			keyed = false
		}
	}
	return [sha256.Size]byte(h.Sum(nil)), keyed
}

// hashGroupEntry writes what identifies both sides of a group entry's
// diff into h, and returns false when the entry cannot be identified.
func hashGroupEntry(h hash.Hash, cwd string, entry *statusEntry, scanStart time.Time) bool {
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
	return hashWorktreeFile(h, filepath.Join(cwd, filepath.FromSlash(entry.path)), scanStart, false) != worktreeFileUnsettled
}

// numstatRecord is one path of `git diff --numstat -z`. oldPath is set on
// a rename, whose counts belong to path.
type numstatRecord struct {
	insertions, deletions int
	path, oldPath         string
}

// numstatRecords parses `git diff --numstat -z` output: a record is
// "<insertions>\t<deletions>\t<path>\0", and a rename leaves the path
// empty and follows with "<old>\0<new>\0". A binary file's counts are "-"
// and read as zero.
func numstatRecords(stdout string) func(yield func(numstatRecord) bool) {
	return func(yield func(numstatRecord) bool) {
		rest := stdout
		for rest != "" {
			var head string
			head, rest, _ = strings.Cut(rest, "\x00")
			insertions, tail, ok := strings.Cut(head, "\t")
			deletions, path, ok2 := strings.Cut(tail, "\t")
			if !ok || !ok2 {
				continue
			}
			record := numstatRecord{insertions: numstatCount(insertions), deletions: numstatCount(deletions), path: path}
			if record.path == "" {
				record.oldPath, rest, _ = strings.Cut(rest, "\x00")
				record.path, rest, _ = strings.Cut(rest, "\x00")
			}
			if !yield(record) {
				return
			}
		}
	}
}

func numstatCount(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}
