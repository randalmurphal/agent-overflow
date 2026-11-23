package git

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"agent-overflow/internal/gitdiff"
)

// Which objects of a repository are local, read with commands that fetch
// on no git: rev-list with --missing=print, and ls-tree, which reads trees
// and never blobs. The facts reads ask cat-file only about the objects
// these found local. Asked about a promised object that is not, a git that
// does not know GIT_NO_LAZY_FETCH starts a fetch, and git 2.43 as Ubuntu
// 24.04 ships it is refused the fetch and dies with its whole batch.

// localObjectsMaxBytes bounds one listing of localObjects or pathEntries.
const localObjectsMaxBytes = 4 << 20

// literalPathspecs makes git read each path argument as that path.
var literalPathspecs = []string{"GIT_LITERAL_PATHSPECS=1"}

// commitSet is the full ids of local commits, sorted.
type commitSet []string

// has is whether id names a commit of the set: its full id, or the 40
// digits that abbreviate one in a sha256 repository.
func (s commitSet) has(id string) bool {
	i, _ := slices.BinarySearch(s, id)
	return i < len(s) && strings.HasPrefix(s[i], id)
}

// localCommits answers which of ids, each a full object id, name a commit
// that is local, in one `git rev-list --no-walk`: an id that names no
// local object, or one that is no commit, is skipped (--ignore-missing).
func (c *Core) localCommits(ctx context.Context, cwd string, ids []string) (commitSet, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var stdin strings.Builder
	for _, id := range ids {
		if !gitdiff.IsFullOID(id) {
			return nil, errors.New("rev-list: not a full object ID")
		}
		stdin.WriteString(id)
		stdin.WriteString("^{commit}\n")
	}
	result, err := c.factGit(ctx, cwd, commandSpec{
		stdin:    stdin.String(),
		maxBytes: int64(len(ids))*128 + 1<<10,
		args:     []string{"rev-list", "--no-walk", "--missing=print", "--ignore-missing", "--stdin"},
	})
	if err != nil {
		return nil, err
	}
	var local commitSet
	for line := range strings.SplitSeq(result.stdout, "\n") {
		switch {
		case line == "", strings.HasPrefix(line, "?"):
		case gitdiff.IsFullOID(line):
			local = append(local, line)
		default:
			return nil, errors.New("rev-list --no-walk: malformed answer")
		}
	}
	slices.Sort(local)
	return local, nil
}

// pathRuns calls run with paths in batches of at most
// gitdiff.MaxEntryPaths paths totaling at most gitdiff.MaxEntryPathBytes
// (the paths ride argv). A path is at most 4096 bytes (gitdiff.CleanPath),
// so each batch takes at least one.
func pathRuns(paths []string, run func(batch []string) error) error {
	for start := 0; start < len(paths); {
		end, total := start, 0
		for end < len(paths) && end-start < gitdiff.MaxEntryPaths && (end == start || total+len(paths[end]) <= gitdiff.MaxEntryPathBytes) {
			total += len(paths[end])
			end++
		}
		if err := run(paths[start:end]); err != nil {
			return err
		}
		start = end
	}
	return nil
}

// pathEntry is what a commit's tree holds at a path: its kind ("blob",
// "tree", or "commit" for a submodule) and, for a blob, its id. A path the
// tree lacks has neither.
type pathEntry struct{ kind, oid string }

// pathEntries answers commit's tree's entry at each of paths, in `git
// ls-tree` runs (pathRuns). A path names its own entry: a directory
// answers as a tree, never as what it holds. Git reads only the trees on
// the way to the paths, and lists no more than those trees' entries (a
// directory named beside a path under it lists whole).
func (c *Core) pathEntries(ctx context.Context, cwd, commit string, paths []string) (map[string]pathEntry, error) {
	named := make(map[string]bool, len(paths))
	for _, path := range paths {
		named[path] = true
	}
	entries := make(map[string]pathEntry, len(paths))
	err := pathRuns(paths, func(batch []string) error {
		args := append([]string{"ls-tree", "-z", "--full-tree", commit, "--"}, batch...)
		result, err := c.factGit(ctx, cwd, commandSpec{extraEnv: literalPathspecs, maxBytes: localObjectsMaxBytes, args: args})
		if err != nil {
			return err
		}
		for record := range strings.SplitSeq(result.stdout, "\x00") {
			if record == "" {
				continue
			}
			// "<mode> <kind> <oid>\t<path>"
			meta, path, ok := strings.Cut(record, "\t")
			fields := strings.Fields(meta)
			if !ok || len(fields) != 3 || !gitdiff.IsFullOID(fields[2]) {
				return errors.New("ls-tree: malformed entry")
			}
			switch {
			case !named[path]:
			case fields[1] == "blob":
				entries[path] = pathEntry{kind: "blob", oid: fields[2]}
			default:
				entries[path] = pathEntry{kind: fields[1]}
			}
			// Git lists a directory named beside a path under it as its
			// entries, without its own: an entry under a named path makes
			// that path a tree.
			for i := 0; i < len(path); i++ {
				if path[i] == '/' && named[path[:i]] {
					entries[path[:i]] = pathEntry{kind: "tree"}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// localObjects answers the ids of the local objects commit's tree holds at
// paths and on the way to them, in `git rev-list --objects
// --missing=print` runs over the tree (pathRuns). The walk reads trees: an
// object that is not local is printed as missing. Each of paths names a
// blob (pathEntries); a directory would list everything under it. An id
// answers for every path that holds it: git lists an object once, under
// one of its paths.
func (c *Core) localObjects(ctx context.Context, cwd, commit string, paths []string) (map[string]bool, error) {
	local := make(map[string]bool, len(paths))
	err := pathRuns(paths, func(batch []string) error {
		args := append([]string{"rev-list", "--objects", "--missing=print", "--no-walk", commit + "^{tree}", "--"}, batch...)
		result, err := c.factGit(ctx, cwd, commandSpec{extraEnv: literalPathspecs, maxBytes: localObjectsMaxBytes, args: args})
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(result.stdout, "\n") {
			// "<oid> <path>" for an object that is local, "?<oid>" for
			// one that is not.
			if oid, _, _ := strings.Cut(line, " "); gitdiff.IsFullOID(oid) {
				local[oid] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return local, nil
}

// perCommit answers read for each commit's paths, factImageReads commits
// at a time, and the error of each commit whose read failed.
func perCommit[T any](pathsOf map[string][]string, read func(commit string, paths []string) (T, error)) (map[string]T, map[string]error) {
	var (
		answers = make(map[string]T, len(pathsOf))
		errs    = map[string]error{}
		mu      sync.Mutex
		wg      sync.WaitGroup
		slots   = make(chan struct{}, factImageReads)
	)
	for commit, paths := range pathsOf {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			answer, err := read(commit, paths)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[commit] = fmt.Errorf("%d paths: %w", len(paths), err)
				return
			}
			answers[commit] = answer
		})
	}
	wg.Wait()
	return answers, errs
}
