package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Commit is one row of the review pane's per-commit selector.
type Commit struct {
	SHA        string `json:"sha"`
	ShortSHA   string `json:"shortSha"`
	Subject    string `json:"subject"`
	Author     string `json:"author"`
	AuthoredAt int64  `json:"authoredAt"`
}

// maxListedCommits bounds the selector list; a branch this far off its
// base is not being reviewed commit-by-commit anyway.
const maxListedCommits = 300

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Separators for the `git log --pretty` parse. Records are LED by a
// NUL (`%x00`) — the one byte a commit object can never contain, so a
// subject holding arbitrary control characters cannot forge a record
// boundary. Fields split on \x1f with the subject last and SplitN, so
// a \x1f inside the subject stays in the subject.
const (
	logFieldSep    = "\x1f"
	logRecordLead  = "\x00"
	logCommitCount = 5 // %H, %h, %an, %at, %s
)

// ListCommits returns the commits reachable from HEAD but not from
// baseBranch (`base..HEAD`), newest first — the commit list a PR onto
// baseBranch would carry. Capped at maxListedCommits.
func ListCommits(ctx context.Context, workspace, baseBranch string) ([]Commit, error) {
	return ListCommitsRange(ctx, workspace, baseBranch, "HEAD")
}

// ListCommitsRange returns `base..head`, newest first, capped at
// maxListedCommits. head must be "HEAD" or a hex SHA (a fetched PR head
// OID); base is a picker branch name resolved via resolveBaseRef (so a
// remote-only branch like "feature" works as "origin/feature").
func ListCommitsRange(ctx context.Context, workspace, base, head string) ([]Commit, error) {
	base, err := resolveBaseRef(ctx, workspace, base)
	if err != nil {
		return nil, err
	}
	head = strings.TrimSpace(head)
	if head != "HEAD" && !commitSHAPattern.MatchString(head) {
		return nil, fmt.Errorf("gitdiff: invalid head %q", head)
	}
	return logCommitRange(ctx, workspace, base, head)
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
		"--pretty=format:%x00%H"+logFieldSep+"%h"+logFieldSep+"%an"+logFieldSep+"%at"+logFieldSep+"%s",
		rev, "--")
	if err != nil {
		return nil, err
	}
	return parseCommitLog(stdout)
}

func parseCommitLog(stdout string) ([]Commit, error) {
	commits := []Commit{}
	for _, record := range strings.Split(stdout, logRecordLead) {
		// git separates format entries with a newline, which lands at the
		// tail of the previous record; the subject itself never holds one.
		record = strings.TrimSuffix(record, "\n")
		if record == "" {
			continue
		}
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
		})
	}
	return commits, nil
}

// CommitDiff returns the unified patch a single commit introduced:
// against its first parent for regular and merge commits, against the
// empty tree for a root commit. The SHA must be hex — it always comes
// from a commit list this package produced or a forge API response,
// never free-form user input.
func CommitDiff(ctx context.Context, workspace, sha string, opts Options) ([]byte, error) {
	sha = strings.TrimSpace(sha)
	if !commitSHAPattern.MatchString(sha) {
		return nil, fmt.Errorf("gitdiff: invalid commit sha %q", sha)
	}
	parents, _, _, err := runGit(ctx, workspace, nil, false, "rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return nil, fmt.Errorf("gitdiff: resolve parents of %s: %w", sha, err)
	}
	hashes := strings.Fields(parents)
	if len(hashes) == 0 {
		return nil, fmt.Errorf("gitdiff: rev-list returned nothing for %s", sha)
	}

	var stdout string
	if len(hashes) > 1 {
		// First-parent diff: for merge commits this matches how GitHub
		// and GitLab render a commit's changes.
		stdout, _, _, err = runGitWithStdoutLimit(ctx, workspace, nil, false, maxDiffOutputBytes,
			opts.gitArgs("diff", hashes[1], hashes[0], "--")...)
	} else {
		stdout, _, _, err = runGitWithStdoutLimit(ctx, workspace, nil, false, maxDiffOutputBytes,
			opts.gitArgs("diff-tree", "--root", "--find-renames", sha, "--")...)
	}
	if errors.Is(err, errGitOutputTooLarge) {
		return nil, fmt.Errorf("gitdiff: commit diff exceeds %d byte limit", maxDiffOutputBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("gitdiff: diff commit %s: %w", sha, err)
	}
	return []byte(stdout), nil
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
