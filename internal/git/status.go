package git

import (
	"bytes"
	"fmt"
	"io"
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
	// Forge is the canonical id of the origin remote's forge:
	// "github", "gitlab", or "" when the host is unknown / no origin
	// is configured. Drives the UI's PR/MR label adaptation and gates
	// the "Create PR" action.
	Forge        string `json:"forge,omitempty"`
	OpenPRURL    string `json:"openPrUrl,omitempty"`
	OpenPRNumber int    `json:"openPrNumber,omitempty"`
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
		s.Forge == other.Forge &&
		s.OpenPRURL == other.OpenPRURL &&
		s.OpenPRNumber == other.OpenPRNumber &&
		s.PendingOperation == other.PendingOperation
}

// GitBranch is a branch the picker can offer for checkout. Remote-only
// branches are projected to their short name (e.g. "feature" rather than
// "origin/feature") so the UI presents a unified list — `git checkout
// <name>` against a name that exists only on a remote auto-creates the
// local tracking branch.
//
// AheadCount and BehindCount are populated from %(upstream:track) when
// the branch has a configured upstream. Remote-only projected entries
// (those without a local checkout) have no upstream relationship, so
// both counts stay 0.
type GitBranch struct {
	Name         string `json:"name"`
	IsCurrent    bool   `json:"isCurrent"`
	IsDefault    bool   `json:"isDefault"`
	WorktreePath string `json:"worktreePath,omitempty"`
	AheadCount   int    `json:"aheadCount,omitempty"`
	BehindCount  int    `json:"behindCount,omitempty"`
}

// Status reads repository status using porcelain v2 output.
func (c *Core) Status(cwd string) (GitStatus, error) {
	status, err := c.baseStatus(cwd)
	if err != nil || !status.IsRepo {
		return status, err
	}
	if status.Branch != "" {
		status.OpenPRURL, status.OpenPRNumber = c.lookupOpenPR(cwd, status.Branch)
	}
	return status, nil
}

// StatusFast is the same as Status but uses only cached PR info,
// avoiding the network call to gh/glab. Returns immediately even when
// the PR cache is cold — the caller is expected to arrange a follow-up
// full Status call that warms the cache and broadcasts the PR fields.
func (c *Core) StatusFast(cwd string) (GitStatus, error) {
	status, err := c.baseStatus(cwd)
	if err != nil || !status.IsRepo {
		return status, err
	}
	if status.Branch != "" {
		status.OpenPRURL, status.OpenPRNumber = c.lookupOpenPRCached(cwd, status.Branch)
	}
	return status, nil
}

// baseStatus gathers every status field shared by Status and StatusFast:
// branch and ahead/behind, workspace churn (insertions/deletions/files),
// detected forge, and any pending merge/rebase/bisect. The open-PR lookup is
// the only part that differs between the two callers (live network vs cache),
// so it stays with them; centralising the rest keeps the two entry points from
// drifting apart.
func (c *Core) baseStatus(cwd string) (GitStatus, error) {
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

	// Tracked churn measured against HEAD with the same diff the panel's
	// DiffWorkspaceVsHead produces (HEAD, --minimal --no-ext-diff --no-textconv):
	// numstat's per-file insertion/deletion counts equal the '+'/'-' content
	// lines the panel parses from --patch, so the header badge and the panel
	// report the same numbers. On a fresh repo with no HEAD this exits non-zero
	// with empty output (runBinary surfaces that as a result, not an error),
	// yielding zero tracked churn — which is exactly what the panel shows there.
	headNumstat, err := c.run(cwd, "diff", "--numstat", "--minimal", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return GitStatus{}, err
	}

	defaultBranch, _ := c.defaultBranchName(cwd)
	originURL, hasOriginRemote := c.originURL(cwd)
	forge := ""
	if hasOriginRemote {
		forge = classifyOriginURL(originURL, c.gitLabHostsSnapshot())
	}
	// Populate the forge cache so any concurrent DetectForge call (e.g. through
	// forgeFor → ListOpenPRs) reuses the same classification rather than
	// re-shelling `git remote get-url`.
	c.storeForgeCache(cwd, forge, c.nowFn())

	status := parseStatusOutput(result.stdout, headNumstat.stdout)
	// Untracked, non-ignored files count as all-insertions — the same files the
	// diff panel folds into its workspace total. They're counted individually
	// (git status collapses a wholly-untracked directory to one porcelain entry,
	// which parseStatusOutput excludes from FileCount to avoid undercounting).
	// Best-effort: any failure degrades to the tracked-only counts rather than
	// failing the whole refresh, matching how branch/forge/pending degrade above.
	untrackedIns, untrackedFiles := c.untrackedStats(cwd, maxUntrackedScanBytes)
	status.Insertions += untrackedIns
	status.FileCount += untrackedFiles
	status.IsRepo = true
	status.HasOriginRemote = hasOriginRemote
	status.Forge = forge
	status.IsDefaultBranch = isDefaultBranchName(status.Branch, defaultBranch)
	status.PendingOperation = c.pendingOperation(cwd)

	return status, nil
}

// maxUntrackedScanBytes caps how much untracked-file content untrackedStats
// reads while counting "new file" insertions. It is deliberately generous —
// agent-driven workspaces routinely carry hundreds of untracked source files —
// and exists only to bound a pathological un-ignored tree (a build/data
// directory someone forgot to ignore, or a multi-GB log) from stalling the
// gitwatch hot path. The file *count* is unaffected by the cap; only the line
// tally stops once the budget is spent.
//
// This bound differs from the panel's on purpose: DiffWorkspaceVsHead errors out
// at its 10MB patch cap, whereas the badge degrades (caps the line tally, keeps
// the file count). On a pathological tree the panel shows an error while the
// badge shows a bounded number — but the panel isn't displaying a count to match
// in that state anyway, so the divergence is invisible in practice.
const maxUntrackedScanBytes = 64 * 1024 * 1024

// untrackedStats returns the insertions and file count the diff panel
// attributes to untracked, non-ignored files in cwd: every such file is "added"
// whole versus /dev/null. Counting happens here in Go rather than via a `git
// diff` per file because gitwatch runs this every 250ms–1500ms and a workspace
// can hold hundreds of untracked files — one subprocess each would swamp the
// watcher. New files have no deletions, so only insertions are returned.
//
// budget caps the total content bytes read across the whole scan (the caller
// passes maxUntrackedScanBytes); the file count is never capped — only the line
// tally stops once budget is spent. Passing it in rather than reading the const
// directly lets tests drive the budget-exhausted path without a giant fixture.
//
// Best-effort by design: a failed enumeration or a file that vanishes mid-scan
// (routine while an agent is writing) is skipped, so the ambient badge degrades
// gracefully rather than erroring — matching how branch/forge/pending degrade
// in baseStatus.
func (c *Core) untrackedStats(cwd string, budget int) (insertions, files int) {
	listing, err := c.run(cwd, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || listing.exitCode != 0 {
		return 0, 0
	}

	for rel := range strings.SplitSeq(listing.stdout, "\x00") {
		if rel == "" {
			continue
		}
		files++
		if budget <= 0 {
			continue // keep counting files; just stop tallying lines
		}
		ins, read := countUntrackedFileLines(filepath.Join(cwd, rel), budget)
		insertions += ins
		budget -= read
	}
	return insertions, files
}

// countUntrackedFileLines returns the added-line count git's numstat would
// report for the untracked entry at path, plus the bytes read (so the caller
// can debit its scan budget). It matches git's accounting without following
// links or opening special files — either of which would let the gitwatch hot
// path read outside the workspace or block forever on a FIFO/device:
//   - a symlink is counted as its target *text* (one line), exactly as git's
//     mode-120000 diff does — resolved via Lstat + Readlink, never opened;
//   - any non-regular entry (FIFO, socket, device, directory) counts 0;
//   - a regular file is read (capped at budget) and counted by countAddedLines.
//
// Divergence from the panel on symlinks whose target is not a regular file:
// the panel runs `git diff --no-index /dev/null <symlink>`, which follows the
// link — so a symlink to a directory errors out (contributing 0) and a symlink
// to a FIFO would block on open (the panel never finishes that file). This
// counter always reports the link text (1 line) and never touches the target,
// so the badge can read up to 1 line higher per such symlink. That is a
// deliberate robustness tradeoff: the hot path must never follow a link off the
// workspace or hang on a device. (A symlink to a regular file matches the panel:
// both count the one-line link text, since git --no-index does not follow it.)
func countUntrackedFileLines(path string, budget int) (insertions, bytesRead int) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0 // vanished mid-scan — routine while an agent is writing
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return 0, 0
		}
		return countAddedLines([]byte(target)), 0
	}
	if !mode.IsRegular() {
		return 0, 0
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(budget)))
	if err != nil {
		return 0, len(data)
	}
	return countAddedLines(data), len(data)
}

// countAddedLines counts the lines git's numstat would attribute to data added
// whole (a new file versus /dev/null). It mirrors git's default content
// detection: a NUL byte within the first 8000 bytes (git's own probe window)
// marks the data binary — numstat "-", zero lines — otherwise every newline is
// a line, plus one more for a final line with no trailing newline.
//
// This must stay aligned with the diff panel's untracked-file count, which is a
// different code path: the panel sums the frontend's parse
// (frontend/src/lib/utils/patchFiles.ts) of `git diff --no-index --patch
// /dev/null <file>`, counting every '+' line that is not the '+++' header —
// which equals the newline count here (the "\ No newline" marker and "Binary
// files differ" lines start with '\'/'B', so neither side counts them).
// TestCountAddedLinesMatchesPanelParse and the panel-total assertion in
// TestStatusInsertionsIncludeUntracked pin the two together; keep them in sync.
//
// Known divergences (rare, documented rather than fixed):
//   - .gitattributes content overrides (binary, -text, -diff): the panel uses
//     real `git diff` and honours them; this raw heuristic does not.
//   - a file content line beginning with "++" (two or more '+'): git's patch
//     adds one more '+' prefix, so the panel sees "+++…" and the frontend's
//     startsWith('+++') header-skip drops it — the panel undercounts while this
//     counter is correct. The mirror "---" case cannot occur for an untracked
//     file: every content line is an addition prefixed "+", never "-".
func countAddedLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	probe := data
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
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

// CurrentBranch returns the branch name reported by Status, or "" when
// the call fails (detached HEAD, non-repo path, transient git error).
// Callers treat "" as "unknown" — distinct from a deliberate empty
// branch — so the helper never propagates the underlying error.
func (c *Core) CurrentBranch(cwd string) string {
	status, err := c.Status(cwd)
	if err != nil {
		return ""
	}
	return status.Branch
}

// BranchIsDefault reports whether branch is the repo's default (e.g.
// `main`/`master`). Returns false when ListBranches fails or when the
// branch isn't present in the listing.
func (c *Core) BranchIsDefault(cwd, branch string) bool {
	branches, err := c.ListBranches(cwd)
	if err != nil {
		return false
	}
	for _, candidate := range branches {
		if candidate.Name == branch {
			return candidate.IsDefault
		}
	}
	return false
}

// ListBranches returns local and remote branches from git branch output.
func (c *Core) ListBranches(cwd string) ([]GitBranch, error) {
	result, err := c.run(cwd, "branch", "-a",
		"--format=%(refname:short)|%(HEAD)|%(worktreepath)|%(upstream:track,nobracket)")
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

// parseStatusOutput derives the porcelain-v2 fields (branch, ahead/behind,
// changed paths) and folds in the tracked churn from `git diff HEAD --numstat`.
// FileCount here counts only tracked and staged changes (porcelain paths plus
// the numstat paths, which are a subset). Untracked entries are excluded and
// counted individually by the caller via untrackedStats, because git status
// collapses a wholly-untracked directory to a single porcelain entry and would
// undercount. Untracked insertions are likewise added by the caller.
func parseStatusOutput(statusStdout, headNumstat string) GitStatus {
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
			// Untracked ('?') and ignored ('!') entries are excluded from
			// FileCount here — untracked files are counted individually via
			// ls-files in baseStatus because git status collapses a
			// wholly-untracked directory to a single entry. Tracked and staged
			// changes are counted from their porcelain paths.
			if strings.HasPrefix(line, "? ") || strings.HasPrefix(line, "! ") {
				continue
			}
			if path := parsePorcelainPath(line); path != "" {
				paths[path] = struct{}{}
			}
		}
	}

	insertions, deletions := 0, 0
	for _, entry := range parseNumstat(headNumstat) {
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
	type rawEntry struct {
		name         string
		isCurrent    bool
		worktreePath string
		ahead        int
		behind       int
		isRemote     bool
	}

	remoteSet := make(map[string]struct{}, len(remoteNames))
	for _, r := range remoteNames {
		remoteSet[r] = struct{}{}
	}

	var raws []rawEntry
	localSet := make(map[string]struct{})
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		// Drop remote symrefs. Two forms appear in the wild:
		//   - `refs/remotes/<r>/HEAD` → "<r>/HEAD" (standard refname:short)
		//   - same ref but git collapses to the bare "<r>" on some
		//     versions (observed locally on git 2.45+).
		// Both must go; a local branch literally named "<r>/HEAD" or
		// "<r>" would be filtered too, but those are degenerate names
		// nobody picks intentionally.
		if _, isBareRemote := remoteSet[name]; isBareRemote {
			continue
		}
		isRemote := isRemoteBranchName(name, remoteNames)
		if isRemote && strings.HasSuffix(name, "/HEAD") {
			continue
		}
		ahead, behind := parseUpstreamTrack(parts[3])
		raws = append(raws, rawEntry{
			name:         name,
			isCurrent:    strings.TrimSpace(parts[1]) == "*",
			worktreePath: strings.TrimSpace(parts[2]),
			ahead:        ahead,
			behind:       behind,
			isRemote:     isRemote,
		})
		if !isRemote {
			localSet[name] = struct{}{}
		}
	}

	seenRemoteBase := make(map[string]struct{})
	var branches []GitBranch
	for _, e := range raws {
		name := e.name
		if e.isRemote {
			base := stripRemotePrefix(name, remoteNames)
			if _, hasLocal := localSet[base]; hasLocal {
				continue
			}
			if _, seen := seenRemoteBase[base]; seen {
				continue
			}
			seenRemoteBase[base] = struct{}{}
			name = base
		}
		branches = append(branches, GitBranch{
			Name:         name,
			IsCurrent:    e.isCurrent,
			IsDefault:    isDefaultBranchName(name, defaultBranch),
			WorktreePath: e.worktreePath,
			AheadCount:   e.ahead,
			BehindCount:  e.behind,
		})
	}
	return branches
}

// parseUpstreamTrack reads the output of `--format=%(upstream:track,nobracket)`.
// Examples: "" (no upstream or in sync), "gone" (upstream deleted), "ahead 3",
// "behind 2", "ahead 3, behind 2". Unknown words contribute 0.
func parseUpstreamTrack(raw string) (ahead, behind int) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "gone" {
		return 0, 0
	}
	for _, part := range strings.Split(raw, ",") {
		fields := strings.Fields(part)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "ahead":
			ahead = n
		case "behind":
			behind = n
		}
	}
	return ahead, behind
}

func stripRemotePrefix(name string, remoteNames []string) string {
	for _, r := range remoteNames {
		if strings.HasPrefix(name, r+"/") {
			return strings.TrimPrefix(name, r+"/")
		}
	}
	return name
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
	_, ok := c.originURL(cwd)
	return ok
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

// lookupOpenPRCached returns cached PR info without making a network
// call. Returns ("", 0) on cache miss. Used by StatusFast to keep
// the initial subscribe path free of network calls.
func (c *Core) lookupOpenPRCached(cwd, branch string) (string, int) {
	if branch == "" {
		return "", 0
	}
	key := prCacheKey(cwd, branch)
	c.prCacheMu.RLock()
	defer c.prCacheMu.RUnlock()
	if entry, ok := c.prCache[key]; ok && entry.expiresAt.After(c.nowFn()) {
		return entry.url, entry.number
	}
	return "", 0
}

func (c *Core) lookupOpenPR(cwd, branch string) (string, int) {
	if branch == "" {
		return "", 0
	}
	key := prCacheKey(cwd, branch)
	now := c.nowFn()

	c.prCacheMu.RLock()
	if entry, ok := c.prCache[key]; ok && entry.expiresAt.After(now) {
		c.prCacheMu.RUnlock()
		return entry.url, entry.number
	}
	c.prCacheMu.RUnlock()

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
	// Sweep expired sibling entries on each write so the map stays
	// bounded by the number of recently-active (cwd, branch) pairs
	// rather than the lifetime total.
	for k, entry := range c.prCache {
		if entry.expiresAt.Before(now) {
			delete(c.prCache, k)
		}
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

// CountUnpushedCommits returns the number of commits on branch that have not
// been pushed to its configured upstream. Returns hasUpstream=false when no
// upstream is configured (in which case the count is meaningless and the
// caller should treat the branch as "no remote"). Queries the symbolic
// upstream so non-origin remotes work too.
func (c *Core) CountUnpushedCommits(cwd, branch string) (count int, hasUpstream bool, err error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return 0, false, nil
	}
	upstream, ok := c.upstreamFor(cwd, branch)
	if !ok {
		return 0, false, nil
	}
	stdout, _, err := c.Execute(cwd, "rev-list", "--count", branch, "^"+upstream)
	if err != nil {
		return 0, true, err
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(stdout))
	if parseErr != nil {
		return 0, true, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(stdout), parseErr)
	}
	return value, true, nil
}

// upstreamFor resolves the upstream ref for branch (e.g. "origin/main").
// Returns false when no upstream is configured.
func (c *Core) upstreamFor(cwd, branch string) (string, bool) {
	result, err := c.run(cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}")
	if err != nil {
		return "", false
	}
	if result.exitCode != 0 {
		return "", false
	}
	upstream := strings.TrimSpace(result.stdout)
	if upstream == "" {
		return "", false
	}
	return upstream, true
}

// CountWorkingTreeChanges returns the number of changed files (staged,
// unstaged, untracked) in cwd via `git status --porcelain`. Cheaper than the
// full Status aggregator when the caller only needs a dirty-or-not signal.
func (c *Core) CountWorkingTreeChanges(cwd string) (int, error) {
	stdout, _, err := c.Execute(cwd, "status", "--porcelain")
	if err != nil {
		return 0, err
	}
	count := 0
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}
