package git

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
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
	// OpenPRLookupError is set when the forge lookup failed, distinct from
	// a successful lookup that found no open PR/MR for the branch.
	OpenPRLookupError string `json:"openPrLookupError,omitempty"`
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
		s.OpenPRLookupError == other.OpenPRLookupError &&
		s.PendingOperation == other.PendingOperation
}

// Status reads repository status using porcelain v2 output.
func (c *Core) Status(cwd string) (GitStatus, error) {
	status, err := c.baseStatus(cwd)
	if err != nil || !status.IsRepo {
		return status, err
	}
	if status.Branch != "" && status.Forge != "" {
		status.OpenPRURL, status.OpenPRNumber, status.OpenPRLookupError = c.lookupOpenPR(cwd, status.Branch)
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
	if status.Branch != "" && status.Forge != "" {
		status.OpenPRURL, status.OpenPRNumber, status.OpenPRLookupError = c.lookupOpenPRCached(cwd, status.Branch)
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
	// The six probes below are mutually independent, so they run concurrently
	// and the refresh costs max(probe) instead of the sum. That matters on
	// filesystems where a git subprocess is expensive: on a repo reached over
	// WSL's 9P bridge (a Windows drive) `status` alone measures ~1.1s and the
	// numstat diff ~0.9s, and gitwatch runs this on every debounce edge — the
	// serial batch measured 2.4s there versus 0.6s fanned out (14ms vs 6ms on
	// native ext4). Results land in per-probe slots rather than a shared error
	// channel so the join below keeps the exact error precedence the serial
	// version had: status first, then the non-repo classification, then diff.
	var (
		wg sync.WaitGroup

		statusResult commandResult
		statusErr    error

		numstat    commandResult
		numstatErr error

		defaultBranch  string
		origin         originIdentity
		untrackedIns   int
		untrackedFiles int
		pending        string
	)
	wg.Add(6)

	go func() {
		defer wg.Done()
		// runLocaleC: the non-repo classification below matches git's own
		// English message, which a git built with NLS would otherwise translate
		// — misreporting a plain non-repo path as a hard error.
		statusResult, statusErr = c.runLocaleC(cwd, "status", "--porcelain=v2", "--branch")
	}()

	go func() {
		defer wg.Done()
		// Tracked churn measured against HEAD with the same diff the panel's
		// DiffWorkspaceVsHead produces (HEAD, --minimal --no-ext-diff --no-textconv):
		// numstat's per-file insertion/deletion counts equal the '+'/'-' content
		// lines the panel parses from --patch, so the header badge and the panel
		// report the same numbers. On a fresh repo with no HEAD this exits non-zero
		// with empty output (runBinary surfaces that as a result, not an error),
		// yielding zero tracked churn — which is exactly what the panel shows there.
		numstat, numstatErr = c.run(cwd, "diff", "--numstat", "--minimal", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	}()

	go func() {
		defer wg.Done()
		defaultBranch, _ = c.defaultBranchName(cwd)
	}()

	go func() {
		defer wg.Done()
		origin = c.originRemote(cwd)
	}()

	go func() {
		defer wg.Done()
		// Untracked, non-ignored files count as all-insertions — the same files the
		// diff panel folds into its workspace total. They're counted individually
		// (git status collapses a wholly-untracked directory to one porcelain entry,
		// which parseStatusOutput excludes from FileCount to avoid undercounting).
		// Best-effort: any failure degrades to the tracked-only counts rather than
		// failing the whole refresh, matching how branch/forge/pending degrade.
		untrackedIns, untrackedFiles = c.untrackedStats(cwd, maxUntrackedScanBytes)
	}()

	go func() {
		defer wg.Done()
		pending = c.pendingOperation(cwd)
	}()

	wg.Wait()

	// A non-repo cwd fails all six probes; only status's failure is
	// classified, and the other five results are dropped unread exactly as
	// they were when the early return happened before they ran.
	if statusErr != nil {
		return GitStatus{}, statusErr
	}
	if statusResult.exitCode != 0 {
		if strings.Contains(strings.ToLower(statusResult.stderr), "not a git repository") {
			return GitStatus{IsRepo: false}, nil
		}
		return GitStatus{}, fmt.Errorf("git status failed: %s", strings.TrimSpace(statusResult.stderr))
	}
	if numstatErr != nil {
		return GitStatus{}, numstatErr
	}

	// Populate the forge cache so any concurrent DetectForge call (e.g. through
	// forgeFor → ListOpenPRs) reuses the same classification rather than
	// re-shelling `git remote get-url` — and so the open-PR lookup can compare
	// the current origin identity without a second read. Recorded here rather
	// than inside the goroutine above so a non-repo cwd, whose origin read is
	// one of the discarded failures, still leaves no entry behind.
	forge := c.recordOrigin(cwd, origin, c.nowFn())

	status := parseStatusOutput(statusResult.stdout, numstat.stdout)
	status.Insertions += untrackedIns
	status.FileCount += untrackedFiles
	status.IsRepo = true
	status.HasOriginRemote = origin.known
	status.Forge = forge
	status.IsDefaultBranch = isDefaultBranchName(status.Branch, defaultBranch)
	status.PendingOperation = pending

	return status, nil
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

func (c *Core) currentBranch(cwd string) (string, error) {
	result, err := c.run(cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		if stderr := strings.TrimSpace(result.stderr); stderr != "" {
			return "", fmt.Errorf("git symbolic-ref HEAD failed: %s", stderr)
		}
		return "", fmt.Errorf("cannot operate on detached HEAD")
	}
	branch := strings.TrimSpace(result.stdout)
	if branch == "" {
		return "", fmt.Errorf("git symbolic-ref HEAD returned an empty branch")
	}
	return branch, nil
}

// CurrentBranch returns the current branch name without computing the rest of
// repository status. symbolic-ref works for both committed and unborn branches;
// detached HEAD, non-repository paths, and transient git errors return "".
//
// Keep this branch-only: draft-thread creation calls it while assembling the
// composer defaults, where running Status would also scan changes and perform a
// forge PR lookup before the composer can mount.
func (c *Core) CurrentBranch(cwd string) string {
	branch, err := c.currentBranch(cwd)
	if err != nil {
		return ""
	}
	return branch
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
	if err := validateBranchName(branch); err != nil {
		return 0, false, err
	}
	upstream, ok := c.upstreamFor(cwd, branch)
	if !ok {
		return 0, false, nil
	}
	// The trailing "--" disambiguates refs from pathspecs when a branch
	// name also exists as a filesystem path in the worktree.
	stdout, _, err := c.Execute(cwd, "rev-list", "--count", branch, "^"+upstream, "--")
	if err != nil {
		return 0, true, err
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(stdout))
	if parseErr != nil {
		return 0, true, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(stdout), parseErr)
	}
	return value, true, nil
}

// CountCommitsAhead returns the number of commits reachable from branch but not
// from base — what branch has that base does not. Both are local refs, so this
// answers for a branch with no upstream at all, which is what a workflow's
// item and unit branches are.
//
// cwd may be any checkout of the repository, including one from which the
// branch's own worktree has already been removed: refs are repository-wide, so
// this stays answerable after a checkout is retired.
func (c *Core) CountCommitsAhead(cwd, branch, base string) (int, error) {
	branch = strings.TrimSpace(branch)
	base = strings.TrimSpace(base)
	if branch == "" || base == "" {
		return 0, fmt.Errorf("git branch and base branch are required")
	}
	if err := validateBranchName(branch); err != nil {
		return 0, err
	}
	if err := validateBranchName(base); err != nil {
		return 0, err
	}
	// The trailing "--" disambiguates refs from pathspecs when a branch
	// name also exists as a filesystem path in the worktree.
	stdout, _, err := c.Execute(cwd, "rev-list", "--count", branch, "^"+base, "--")
	if err != nil {
		return 0, err
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(stdout))
	if parseErr != nil {
		return 0, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(stdout), parseErr)
	}
	return value, nil
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
// unstaged, untracked) in cwd. Cheaper than the full Status aggregator when the
// caller only needs a dirty-or-not signal.
func (c *Core) CountWorkingTreeChanges(cwd string) (int, error) {
	_, total, err := c.WorkingTreeChanges(cwd, 0)
	return total, err
}

// WorkingTreeChanges lists the changed files (staged, unstaged, untracked) in
// cwd and reports the total, which is what CountWorkingTreeChanges is. `limit`
// caps the returned slice — 0 returns the count alone — so a caller previewing
// what a destructive action would discard can name the first N files without
// materializing a pathological tree.
//
// `--porcelain -z` is used rather than the line-oriented form because git
// quotes and escapes paths with spaces or non-ASCII bytes in the latter: a
// preview must show the real filename. Rename and copy entries carry a second
// NUL-terminated field (the original path) which is consumed as part of the
// same change rather than counted twice.
func (c *Core) WorkingTreeChanges(cwd string, limit int) (paths []string, total int, err error) {
	stdout, _, err := c.Execute(cwd, "status", "--porcelain", "-z")
	if err != nil {
		return nil, 0, err
	}
	fields := strings.Split(stdout, "\x00")
	for index := 0; index < len(fields); index++ {
		entry := fields[index]
		// The trailing NUL leaves an empty final field; a short entry cannot
		// carry the mandatory "XY " status prefix.
		if len(entry) < 4 {
			continue
		}
		status, path := entry[:2], entry[3:]
		if status[0] == 'R' || status[0] == 'C' {
			index++ // The origin path belongs to this entry, not the next one.
		}
		total++
		if limit > 0 && len(paths) < limit {
			paths = append(paths, path)
		}
	}
	return paths, total, nil
}

// DeleteBranch removes a local branch. `force` uses `-D`, which is what a
// discard needs: the branch being thrown away is by definition one whose
// commits never landed, and `-d` refuses exactly those.
func (c *Core) DeleteBranch(cwd, branch string, force bool) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("git branch name is required")
	}
	if err := validateBranchName(branch); err != nil {
		return err
	}
	exists, err := c.branchExistsChecked(cwd, branch)
	if err != nil {
		return fmt.Errorf("check branch %q: %w", branch, err)
	}
	if !exists {
		return nil
	}
	flag := "-d"
	if force {
		flag = "-D"
	}
	result, err := c.run(cwd, "branch", flag, "--", branch)
	if err != nil {
		return fmt.Errorf("delete branch %q: %w", branch, err)
	}
	if result.exitCode != 0 {
		return fmt.Errorf("delete branch %q: %s", branch, strings.TrimSpace(result.stderr))
	}
	return nil
}
