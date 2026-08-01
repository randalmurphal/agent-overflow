// Package workspacefiles finds files inside a workspace for @-mention
// completion. For git repositories we defer to `git ls-files` so ignore rules
// are honoured (both the user's .gitignore and gh exclude-standard chains);
// outside a git repo we fall back to a plain filesystem walk. A short TTL
// cache keeps the popover responsive.
package workspacefiles

import (
	"container/heap"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTTL         = 15 * time.Second
	DefaultMaxEntries  = 25_000
	DefaultResultLimit = 200
)

// IgnoredDirs is the tight whitelist of directory names we never descend into
// during the non-git fallback walk. Git-backed workspaces defer to the user's
// .gitignore, but we still filter these out unconditionally so a repo that
// accidentally committed node_modules/ or dist/ doesn't drown the @-picker.
var IgnoredDirs = map[string]struct{}{
	".git":         {},
	".convex":      {},
	".next":        {},
	".turbo":       {},
	".cache":       {},
	"node_modules": {},
	"dist":         {},
	"build":        {},
	"out":          {},
	"target":       {},
}

// gitCommand is overridable in tests so we can inject a fake `git ls-files`
// without shelling out. It returns a *exec.Cmd configured for the given cwd.
var gitCommand = func(ctx context.Context, cwd string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	return cmd
}

// WorkspaceFile is one search result. Paths are POSIX slashes regardless of
// host OS so the frontend can treat them uniformly.
type WorkspaceFile struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	ParentPath string `json:"parentPath,omitempty"`
}

type searchableEntry struct {
	WorkspaceFile
	normalizedPath string
	normalizedName string
}

type workspaceIndex struct {
	scannedAt time.Time
	entries   []searchableEntry
	truncated bool
}

// Searcher holds per-workspace cached indices. Safe for concurrent use from
// multiple goroutines — callers typically share one across the app.
type Searcher struct {
	ttl        time.Duration
	maxEntries int

	mu      sync.Mutex
	indices map[string]*workspaceIndex
}

// Config customises limits; zero values fall back to defaults.
type Config struct {
	TTL        time.Duration
	MaxEntries int
}

// NewSearcher returns a ready-to-use Searcher.
func NewSearcher(cfg Config) *Searcher {
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	max := cfg.MaxEntries
	if max == 0 {
		max = DefaultMaxEntries
	}
	return &Searcher{
		ttl:        ttl,
		maxEntries: max,
		indices:    make(map[string]*workspaceIndex),
	}
}

// Invalidate forgets any cached index for the given workspace.
func (s *Searcher) Invalidate(root string) {
	s.mu.Lock()
	delete(s.indices, root)
	s.mu.Unlock()
}

// Search returns up to `limit` workspace files matching `query`. A zero limit
// falls back to DefaultResultLimit so the frontend can ask for "sensible
// default" without knowing the number.
func (s *Searcher) Search(root, query string, limit int) ([]WorkspaceFile, bool, error) {
	if root == "" {
		return nil, false, fmt.Errorf("workspacefiles: root is required")
	}
	if limit <= 0 {
		limit = DefaultResultLimit
	}

	idx, err := s.getIndex(root)
	if err != nil {
		return nil, false, err
	}

	normalized := normalizeQuery(query)
	ranked := rankEntries(idx.entries, normalized, limit)
	files := make([]WorkspaceFile, len(ranked))
	for i, r := range ranked {
		files[i] = r.entry.WorkspaceFile
	}
	truncated := idx.truncated || len(idx.entries) > limit && len(ranked) == limit
	return files, truncated, nil
}

func (s *Searcher) getIndex(root string) (*workspaceIndex, error) {
	s.mu.Lock()
	idx := s.indices[root]
	if idx != nil && time.Since(idx.scannedAt) < s.ttl {
		s.mu.Unlock()
		return idx, nil
	}
	s.mu.Unlock()

	built, err := buildIndex(root, s.maxEntries)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.indices[root] = built
	// Sweep expired siblings so an idle or closed workspace's index (up to
	// maxEntries entries, several MB for a large repo) doesn't stay resident
	// for the process lifetime. Observationally free: an index past the TTL
	// is never served — getIndex always rebuilds — so dropping it only
	// releases memory.
	now := time.Now()
	for key, idx := range s.indices {
		if key != root && now.Sub(idx.scannedAt) >= s.ttl {
			delete(s.indices, key)
		}
	}
	s.mu.Unlock()
	return built, nil
}

// buildIndex validates the root and picks the best indexing strategy: git
// ls-files (if the workspace is inside a git repo) or a plain filesystem walk.
// The git path is both faster on large repos and honours the user's .gitignore.
func buildIndex(root string, maxEntries int) (*workspaceIndex, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("workspacefiles: stat root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspacefiles: root %s is not a directory", root)
	}

	if isGitRepo(root) {
		if idx, err := buildIndexFromGit(root, maxEntries); err == nil {
			return idx, nil
		}
		// Fall back to the filesystem walk on git failure so a broken git
		// install or corrupted repo doesn't leave the user without a picker.
	}

	return buildIndexFromWalk(root, maxEntries)
}

// isGitRepo reports whether root is inside a git repository by looking for a
// `.git` entry at the workspace root. `.git` can be a directory (normal
// checkout) or a file (worktree / submodule).
func isGitRepo(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

// buildIndexFromGit runs `git ls-files --cached --others --exclude-standard`
// to enumerate every file that is either tracked or untracked-but-not-ignored,
// then synthesises directory entries so '@src' can still surface `src/`.
// IgnoredDirs is applied on top of git's filtering as belt-and-braces — a repo
// that accidentally commits node_modules/ still stays out of the picker.
func buildIndexFromGit(root string, maxEntries int) (*workspaceIndex, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := gitCommand(ctx, root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("workspacefiles: git ls-files in %s: %w", root, err)
	}

	rawPaths := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	sort.Strings(rawPaths)

	// Sets so a directory isn't emitted twice when multiple files share a
	// parent. Entries are produced in a single pass at the end.
	dirSet := make(map[string]struct{})
	var files []string
	for _, raw := range rawPaths {
		if raw == "" {
			continue
		}
		rel := filepath.ToSlash(raw)
		if pathContainsIgnoredDir(rel) {
			continue
		}
		files = append(files, rel)
		for d := parentOf(rel); d != ""; d = parentOf(d) {
			dirSet[d] = struct{}{}
		}
	}

	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var entries []searchableEntry
	truncated := false

	// Emit directories first so short queries (e.g. "src") still surface
	// directory results even when file entries would fill the cap.
	for _, d := range dirs {
		entries = append(entries, makeEntry(d, "directory"))
		if len(entries) >= maxEntries {
			truncated = true
			break
		}
	}
	if !truncated {
		for _, f := range files {
			entries = append(entries, makeEntry(f, "file"))
			if len(entries) >= maxEntries {
				truncated = true
				break
			}
		}
	}

	return &workspaceIndex{
		scannedAt: time.Now(),
		entries:   entries,
		truncated: truncated,
	}, nil
}

// pathContainsIgnoredDir returns true when any segment of the path is in the
// IgnoredDirs set, so a committed node_modules/ is still skipped.
func pathContainsIgnoredDir(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if _, skip := IgnoredDirs[seg]; skip {
			return true
		}
	}
	return false
}

func makeEntry(rel, kind string) searchableEntry {
	name := rel
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		name = rel[idx+1:]
	}
	return searchableEntry{
		WorkspaceFile: WorkspaceFile{
			Path:       rel,
			Kind:       kind,
			ParentPath: parentOf(rel),
		},
		normalizedPath: strings.ToLower(rel),
		normalizedName: strings.ToLower(name),
	}
}

// buildIndexFromWalk walks root breadth-first, skipping ignored directories,
// and stops when we've collected maxEntries. Used when the workspace is not a
// git repo (or when git fails for some reason). Directories are included in
// the result so '@src' can surface `src/` too.
func buildIndexFromWalk(root string, maxEntries int) (*workspaceIndex, error) {
	var entries []searchableEntry
	truncated := false
	queue := []string{""}

	for len(queue) > 0 && !truncated {
		current := queue[0]
		queue = queue[1:]

		dirPath := filepath.Join(root, current)
		children, readErr := os.ReadDir(dirPath)
		if readErr != nil {
			// A single directory failing to read shouldn't kill the whole
			// scan (e.g. permission issues). Skip and continue.
			continue
		}

		// Sort for deterministic output.
		sort.Slice(children, func(i, j int) bool {
			return children[i].Name() < children[j].Name()
		})

		for _, child := range children {
			name := child.Name()
			if name == "" || name == "." || name == ".." {
				continue
			}
			isDir := child.IsDir()
			if isDir {
				if _, skip := IgnoredDirs[name]; skip {
					continue
				}
			} else if !child.Type().IsRegular() {
				continue
			}

			rel := filepath.ToSlash(filepath.Join(current, name))
			kind := "file"
			if isDir {
				kind = "directory"
			}
			entries = append(entries, makeEntry(rel, kind))
			if len(entries) >= maxEntries {
				truncated = true
				break
			}
			if isDir {
				queue = append(queue, rel)
			}
		}
	}

	return &workspaceIndex{
		scannedAt: time.Now(),
		entries:   entries,
		truncated: truncated,
	}, nil
}

func parentOf(relativePath string) string {
	idx := strings.LastIndex(relativePath, "/")
	if idx < 0 {
		return ""
	}
	return relativePath[:idx]
}

func normalizeQuery(query string) string {
	trimmed := strings.TrimSpace(query)
	trimmed = strings.TrimLeft(trimmed, "@./")
	return strings.ToLower(trimmed)
}

type rankedEntry struct {
	entry searchableEntry
	score int
}

// rankBetter is the total order rankEntries returns results in: score
// ascending (lower = better), ties broken by path. Paths are unique within an
// index (git ls-files output plus derived parent dirs cannot collide), so
// this is a strict total order and stability adds nothing.
func rankBetter(a, b rankedEntry) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	return a.entry.Path < b.entry.Path
}

// topKHeap is a max-heap under rankBetter: the root is the worst entry kept,
// so a better candidate replaces it in O(log limit).
type topKHeap []rankedEntry

func (h topKHeap) Len() int           { return len(h) }
func (h topKHeap) Less(i, j int) bool { return rankBetter(h[j], h[i]) }
func (h topKHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *topKHeap) Push(x any)        { *h = append(*h, x.(rankedEntry)) }
func (h *topKHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// rankEntries scores every entry and returns the top `limit` ordered by best
// match first. Bounded top-k selection instead of a full sort: search runs on
// every @-picker keystroke, and a short query matches most of the index (up
// to maxEntries), so sorting everything to keep `limit` entries would pay
// O(n log n) per keystroke. Because rankBetter is a total order, the output
// is byte-identical to sorting all matches and truncating.
func rankEntries(entries []searchableEntry, query string, limit int) []rankedEntry {
	if limit <= 0 {
		return nil
	}
	// Cap the pre-allocation by the entry count: limit arrives over the wire
	// and must not size an allocation on its own.
	worst := make(topKHeap, 0, min(limit, len(entries)))
	for i := range entries {
		score := scoreEntry(entries[i], query)
		if score < 0 {
			continue
		}
		candidate := rankedEntry{entry: entries[i], score: score}
		if len(worst) < limit {
			heap.Push(&worst, candidate)
			continue
		}
		if rankBetter(candidate, worst[0]) {
			worst[0] = candidate
			heap.Fix(&worst, 0)
		}
	}
	ranked := []rankedEntry(worst)
	sort.Slice(ranked, func(i, j int) bool { return rankBetter(ranked[i], ranked[j]) })
	return ranked
}

// scoreEntry returns a small integer where lower = better. A -1 return
// means "no match".
func scoreEntry(entry searchableEntry, query string) int {
	if query == "" {
		if entry.WorkspaceFile.Kind == "directory" {
			return 0
		}
		return 1
	}
	normalizedPath := entry.normalizedPath
	normalizedName := entry.normalizedName

	switch {
	case normalizedName == query:
		return 0
	case normalizedPath == query:
		return 1
	case strings.HasPrefix(normalizedName, query):
		return 2
	case strings.HasPrefix(normalizedPath, query):
		return 3
	case strings.Contains(normalizedPath, "/"+query):
		return 4
	case strings.Contains(normalizedName, query):
		return 5
	case strings.Contains(normalizedPath, query):
		return 6
	}

	if score := subsequenceScore(normalizedName, query); score >= 0 {
		return 100 + score
	}
	if score := subsequenceScore(normalizedPath, query); score >= 0 {
		return 200 + score
	}
	return -1
}

// subsequenceScore returns the total penalty for matching query as a
// subsequence of value (-1 if not a subsequence).
func subsequenceScore(value, query string) int {
	if query == "" {
		return 0
	}
	queryIndex := 0
	firstMatch := -1
	previousMatch := -1
	gap := 0
	for i := 0; i < len(value); i++ {
		if value[i] != query[queryIndex] {
			continue
		}
		if firstMatch == -1 {
			firstMatch = i
		}
		if previousMatch != -1 {
			gap += i - previousMatch - 1
		}
		previousMatch = i
		queryIndex++
		if queryIndex == len(query) {
			span := i - firstMatch + 1 - len(query)
			length := len(value) - len(query)
			if length > 64 {
				length = 64
			}
			return firstMatch*2 + gap*3 + span + length
		}
	}
	return -1
}
