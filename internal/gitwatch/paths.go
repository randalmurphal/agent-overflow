package gitwatch

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gitops "agent-overflow/internal/git"
)

// canonicalize resolves p to an absolute, symlink-free path AND
// returns the absolute (pre-symlink) form so callers can refuse based
// on either. macOS /tmp goes through symlinks (/tmp → /private/tmp,
// /etc → /private/etc); without keeping both forms we'd miss a
// user-supplied "/etc" because canonicalize resolves it to "/private/etc"
// which isn't equal to "/etc" any more.
//
// Intentionally distinct from gitops.CanonicalPath: that helper does
// best-effort resolution and falls back to the original on error,
// because non-existent paths are a normal case for branch comparison /
// diff display. Subscribe-time MUST surface errors instead so a bad
// cwd doesn't quietly install a watcher rooted at the literal user
// input.
func canonicalize(p string) (abs, canon string, err error) {
	abs, err = filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	return abs, filepath.Clean(resolved), nil
}

// systemPaths are absolute paths (and the canonical form of paths that
// symlink elsewhere) the manager refuses to watch. The list is
// intentionally small — defense-in-depth against a workspace path that
// resolves to a system root, not a general allow-list. CreateThread is
// LocalOnly so the wire input is already trusted; this is a backstop
// for a misconfigured row, a developer-time mistake, or a future bug
// in path resolution.
var systemPaths = map[string]struct{}{
	"/":            {},
	"/etc":         {},
	"/var":         {},
	"/usr":         {},
	"/bin":         {},
	"/sbin":        {},
	"/opt":         {},
	"/private":     {},
	"/private/etc": {},
	"/private/var": {},
	"/System":      {},
	"/Library":     {},
	"/Volumes":     {},
	"/Users":       {},
	"/home":        {},
	"/proc":        {},
	"/sys":         {},
	"/dev":         {},
	`C:\`:          {},
	`C:\Windows`:   {},
	`C:\Users`:     {},
}

// rejectSystemPath returns an error if either the user-supplied
// absolute path OR its symlink-resolved canonical form is a system
// root a recursive fs watcher should never touch. Also refuses
// non-directories so a path that resolves to a device node or socket
// can't slip through.
func rejectSystemPath(abs, canon string) error {
	if _, blocked := systemPaths[abs]; blocked {
		return fmt.Errorf("gitwatch: refusing to watch system path %q", abs)
	}
	if _, blocked := systemPaths[canon]; blocked {
		return fmt.Errorf("gitwatch: refusing to watch system path %q", canon)
	}
	info, err := os.Stat(canon)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("gitwatch: cwd is not a directory")
	}
	return nil
}

func normalizeWatchRoots(cwd string, roots []gitops.WatchRoot) ([]gitops.WatchRoot, error) {
	if len(roots) == 0 {
		roots = []gitops.WatchRoot{{Path: cwd, Recursive: true}}
	}

	candidates := make([]gitops.WatchRoot, 0, len(roots)+1)
	indexByPath := make(map[string]int, len(roots)+1)
	// cwd itself is always a candidate so the workspace is watched even
	// if the roots list omits it — but non-recursively: when ignored-
	// subtree pruning is active, the supplied roots deliberately watch
	// cwd non-recursively, and forcing Recursive here would drag every
	// pruned subtree back in via the OR-merge below.
	for i, root := range append([]gitops.WatchRoot{{Path: cwd, Recursive: false}}, roots...) {
		path := strings.TrimSpace(root.Path)
		if path == "" {
			continue
		}
		abs, canon, err := canonicalize(path)
		if err != nil {
			// cwd (index 0) failing is fatal — there is no workspace to
			// watch. A pruned child root can legitimately vanish between
			// WatchRoots computing it and this normalization (an agent
			// deleting a directory); dropping it is correct — its parent
			// ancestor root sees the deletion and triggers a refresh.
			if i == 0 {
				return nil, err
			}
			continue
		}
		if err := rejectSystemPath(abs, canon); err != nil {
			if i == 0 {
				return nil, err
			}
			// Unlike a vanished dir, this drop is a policy decision, and
			// dropping a trigger-bearing root (e.g. an excludes file
			// directly under a refused path) silently reopens the
			// staleness hole its triggers exist to close — say so.
			if root.Kind != gitops.KindSubtree || root.TriggerFile != "" {
				log.Printf("gitwatch: dropping trigger-bearing watch root %s for %s: %v", path, cwd, err)
			}
			continue
		}
		if index, ok := indexByPath[canon]; ok {
			candidates[index].Recursive = candidates[index].Recursive || root.Recursive
			// Kinds are ordered by trigger surface (each kind's rebuild
			// triggers are a superset of the previous one's), so taking
			// the max preserves every trigger of both duplicates.
			// TriggerFile is orthogonal: keep the first non-empty (one
			// WatchRoots output never carries two different trigger
			// files for the same directory).
			candidates[index].Kind = max(candidates[index].Kind, root.Kind)
			if candidates[index].TriggerFile == "" {
				candidates[index].TriggerFile = root.TriggerFile
			}
			continue
		}
		indexByPath[canon] = len(candidates)
		candidates = append(candidates, gitops.WatchRoot{
			Path:        canon,
			Recursive:   root.Recursive,
			Kind:        root.Kind,
			TriggerFile: root.TriggerFile,
		})
	}
	if len(candidates) == 0 {
		return nil, errors.New("gitwatch: no valid watch roots")
	}

	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].Path) != len(candidates[j].Path) {
			return len(candidates[i].Path) < len(candidates[j].Path)
		}
		if candidates[i].Path != candidates[j].Path {
			return candidates[i].Path < candidates[j].Path
		}
		return candidates[i].Recursive && !candidates[j].Recursive
	})

	pruned := make([]gitops.WatchRoot, 0, len(candidates))
	for _, candidate := range candidates {
		// Only plain-content roots are dropped when a recursive root
		// already covers them. Trigger-bearing roots (ancestor, git
		// metadata, global-ignore dir) must survive coverage: the
		// watcher matches rebuild triggers against root paths, so
		// dropping one silences its triggers even though the covering
		// root still delivers the raw events. The duplicate watchpoint
		// costs one extra watch and some double-delivered events the
		// debounce absorbs.
		if candidate.Kind == gitops.KindSubtree && candidate.TriggerFile == "" &&
			isCoveredByAnyRoot(candidate, pruned) {
			continue
		}
		pruned = append(pruned, candidate)
	}
	return pruned, nil
}

func isCoveredByAnyRoot(path gitops.WatchRoot, roots []gitops.WatchRoot) bool {
	for _, root := range roots {
		if root.Recursive && sameOrDescendant(root.Path, path.Path) {
			return true
		}
	}
	return false
}

func sameOrDescendant(parent, child string) bool {
	if parent == child {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
