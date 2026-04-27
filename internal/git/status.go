package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GitStatus summarizes repository state for a thread workspace.
type GitStatus struct {
	IsRepo          bool   `json:"isRepo"`
	Branch          string `json:"branch"`
	IsDefaultBranch bool   `json:"isDefaultBranch"`
	HasChanges      bool   `json:"hasChanges"`
	Insertions      int    `json:"insertions"`
	Deletions       int    `json:"deletions"`
	FileCount       int    `json:"fileCount"`
	HasUpstream     bool   `json:"hasUpstream"`
	AheadCount      int    `json:"aheadCount"`
	BehindCount     int    `json:"behindCount"`
	HasOriginRemote bool   `json:"hasOriginRemote"`
	OpenPRURL       string `json:"openPrUrl,omitempty"`
	OpenPRNumber    int    `json:"openPrNumber,omitempty"`
	// PendingOperation surfaces any in-progress multi-step operation that
	// blocks new commits. Values: "merge", "rebase", "bisect", or "" when
	// the repo is clean. Callers gate Ship Changes on this being empty.
	PendingOperation string `json:"pendingOperation,omitempty"`
}

// Equal reports whether two GitStatus values are field-by-field
// identical. Used on hot paths (gitwatch dedup at 250ms cadence) where
// reflect.DeepEqual would add reflection overhead — and would silently
// keep working if the struct grew a pointer/map/slice field that the
// caller forgot to consider. A typed Equal forces every new field to
// be added here too.
func (s GitStatus) Equal(other GitStatus) bool {
	return s.IsRepo == other.IsRepo &&
		s.Branch == other.Branch &&
		s.IsDefaultBranch == other.IsDefaultBranch &&
		s.HasChanges == other.HasChanges &&
		s.Insertions == other.Insertions &&
		s.Deletions == other.Deletions &&
		s.FileCount == other.FileCount &&
		s.HasUpstream == other.HasUpstream &&
		s.AheadCount == other.AheadCount &&
		s.BehindCount == other.BehindCount &&
		s.HasOriginRemote == other.HasOriginRemote &&
		s.OpenPRURL == other.OpenPRURL &&
		s.OpenPRNumber == other.OpenPRNumber &&
		s.PendingOperation == other.PendingOperation
}

// GitBranch represents a local or remote branch entry.
type GitBranch struct {
	Name         string `json:"name"`
	IsRemote     bool   `json:"isRemote"`
	IsCurrent    bool   `json:"isCurrent"`
	IsDefault    bool   `json:"isDefault"`
	WorktreePath string `json:"worktreePath,omitempty"`
}

// Status reads repository status using porcelain v2 output.
func (c *Core) Status(cwd string) (GitStatus, error) {
	result, err := c.run(cwd, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return GitStatus{}, err
	}
	if result.exitCode != 0 {
		if strings.Contains(strings.ToLower(result.stderr), "not a git repository") {
			return GitStatus{IsRepo: false}, nil
		}
		return GitStatus{}, fmt.Errorf("git status failed: %s", strings.TrimSpace(result.stderr))
	}

	unstagedNumstat, err := c.run(cwd, "diff", "--numstat")
	if err != nil {
		return GitStatus{}, err
	}
	stagedNumstat, err := c.run(cwd, "diff", "--cached", "--numstat")
	if err != nil {
		return GitStatus{}, err
	}

	defaultBranch, _ := c.defaultBranchName(cwd)
	hasOriginRemote := c.originRemoteExists(cwd)
	status := parseStatusOutput(result.stdout, unstagedNumstat.stdout, stagedNumstat.stdout)
	status.IsRepo = true
	status.HasOriginRemote = hasOriginRemote
	status.IsDefaultBranch = isDefaultBranchName(status.Branch, defaultBranch)
	status.PendingOperation = c.pendingOperation(cwd)

	if status.Branch != "" {
		status.OpenPRURL, status.OpenPRNumber = c.lookupOpenPR(cwd, status.Branch)
	}

	return status, nil
}

// pendingOperation detects an in-progress merge, rebase, or bisect by
// inspecting well-known files under the repository's git directory. Returns
// "" when the repo is idle.
//
// We use `git rev-parse --git-dir` to resolve the correct git-dir because it
// handles both plain repos (`.git` is a dir) and linked worktrees (`.git` is a
// file pointing into a `worktrees/<name>` dir). Checking for a literal `.git`
// folder under `cwd` would miss ongoing ops in worktrees.
func (c *Core) pendingOperation(cwd string) string {
	gitDir := c.resolveGitDir(cwd)
	if gitDir == "" {
		return ""
	}

	// MERGE_HEAD is present during an in-progress merge (including merge
	// conflicts waiting for resolution).
	if fileExists(filepath.Join(gitDir, "MERGE_HEAD")) {
		return "merge"
	}
	// rebase-merge / rebase-apply cover both interactive rebases and the
	// legacy format-patch style rebases (`rebase-apply`).
	if dirExists(filepath.Join(gitDir, "rebase-merge")) || dirExists(filepath.Join(gitDir, "rebase-apply")) {
		return "rebase"
	}
	// BISECT_LOG is present during a `git bisect` session.
	if fileExists(filepath.Join(gitDir, "BISECT_LOG")) {
		return "bisect"
	}
	return ""
}

// resolveGitDir runs `git rev-parse --git-dir` and returns the absolute git
// directory for the repository containing cwd. Returns "" if the directory
// isn't a git repo or rev-parse fails; callers treat "" as "no pending op"
// which is the correct fallback in both cases.
func (c *Core) resolveGitDir(cwd string) string {
	result, err := c.run(cwd, "rev-parse", "--git-dir")
	if err != nil || result.exitCode != 0 {
		return ""
	}
	gitDir := strings.TrimSpace(result.stdout)
	if gitDir == "" {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(cwd, gitDir)
	}
	return gitDir
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// WorkingTreeDiff returns the current diff against HEAD plus cached changes.
func (c *Core) WorkingTreeDiff(cwd string) (string, error) {
	headDiff, err := c.run(cwd, "diff", "HEAD")
	if err != nil {
		return "", err
	}
	if headDiff.exitCode != 0 {
		return "", fmt.Errorf("git diff HEAD failed: %s", strings.TrimSpace(headDiff.stderr))
	}

	cachedDiff, err := c.run(cwd, "diff", "--cached")
	if err != nil {
		return "", err
	}
	if cachedDiff.exitCode != 0 {
		return "", fmt.Errorf("git diff --cached failed: %s", strings.TrimSpace(cachedDiff.stderr))
	}

	return combineDiffs(headDiff.stdout, cachedDiff.stdout), nil
}

// ListBranches returns local and remote branches from git branch output.
func (c *Core) ListBranches(cwd string) ([]GitBranch, error) {
	result, err := c.run(cwd, "branch", "-a", "--format=%(refname:short)|%(HEAD)|%(worktreepath)")
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git branch failed: %s", strings.TrimSpace(result.stderr))
	}

	defaultBranch, _ := c.defaultBranchName(cwd)
	return parseBranchList(result.stdout, defaultBranch, c.listRemoteNames(cwd)), nil
}

// RepositoryRoot resolves the canonical git top-level directory for cwd.
func (c *Core) RepositoryRoot(cwd string) (string, error) {
	result, err := c.run(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("git rev-parse --show-toplevel failed: %s", strings.TrimSpace(result.stderr))
	}
	return strings.TrimSpace(result.stdout), nil
}

func parseStatusOutput(statusStdout, unstagedNumstat, stagedNumstat string) GitStatus {
	var status GitStatus
	paths := make(map[string]struct{})

	for _, line := range strings.Split(statusStdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			branch := strings.TrimSpace(strings.TrimPrefix(line, "# branch.head "))
			if !strings.HasPrefix(branch, "(") {
				status.Branch = branch
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			status.HasUpstream = strings.TrimSpace(strings.TrimPrefix(line, "# branch.upstream ")) != ""
		case strings.HasPrefix(line, "# branch.ab "):
			status.AheadCount, status.BehindCount = parseAheadBehind(strings.TrimSpace(strings.TrimPrefix(line, "# branch.ab ")))
		case !strings.HasPrefix(line, "#"):
			status.HasChanges = true
			if path := parsePorcelainPath(line); path != "" {
				paths[path] = struct{}{}
			}
		}
	}

	insertions, deletions := 0, 0
	for _, entry := range append(parseNumstat(unstagedNumstat), parseNumstat(stagedNumstat)...) {
		insertions += entry.Insertions
		deletions += entry.Deletions
		paths[entry.Path] = struct{}{}
	}

	status.Insertions = insertions
	status.Deletions = deletions
	status.FileCount = len(paths)
	return status
}

type numstatEntry struct {
	Path       string
	Insertions int
	Deletions  int
}

func parseNumstat(stdout string) []numstatEntry {
	var entries []numstatEntry
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}

		path := normalizeNumstatPath(parts[len(parts)-1])
		entries = append(entries, numstatEntry{
			Path:       path,
			Insertions: parseNumstatCount(parts[0]),
			Deletions:  parseNumstatCount(parts[1]),
		})
	}
	return entries
}

func parseNumstatCount(raw string) int {
	if raw == "-" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}

func normalizeNumstatPath(path string) string {
	if idx := strings.Index(path, " => "); idx >= 0 {
		return strings.TrimSpace(path[idx+4:])
	}
	return strings.TrimSpace(path)
}

func parseAheadBehind(raw string) (int, int) {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return 0, 0
	}
	ahead, _ := strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
	behind, _ := strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
	return ahead, behind
}

func parsePorcelainPath(line string) string {
	if strings.HasPrefix(line, "? ") || strings.HasPrefix(line, "! ") {
		return strings.TrimSpace(line[2:])
	}
	if idx := strings.Index(line, "\t"); idx >= 0 {
		fields := strings.Split(line[idx+1:], "\t")
		return strings.TrimSpace(fields[0])
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSpace(fields[len(fields)-1])
}

func combineDiffs(headDiff, cachedDiff string) string {
	headDiff = strings.TrimSpace(headDiff)
	cachedDiff = strings.TrimSpace(cachedDiff)

	if cachedDiff == "" {
		return headDiff
	}
	if headDiff == "" {
		return cachedDiff
	}
	return headDiff + "\n" + cachedDiff
}

func parseBranchList(stdout, defaultBranch string, remoteNames []string) []GitBranch {
	var branches []GitBranch
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" || strings.HasSuffix(name, "/HEAD") {
			continue
		}

		branches = append(branches, GitBranch{
			Name:         name,
			IsRemote:     isRemoteBranchName(name, remoteNames),
			IsCurrent:    strings.TrimSpace(parts[1]) == "*",
			IsDefault:    isDefaultBranchName(name, defaultBranch),
			WorktreePath: strings.TrimSpace(parts[2]),
		})
	}
	return branches
}

func isDefaultBranchName(branchName, defaultBranch string) bool {
	if branchName == "" {
		return false
	}

	if defaultBranch == "" {
		return branchName == "main" || branchName == "master" ||
			strings.HasSuffix(branchName, "/main") || strings.HasSuffix(branchName, "/master")
	}

	if branchName == defaultBranch {
		return true
	}
	return strings.HasSuffix(branchName, "/"+defaultBranch)
}

func (c *Core) defaultBranchName(cwd string) (string, error) {
	result, err := c.run(cwd, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "", nil
	}
	return strings.TrimPrefix(strings.TrimSpace(result.stdout), "refs/remotes/origin/"), nil
}

func (c *Core) originRemoteExists(cwd string) bool {
	result, err := c.run(cwd, "remote", "get-url", "origin")
	if err != nil {
		return false
	}
	return result.exitCode == 0
}

func (c *Core) listRemoteNames(cwd string) []string {
	result, err := c.run(cwd, "remote")
	if err != nil || result.exitCode != 0 {
		return nil
	}

	var names []string
	for _, line := range strings.Split(result.stdout, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func isRemoteBranchName(name string, remoteNames []string) bool {
	for _, remote := range remoteNames {
		if strings.HasPrefix(name, remote+"/") {
			return true
		}
	}
	return false
}

func (c *Core) lookupOpenPR(cwd, branch string) (string, int) {
	if branch == "" {
		return "", 0
	}
	key := prCacheKey(cwd, branch)
	now := c.nowFn()

	c.prCacheMu.Lock()
	if entry, ok := c.prCache[key]; ok && entry.expiresAt.After(now) {
		c.prCacheMu.Unlock()
		return entry.url, entry.number
	}
	c.prCacheMu.Unlock()

	// Slow path: shell out. Done outside the lock — `gh pr list` is a
	// network call and we don't want to serialise unrelated lookups
	// behind it. A concurrent caller may double-fetch in the rare
	// race window; the second writer wins and both end up with the
	// same value.
	url, number := "", 0
	pulls, err := c.ListOpenPRs(cwd, branch)
	if err == nil && len(pulls) > 0 {
		url = pulls[0].URL
		number = pulls[0].Number
	}

	c.prCacheMu.Lock()
	c.prCache[key] = prCacheEntry{
		url:       url,
		number:    number,
		expiresAt: now.Add(prLookupTTL),
	}
	c.prCacheMu.Unlock()
	return url, number
}

// InvalidatePRCache drops every cached open-PR entry for cwd. Call
// after a successful CreatePR (or any action that materially changes
// PR state) so the next status refresh sees the new PR immediately
// rather than waiting up to prLookupTTL.
func (c *Core) InvalidatePRCache(cwd string) {
	prefix := cwd + "\x00"
	c.prCacheMu.Lock()
	defer c.prCacheMu.Unlock()
	for key := range c.prCache {
		if strings.HasPrefix(key, prefix) {
			delete(c.prCache, key)
		}
	}
}
