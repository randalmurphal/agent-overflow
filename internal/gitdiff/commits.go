package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Commit is one row of the review pane's per-commit selector.
type Commit struct {
	SHA      string `json:"sha"`
	ShortSHA string `json:"shortSha"`
	Subject  string `json:"subject"`
	// Body is the message after the subject, trailing newlines trimmed;
	// empty for a one-line message.
	Body       string `json:"body"`
	Author     string `json:"author"`
	AuthoredAt int64  `json:"authoredAt"`
}

// CommitList is a range's commits for the selector, newest first and
// capped, and how many the range holds.
type CommitList struct {
	Commits []Commit `json:"commits"`
	// Total is how many commits the range holds, counted without listing
	// them; null when they could not be counted in time. The list is
	// complete exactly when Total equals the number of Commits.
	Total *int `json:"total"`
}

// maxListedCommits bounds the selector list; a branch this far off its
// base is not being reviewed commit-by-commit anyway.
const maxListedCommits = 300

// commitCountWait bounds counting a range past the cap.
const commitCountWait = 5 * time.Second

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Separators for the `git log --pretty` parse. A commit is two chunks,
// each LED by a NUL (`%x00`), the one byte a commit object can never
// contain, so a message holding arbitrary control characters cannot
// forge a chunk boundary: the header, whose fields split on \x1f with
// the subject last and SplitN so a \x1f inside the subject stays in the
// subject; then the body, whole.
const (
	logFieldSep    = "\x1f"
	logChunkLead   = "\x00"
	logCommitCount = 5 // %H, %h, %an, %at, %s
)

// ListCommits returns the commits reachable from HEAD but not from
// baseBranch (`base..HEAD`), newest first — the commit list a PR onto
// baseBranch would carry. Capped at maxListedCommits.
func ListCommits(ctx context.Context, workspace, baseBranch string) (CommitList, error) {
	return ListCommitsRange(ctx, workspace, baseBranch, "HEAD")
}

// ListCommitsRange returns `base..head`, newest first, capped at
// maxListedCommits. head must be "HEAD" or a hex SHA (a fetched PR head
// OID); base is a picker branch name resolved via resolveBaseRef (so a
// remote-only branch like "feature" works as "origin/feature"). Past the
// cap the range is counted, within commitCountWait.
func ListCommitsRange(ctx context.Context, workspace, base, head string) (CommitList, error) {
	base, err := resolveBaseRef(ctx, workspace, base)
	if err != nil {
		return CommitList{}, err
	}
	head = strings.TrimSpace(head)
	if head != "HEAD" && !commitSHAPattern.MatchString(head) {
		return CommitList{}, fmt.Errorf("gitdiff: invalid head %q", head)
	}
	return listCommitRange(ctx, workspace, base, head, maxListedCommits, commitCountWait)
}

// listCommitRange lists `base..head` up to limit commits and answers how
// many it holds: the listed number when one more did not exist, else a
// count of the range within countWait, or no total when that fails. Only
// ctx ending fails the list.
func listCommitRange(ctx context.Context, workspace, base, head string, limit int, countWait time.Duration) (CommitList, error) {
	rev := base + ".." + head
	commits, err := logRev(ctx, workspace, limit+1, rev)
	if err != nil {
		return CommitList{}, fmt.Errorf("gitdiff: log %s: %w", rev, err)
	}
	if len(commits) <= limit {
		total := len(commits)
		return CommitList{Commits: commits, Total: &total}, nil
	}
	list := CommitList{Commits: commits[:limit]}
	countCtx, cancel := context.WithTimeout(ctx, countWait)
	defer cancel()
	out, _, _, err := runGit(countCtx, workspace, nil, false, "rev-list", "--count", rev, "--")
	if err == nil {
		var total int
		if total, err = strconv.Atoi(strings.TrimSpace(out)); err == nil {
			list.Total = &total
			return list, nil
		}
	}
	if ctx.Err() != nil {
		return CommitList{}, fmt.Errorf("gitdiff: count %s: %w", rev, ctx.Err())
	}
	log.Printf("gitdiff: count %s: %v; the total is unknown", rev, err)
	return list, nil
}

// ListBranchCommits returns the commits on `branch` that are not reachable from
// `base` (`base..branch`), newest first, capped at maxListedCommits — the
// commits a branch would lose if it were deleted without landing.
//
// The two ends resolve by different rules, which is the whole reason this
// signature takes names rather than revisions. `base` goes through
// resolveBaseRef (remote-tracking preferred: it is what the branch would land
// on). `branch` goes through resolveNamedRef (local preferred), because the
// commits at risk are the ones in the LOCAL branch — resolving it to
// `origin/<branch>` would omit everything unpushed and report a branch as safe
// to delete precisely when it is not.
//
// A branch that does not resolve is an error: silently reporting "no unmerged
// commits" for a branch git could not find would turn a lookup failure into a
// claim that nothing would be lost.
func ListBranchCommits(ctx context.Context, workspace, base, branch string) ([]Commit, error) {
	base, err := resolveBaseRef(ctx, workspace, base)
	if err != nil {
		return nil, err
	}
	branch, err = resolveNamedRef(ctx, workspace, branch)
	if err != nil {
		return nil, err
	}
	return logCommitRange(ctx, workspace, base, branch)
}

// ListRecentCommits returns the workspace's most recent commits from
// HEAD (plain `git log`, merges included), newest first, capped at
// limit. This mirrors codex's own "Review a commit" picker
// (recent_commits(cwd, 100)) — deliberately NOT a base..head range, so
// a checkout sitting on the default branch still gets a list. A repo
// with an unborn HEAD (no commits yet) is an empty answer, not an
// error.
func ListRecentCommits(ctx context.Context, workspace string, limit int) ([]Commit, error) {
	if limit <= 0 || limit > maxListedCommits {
		limit = maxListedCommits
	}
	if _, _, exitCode, err := runGit(ctx, workspace, nil, true,
		"rev-parse", "--verify", "--quiet", "HEAD"); err != nil || exitCode != 0 {
		if err != nil {
			return nil, fmt.Errorf("gitdiff: verify HEAD: %w", err)
		}
		return []Commit{}, nil
	}
	commits, err := logRev(ctx, workspace, limit, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("gitdiff: log HEAD: %w", err)
	}
	return commits, nil
}

func logCommitRange(ctx context.Context, workspace, base, head string) ([]Commit, error) {
	commits, err := logRev(ctx, workspace, maxListedCommits, base+".."+head)
	if err != nil {
		return nil, fmt.Errorf("gitdiff: log %s..%s: %w", base, head, err)
	}
	return commits, nil
}

func logRev(ctx context.Context, workspace string, limit int, rev string) ([]Commit, error) {
	stdout, _, _, err := runGit(ctx, workspace, nil, false,
		"log", "--no-decorate", "--max-count="+strconv.Itoa(limit),
		"--pretty=format:%x00%H"+logFieldSep+"%h"+logFieldSep+"%an"+logFieldSep+"%at"+logFieldSep+"%s%x00%b",
		rev, "--")
	if err != nil {
		return nil, err
	}
	return parseCommitLog(stdout)
}

func parseCommitLog(stdout string) ([]Commit, error) {
	commits := []Commit{}
	if stdout == "" {
		return commits, nil
	}
	chunks := strings.Split(stdout, logChunkLead)
	// Output starts with a lead, so the first chunk is empty; after it the
	// chunks pair up as header, body.
	if chunks[0] != "" || len(chunks)%2 != 1 {
		return nil, fmt.Errorf("gitdiff: malformed log output (%d chunks)", len(chunks))
	}
	for i := 1; i < len(chunks); i += 2 {
		record := chunks[i]
		// git separates format entries with a newline, which lands at the
		// tail of the previous commit's body along with the body's own
		// trailing newlines.
		body := strings.TrimRight(chunks[i+1], "\n")
		fields := strings.SplitN(record, logFieldSep, logCommitCount)
		if len(fields) != logCommitCount {
			return nil, fmt.Errorf("gitdiff: malformed log record %q", record)
		}
		authoredAt, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("gitdiff: malformed author timestamp %q: %w", fields[3], err)
		}
		commits = append(commits, Commit{
			SHA:        fields[0],
			ShortSHA:   fields[1],
			Author:     fields[2],
			AuthoredAt: authoredAt * 1000,
			Subject:    fields[4],
			Body:       body,
		})
	}
	return commits, nil
}

// OpenCommitDiff opens the patch a single commit introduced (CommitSides).
func OpenCommitDiff(ctx context.Context, workspace, sha string, opts Options) (*Diff, error) {
	sides, err := CommitSides(ctx, workspace, sha)
	if err != nil {
		return nil, err
	}
	return sides.OpenDiff(opts), nil
}

// OpenCommitRangeDiff opens the patch of a contiguous run of a pull
// request's commits (CommitRangeSides).
func OpenCommitRangeDiff(ctx context.Context, workspace, base, head, first, last string, opts Options) (*Diff, error) {
	sides, err := CommitRangeSides(ctx, workspace, base, head, first, last)
	if err != nil {
		return nil, err
	}
	return sides.OpenDiff(opts), nil
}

// OpenMergeBaseDiff opens the three-dot patch from the merge base of base
// and head to head (MergeBaseSides).
func OpenMergeBaseDiff(ctx context.Context, workspace, base, head string, opts Options) (*Diff, error) {
	sides, err := MergeBaseSides(ctx, workspace, base, head)
	if err != nil {
		return nil, err
	}
	return sides.OpenDiff(opts), nil
}

// ShowFileAtCommit returns the full content of path as of the given
// commit. Backs review-diff hunk-gap expansion when a single commit is
// selected, where the diff's new side is that commit rather than the
// worktree. The path is `:`-joined into a single argument, so it
// cannot be interpreted as a flag.
func ShowFileAtCommit(ctx context.Context, workspace, sha, path string) (string, error) {
	sha = strings.TrimSpace(sha)
	if !commitSHAPattern.MatchString(sha) {
		return "", fmt.Errorf("gitdiff: invalid commit sha %q", sha)
	}
	if strings.ContainsRune(path, '\x00') {
		return "", errors.New("gitdiff: show file: path must not contain NUL")
	}
	stdout, stderr, code, err := runGit(ctx, workspace, nil, true, "show", sha+":"+path)
	if err != nil {
		return "", fmt.Errorf("gitdiff: show %s:%s: %w", sha, path, err)
	}
	if code != 0 {
		return "", fmt.Errorf("gitdiff: show %s:%s: %s", sha, path, strings.TrimSpace(stderr))
	}
	return stdout, nil
}
