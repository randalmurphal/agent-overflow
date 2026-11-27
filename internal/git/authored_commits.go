package git

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AuthoredCommit is a commit by its author time and subject.
type AuthoredCommit struct {
	Hash    string
	At      time.Time
	Subject string
}

// AuthoredCommits is the non-merge commits on repo's local branches that
// repo's user.email authored from from to before to, by author time,
// earliest first. A repository with no user.email has none. The walk
// starts at committer time from, which no commit authored since can
// precede but by a clock's skew, and the author time is filtered here,
// so a commit rebased today is not today's.
func (c *Core) AuthoredCommits(ctx context.Context, repo string, from, to time.Time) ([]AuthoredCommit, error) {
	email, err := c.runSpec(commandSpec{binary: "git", cwd: repo, ctx: ctx, args: []string{"config", "--get", "user.email"}})
	if err != nil {
		return nil, fmt.Errorf("git: read user.email in %s: %w", repo, err)
	}
	switch email.exitCode {
	case 0:
	case 1:
		return nil, nil
	default:
		return nil, commandFailure("git config user.email in "+repo, email)
	}
	author := strings.TrimSpace(email.stdout)
	if author == "" {
		return nil, nil
	}
	log, err := c.runSpec(commandSpec{binary: "git", cwd: repo, ctx: ctx, args: []string{
		"log", "--branches", "--no-merges", "-z", "--fixed-strings", "--author=<" + author + ">",
		"--since=" + strconv.FormatInt(from.Unix(), 10), "--format=%H%x00%at%x00%s",
	}})
	if err != nil {
		return nil, fmt.Errorf("git: read the commits in %s: %w", repo, err)
	}
	if log.exitCode != 0 {
		return nil, commandFailure("git log in "+repo, log)
	}
	var out []AuthoredCommit
	// -z ends each commit with a NUL, and the format's own NULs part its
	// fields: hash, author time and subject, three to a commit.
	fields := bytes.Split([]byte(strings.TrimSuffix(log.stdout, "\x00")), []byte{0})
	if len(fields) == 1 && len(fields[0]) == 0 {
		return nil, nil
	}
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("git: the commits in %s came as %d fields, not three to a commit", repo, len(fields))
	}
	for i := 0; i < len(fields); i += 3 {
		at, err := strconv.ParseInt(string(fields[i+1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git: commit %s in %s has author time %q", fields[i], repo, fields[i+1])
		}
		when := time.Unix(at, 0)
		if when.Before(from) || !when.Before(to) {
			continue
		}
		out = append(out, AuthoredCommit{Hash: string(fields[i]), At: when, Subject: string(fields[i+2])})
	}
	// git log answers by committer time; the answer is by author time.
	slices.SortStableFunc(out, func(a, b AuthoredCommit) int { return a.At.Compare(b.At) })
	return out, nil
}
