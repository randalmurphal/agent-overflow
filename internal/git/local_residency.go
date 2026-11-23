package git

import (
	"bytes"
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
//
// What these answer is copied out of git's output: an id that a cache
// holds never keeps a listing alive.

// commitSet is the full ids of local commits, sorted.
type commitSet []string

// has is whether id names a commit of the set: its full id, or the 40
// digits that abbreviate one in a sha256 repository.
func (s commitSet) has(id string) bool {
	i, _ := slices.BinarySearch(s, id)
	return i < len(s) && strings.HasPrefix(s[i], id)
}

// fullIDs is ids, each a full object id, one to a line with suffix.
func fullIDs(ids []string, suffix string) (string, error) {
	var lines strings.Builder
	for _, id := range ids {
		if !gitdiff.IsFullOID(id) {
			return "", errors.New("not a full object ID")
		}
		lines.WriteString(id)
		lines.WriteString(suffix)
		lines.WriteByte('\n')
	}
	return lines.String(), nil
}

// idLines bounds a listing of ids: a line each, as a full id and its
// marks, and no more.
func idLines(ids []string) int64 { return int64(len(ids))*72 + 1<<10 }

// localCommits answers which of ids, each a full object id, name a commit
// that is local, in one `git rev-list --no-walk`: an id that names no
// local object, or one that is no commit, is skipped (--ignore-missing).
func (c *Core) localCommits(ctx context.Context, cwd string, ids []string) (commitSet, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	stdin, err := fullIDs(ids, "^{commit}")
	if err != nil {
		return nil, fmt.Errorf("rev-list --no-walk: %w", err)
	}
	result, err := c.factGit(ctx, cwd, commandSpec{
		stdin:    stdin,
		maxBytes: idLines(ids),
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
			local = append(local, strings.Clone(line))
		default:
			return nil, errors.New("rev-list --no-walk: malformed answer")
		}
	}
	slices.Sort(local)
	return local, nil
}

// localObjects answers which of ids, each a blob's full id, are local, in
// one `git rev-list --objects --no-walk`, which reads no blob: an id that
// names no local object is skipped, or printed as missing. A tree's or a
// commit's id lists what it holds: that fails the run, and git is stopped
// past the ids' own lines.
func (c *Core) localObjects(ctx context.Context, cwd string, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	stdin, err := fullIDs(ids, "")
	if err != nil {
		return nil, fmt.Errorf("rev-list --objects: %w", err)
	}
	var listing bytes.Buffer
	if _, err := c.factGit(ctx, cwd, commandSpec{
		stdin:       stdin,
		output:      &listing,
		outputLimit: idLines(ids),
		args:        []string{"rev-list", "--objects", "--no-walk", "--missing=print", "--ignore-missing", "--stdin"},
	}); err != nil {
		return nil, err
	}
	asked := make(map[string]bool, len(ids))
	for _, id := range ids {
		asked[id] = true
	}
	local := make(map[string]bool, len(ids))
	for line := range strings.SplitSeq(listing.String(), "\n") {
		// "<oid> " for an object that is local, "?<oid>" for one that is
		// not.
		oid, _, _ := strings.Cut(line, " ")
		switch {
		case !gitdiff.IsFullOID(oid):
		case !asked[oid]:
			return nil, errors.New("rev-list --objects: an id names a tree or a commit")
		default:
			local[strings.Clone(oid)] = true
		}
	}
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

const (
	// pathEntriesMaxBytes stops a `git ls-tree` that lists more than any
	// directory holds. The listing is read as it arrives and none of it
	// is held, so the bound is on git's work alone.
	pathEntriesMaxBytes = 256 << 20
	// pathEntryMaxBytes is the longest one entry of a listing is read.
	pathEntryMaxBytes = 16 << 10
)

// pathEntries answers commit's tree's entry at each of paths, in `git
// ls-tree` runs (pathRuns). A path names its own entry: a directory
// answers as a tree, never as what it holds. Git reads the trees on the
// way to the paths and lists no more than those trees' entries: it lists
// a directory named beside a path under it whole, so what it prints is
// read as it arrives (entryStream). A tree on the way that is not local
// fails the commit's read.
func (c *Core) pathEntries(ctx context.Context, cwd, commit string, paths []string) (map[string]pathEntry, error) {
	stream := &entryStream{named: make(map[string]bool, len(paths)), entries: make(map[string]pathEntry, len(paths))}
	for _, path := range paths {
		stream.named[path] = true
	}
	err := pathRuns(paths, func(batch []string) error {
		args := append([]string{"ls-tree", "-z", "--full-tree", commit, "--"}, batch...)
		_, err := c.factGit(ctx, cwd, commandSpec{
			extraEnv: []string{"GIT_LITERAL_PATHSPECS=1"},
			output:   stream, outputLimit: pathEntriesMaxBytes, args: args,
		})
		if err == nil && len(stream.record) != 0 {
			err = errors.New("ls-tree: the listing ends inside an entry")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return stream.entries, nil
}

// entryStream reads `git ls-tree -z` output as it arrives and keeps the
// entries of the named paths, in strings of their own.
type entryStream struct {
	named   map[string]bool
	entries map[string]pathEntry
	// record is the start of an entry a write ended inside.
	record []byte
}

func (s *entryStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		record := p
		if i >= 0 {
			record = p[:i]
		}
		if len(s.record)+len(record) > pathEntryMaxBytes {
			return 0, errors.New("ls-tree: an entry is too long")
		}
		if i < 0 {
			s.record = append(s.record, record...)
			break
		}
		if len(s.record) > 0 {
			s.record = append(s.record, record...)
			record = s.record
		}
		if err := s.take(record); err != nil {
			return 0, err
		}
		s.record, p = s.record[:0], p[i+1:]
	}
	return n, nil
}

// take reads one entry: "<mode> <kind> <oid>\t<path>".
func (s *entryStream) take(record []byte) error {
	meta, path, ok := bytes.Cut(record, []byte{'\t'})
	fields := bytes.Fields(meta)
	if !ok || len(fields) != 3 || !gitdiff.IsFullOID(string(fields[2])) {
		return errors.New("ls-tree: malformed entry")
	}
	switch {
	case !s.named[string(path)]:
	case string(fields[1]) == "blob":
		s.entries[string(path)] = pathEntry{kind: "blob", oid: string(fields[2])}
	default:
		s.entries[string(path)] = pathEntry{kind: string(fields[1])}
	}
	// Git lists a directory named beside a path under it as its entries,
	// without its own: an entry under a named path makes that path a tree.
	for i, b := range path {
		if b == '/' && s.named[string(path[:i])] {
			s.entries[string(path[:i])] = pathEntry{kind: "tree"}
		}
	}
	return nil
}

// pathEntriesOf answers pathEntries for each commit's paths,
// factImageReads commits at a time, and the error of each commit whose
// read failed.
func (c *Core) pathEntriesOf(ctx context.Context, cwd string, pathsOf map[string][]string) (map[string]map[string]pathEntry, map[string]error) {
	var (
		entries = make(map[string]map[string]pathEntry, len(pathsOf))
		errs    = map[string]error{}
		mu      sync.Mutex
		wg      sync.WaitGroup
		slots   = make(chan struct{}, factImageReads)
	)
	for commit, paths := range pathsOf {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			at, err := c.pathEntries(ctx, cwd, commit, paths)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[commit] = fmt.Errorf("%d paths: %w", len(paths), err)
				return
			}
			entries[commit] = at
		})
	}
	wg.Wait()
	return entries, errs
}
