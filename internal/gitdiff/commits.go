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

// field/record separators for the `git log --pretty` parse: subjects
// can contain anything printable, so split on control characters that
// git never emits inside a format placeholder.
const (
	logFieldSep  = "\x1f"
	logRecordSep = "\x1e"
)

// ListCommits returns the commits reachable from HEAD but not from
// baseBranch (`base..HEAD`), newest first — the commit list a PR onto
// baseBranch would carry. Capped at maxListedCommits.
func ListCommits(ctx context.Context, workspace, baseBranch string) ([]Commit, error) {
	return ListCommitsRange(ctx, workspace, baseBranch, "HEAD")
}

// ListCommitsRange returns `base..head`, newest first, capped at
// maxListedCommits. head must be "HEAD" or a hex SHA (a fetched PR head
// OID); base is a ref name and only guarded against flag injection.
func ListCommitsRange(ctx context.Context, workspace, base, head string) ([]Commit, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil, errors.New("gitdiff: base ref is required")
	}
	if strings.HasPrefix(base, "-") {
		return nil, fmt.Errorf("gitdiff: invalid base ref %q", base)
	}
	head = strings.TrimSpace(head)
	if head != "HEAD" && !commitSHAPattern.MatchString(head) {
		return nil, fmt.Errorf("gitdiff: invalid head %q", head)
	}
	stdout, _, _, err := runGit(ctx, workspace, nil, false,
		"log", "--no-decorate", "--max-count="+strconv.Itoa(maxListedCommits),
		"--pretty=format:%H"+logFieldSep+"%h"+logFieldSep+"%an"+logFieldSep+"%at"+logFieldSep+"%s"+logRecordSep,
		base+".."+head, "--")
	if err != nil {
		return nil, fmt.Errorf("gitdiff: log %s..%s: %w", base, head, err)
	}
	return parseCommitLog(stdout)
}

func parseCommitLog(stdout string) ([]Commit, error) {
	commits := []Commit{}
	for _, record := range strings.Split(stdout, logRecordSep) {
		record = strings.TrimLeft(record, "\n")
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.Split(record, logFieldSep)
		if len(fields) != 5 {
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
func CommitDiff(ctx context.Context, workspace, sha string) ([]byte, error) {
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
			"diff", "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv",
			hashes[1], hashes[0], "--")
	} else {
		stdout, _, _, err = runGitWithStdoutLimit(ctx, workspace, nil, false, maxDiffOutputBytes,
			"diff-tree", "--patch", "--minimal", "--no-color", "--root", "--find-renames", sha, "--")
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
