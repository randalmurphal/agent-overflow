package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitStatus summarizes repository state for a thread workspace.
type GitStatus struct {
	IsRepo bool   `json:"isRepo"`
	Branch string `json:"branch"`
	// HeadCommit is the full commit id of a detached HEAD; empty on a
	// branch.
	HeadCommit string `json:"headCommit,omitempty"`
	// Tag is set only on a detached HEAD at least one tag points at: the
	// first such tag in ListTags order.
	Tag             string `json:"tag,omitempty"`
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
	// OpenPRLookupPending is set by StatusFast when the PR cache holds no
	// current entry for the branch: the open-PR fields above are the last
	// PR the branch was known to have (empty when none), not an answer
	// yet, and the follow-up Status call that asks the forge broadcasts
	// the answer. A reader that needs the answer (the review pane choosing
	// between "no PR" and "not yet") waits on it.
	OpenPRLookupPending bool `json:"openPrLookupPending,omitempty"`
	// OpenPRRecheckAt is when a failed open-PR lookup is asked again; set
	// by Status beside OpenPRLookupError, zero otherwise. Not sent: the
	// status watcher re-checks at that time, since nothing on disk changes
	// when the forge recovers.
	OpenPRRecheckAt time.Time `json:"-"`
	// PendingOperation surfaces an in-progress multi-step operation.
	// Values: "merge", "cherry-pick", "revert", "rebase", "bisect", or "" when none is
	// pending. The commit dialog shows it as a notice; git itself decides
	// whether a commit can be recorded.
	PendingOperation string `json:"pendingOperation,omitempty"`
	// Seq orders the statuses handed to clients for one workspace: the
	// gitwatch.Manager position taken before this status's reads began, so
	// a higher Seq never comes from a read that started earlier. Zero is
	// unsequenced. It says when the status was read, not what it says, so
	// Equal ignores it.
	Seq uint64 `json:"seq,omitempty"`
}

// Equal reports whether two GitStatus values say the same thing: every
// field but Seq is identical. Used on hot paths (gitwatch dedup at 250ms
// cadence) where reflect.DeepEqual would add reflection overhead and
// would silently keep working if the struct grew a pointer/map/slice
// field that the caller forgot to consider. A typed Equal forces every
// new field to be added here too; TestGitStatusEqualComparesEveryField
// fails until it is.
func (s GitStatus) Equal(other GitStatus) bool {
	return s.IsRepo == other.IsRepo &&
		s.Branch == other.Branch &&
		s.HeadCommit == other.HeadCommit &&
		s.Tag == other.Tag &&
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
		s.OpenPRLookupPending == other.OpenPRLookupPending &&
		s.OpenPRRecheckAt.Equal(other.OpenPRRecheckAt) &&
		s.PendingOperation == other.PendingOperation
}

// Status reads repository status using porcelain v2 output. ctx bounds
// the forge's open-PR lookup.
func (c *Core) Status(ctx context.Context, cwd string) (GitStatus, error) {
	status, err := c.baseStatus(cwd)
	if err != nil || !status.IsRepo {
		return status, err
	}
	if status.Branch != "" && status.Forge != "" {
		status.OpenPRURL, status.OpenPRNumber, status.OpenPRLookupError = c.lookupOpenPR(ctx, cwd, status.Branch)
		if status.OpenPRLookupError != "" {
			status.OpenPRRecheckAt = c.openPRRecheckAt(cwd, status.Branch)
		}
	}
	return status, nil
}

// StatusFast is the same as Status but uses only cached PR info,
// avoiding the network call to the forge. Returns immediately even when
// the PR cache holds no current answer, with OpenPRLookupPending set and
// the branch's last known PR in the PR fields; the caller is expected to
// arrange a follow-up full Status call that asks the forge and
// broadcasts the answer.
func (c *Core) StatusFast(cwd string) (GitStatus, error) {
	status, err := c.baseStatus(cwd)
	if err != nil || !status.IsRepo {
		return status, err
	}
	if status.Branch != "" && status.Forge != "" {
		var cached bool
		status.OpenPRURL, status.OpenPRNumber, status.OpenPRLookupError, cached = c.lookupOpenPRCached(cwd, status.Branch)
		status.OpenPRLookupPending = !cached
	}
	return status, nil
}

// statusScan is one status read and the churn of the paths it lists.
type statusScan struct {
	result commandResult
	err    error
	// parsed, churn and churnErr are set when the read succeeded.
	parsed   porcelainStatus
	churn    trackedChurnTotals
	churnErr error
}

// scanStatus reads status and the churn of the changed paths it lists.
// The churn follows the read because it diffs only the paths that read
// names; a scan with nothing stored to choose them by diffs the whole
// tree beside the read instead (startChurnInputs).
func (c *Core) scanStatus(cwd string) statusScan {
	in := c.startChurnInputs(cwd, c.nowFn())
	var scan statusScan
	// C locale: the non-repo classification in baseStatus matches git's
	// own English message, which a git built with NLS would otherwise
	// translate, misreporting a plain non-repo path as a hard error.
	scan.result, scan.err = c.runSpec(commandSpec{
		binary: "git", cwd: cwd, extraEnv: localeCEnv, maxBytes: statusOutputLimit,
		args: []string{"status", "--porcelain=v2", "--branch", "-z"},
	})
	if scan.err != nil || scan.result.exitCode != 0 {
		in.wait()
		return scan
	}
	scan.parsed = parsePorcelainStatus(scan.result.stdout)
	scan.churn, scan.churnErr = c.trackedChurn(cwd, scan.parsed, in)
	return scan
}

// baseStatus gathers every status field shared by Status and StatusFast:
// branch and ahead/behind, workspace churn (insertions/deletions/files),
// detected forge, and any pending merge/rebase/bisect. The open-PR lookup is
// the only part that differs between the two callers (live network vs cache),
// so it stays with them; centralising the rest keeps the two entry points from
// drifting apart.
func (c *Core) baseStatus(cwd string) (GitStatus, error) {
	// The five probes below are mutually independent, so they run concurrently
	// and the refresh costs max(probe) instead of the sum. That matters on
	// filesystems where a git subprocess is expensive: on a repo reached over
	// WSL's 9P bridge (a Windows drive) `status` alone measures ~1.1s, and
	// gitwatch runs this on every debounce edge. Results land in per-probe
	// slots rather than a shared error channel so the join below keeps a
	// fixed error precedence: status first, then the non-repo
	// classification, then the churn diff.
	var (
		wg sync.WaitGroup

		statusResult commandResult
		statusErr    error
		parsed       porcelainStatus

		churn    trackedChurnTotals
		churnErr error

		defaultBranch  string
		origin         originIdentity
		untrackedIns   int
		untrackedFiles int
		pending        string
	)
	wg.Add(5)

	go func() {
		defer wg.Done()
		scan := c.scanStatus(cwd)
		statusResult, statusErr = scan.result, scan.err
		parsed, churn, churnErr = scan.parsed, scan.churn, scan.churnErr
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
		// which the tracked file count excludes to avoid undercounting).
		// Best-effort: any failure degrades to the tracked-only counts rather than
		// failing the whole refresh, matching how branch/forge/pending degrade.
		untrackedIns, untrackedFiles = c.untrackedStats(cwd, maxUntrackedScanBytes)
	}()

	go func() {
		defer wg.Done()
		pending = c.pendingOperation(cwd)
	}()

	wg.Wait()

	// A non-repo cwd fails every probe; only status's failure is
	// classified, and the other results are dropped unread.
	if statusErr != nil {
		return GitStatus{}, statusErr
	}
	if statusResult.exitCode != 0 {
		if strings.Contains(strings.ToLower(statusResult.stderr), "not a git repository") {
			return GitStatus{IsRepo: false}, nil
		}
		return GitStatus{}, fmt.Errorf("git status failed: %s", strings.TrimSpace(statusResult.stderr))
	}
	if churnErr != nil {
		return GitStatus{}, churnErr
	}

	// Populate the forge cache so any concurrent DetectForge call (e.g. through
	// forgeFor → ListOpenPRs) reuses the same classification rather than
	// re-shelling `git remote get-url` — and so the open-PR lookup can compare
	// the current origin identity without a second read. Recorded here rather
	// than inside the goroutine above so a non-repo cwd, whose origin read is
	// one of the discarded failures, still leaves no entry behind.
	forge := c.recordOrigin(cwd, origin, c.nowFn())

	status := GitStatus{
		Branch:      parsed.branch,
		HasUpstream: parsed.hasUpstream,
		AheadCount:  parsed.ahead,
		BehindCount: parsed.behind,
		HasChanges:  parsed.hasChanges,
		Insertions:  churn.insertions + untrackedIns,
		Deletions:   churn.deletions,
		FileCount:   churn.files + untrackedFiles,
	}
	if parsed.detached && isFullCommitID(parsed.oid) {
		status.HeadCommit = parsed.oid
	}
	status.IsRepo = true
	status.HasOriginRemote = origin.known
	status.Forge = forge
	status.IsDefaultBranch = isDefaultBranchName(status.Branch, defaultBranch)
	status.PendingOperation = pending
	if status.HeadCommit != "" {
		// Only a detached HEAD pays for the extra read.
		tag, err := c.TagAt(cwd, status.HeadCommit)
		if err != nil {
			return GitStatus{}, err
		}
		status.Tag = tag
	}

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

// currentBranch reads HEAD's branch. The full ref is read and trimmed:
// --short spells a branch that shares its name with a tag "heads/<name>".
func (c *Core) currentBranch(cwd string) (string, error) {
	result, err := c.run(cwd, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		if stderr := strings.TrimSpace(result.stderr); stderr != "" {
			return "", fmt.Errorf("git symbolic-ref HEAD failed: %s", stderr)
		}
		return "", fmt.Errorf("cannot operate on detached HEAD")
	}
	branch := trimBranchRef(strings.TrimSpace(result.stdout))
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

func parseAheadBehind(raw string) (int, int) {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return 0, 0
	}
	ahead, _ := strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
	behind, _ := strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
	return ahead, behind
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
	upstream, ok := c.upstreamRefFor(cwd, branch)
	if !ok {
		return 0, false, nil
	}
	// Both sides fully qualified so a tag of either name is not counted.
	// The trailing "--" disambiguates refs from pathspecs when a branch
	// name also exists as a filesystem path in the worktree.
	stdout, _, err := c.Execute(cwd, "rev-list", "--count", BranchRef(branch).revision(), "^"+upstream, "--")
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
	// Qualified so a tag of either name is not counted. The trailing "--"
	// disambiguates refs from pathspecs when a branch name also exists as a
	// filesystem path in the worktree.
	stdout, _, err := c.Execute(cwd, "rev-list", "--count", BranchRef(branch).revision(), "^"+BranchRef(base).revision(), "--")
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
// Returns false when no upstream is configured. <name>@{upstream} reads
// the branch's configuration, so a tag of its name does not interfere.
func (c *Core) upstreamFor(cwd, branch string) (string, bool) {
	return c.readUpstream(cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}")
}

// upstreamRefFor is upstreamFor fully qualified ("refs/remotes/origin/main"),
// for use as a revision.
func (c *Core) upstreamRefFor(cwd, branch string) (string, bool) {
	return c.readUpstream(cwd, "rev-parse", "--symbolic-full-name", branch+"@{upstream}")
}

func (c *Core) readUpstream(cwd string, args ...string) (string, bool) {
	result, err := c.run(cwd, args...)
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
//
// Callers weigh what a removal or discard would lose, so the flags override
// config that hides work from porcelain status: status.showUntrackedFiles
// (an untracked directory still counts once, as git's default shows it) and
// diff.ignoreSubmodules or submodule.<name>.ignore. Ignored files are not
// counted. Files the index marks assume-unchanged or skip-worktree are not
// either; that is index state, not config.
func (c *Core) WorkingTreeChanges(cwd string, limit int) (paths []string, total int, err error) {
	stdout, _, err := c.Execute(cwd, "status", "--porcelain", "-z", "--untracked-files=normal", "--ignore-submodules=none")
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
