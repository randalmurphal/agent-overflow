package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// RepoIdentity is the two facts that name the REPOSITORY a checkout is of,
// rather than the directory it happens to sit in: the `origin` remote and the
// root commit of HEAD. A client attached to several backends uses the pair to
// recognise the same repository cloned on two machines as one project.
//
// RemoteURL is git's own spelling, verbatim. Nothing is normalised here on
// purpose: the matching happens on the client, so the client owns the
// normalisation (scheme, user, `.git` suffix, host case), and a backend that
// pre-chewed the string would only give it a second, disagreeing dialect to
// reconcile.
//
// RootCommit is the LEXICOGRAPHICALLY SMALLEST parentless commit reachable
// from HEAD. A repository can have several roots — an orphan branch merged in,
// a history grafted from another project — and `rev-list` orders them by
// traversal, which differs with the checked-out branch. Sorting is what makes
// two machines answer the same string for the same repository.
type RepoIdentity struct {
	// Repository is false when cwd does not exist or is not inside a
	// repository. Both other fields are then empty.
	Repository bool
	// RemoteURL is "" when the repository has no `origin` remote.
	RemoteURL string
	// RootCommit is "" when HEAD is unborn.
	RootCommit string
}

// ReadRepoIdentity reads cwd's repository identity, keeping the three answers
// apart: not a repository (Repository false, nil error), a repository whose
// identity is partly or wholly empty (no origin, unborn HEAD), and a git
// command that failed (an error naming the command and git's message). A
// failure is never reported as "not a repository": a checkout git refuses to
// read, such as one with dubious ownership, would otherwise look like a plain
// folder and never match its other checkouts.
//
// knownRootCommit, when non-empty and still a commit in this repository, is
// returned as the root commit without walking history again. The root of an
// existing history does not move in practice, and the walk is the one read
// here whose cost grows with the repository. A known root the repository does
// not contain (the path now holds another repository) is read fresh.
//
// The origin read is uncached on purpose: the result is persisted, and a
// remote reconfigured within the status cache's window must not be stored as
// its previous value.
//
// ctx bounds every git command. A read cut short by ctx returns an error that
// says nothing about the checkout, so the caller must not record it.
func (c *Core) ReadRepoIdentity(ctx context.Context, cwd, knownRootCommit string) (RepoIdentity, error) {
	if cwd == "" {
		return RepoIdentity{}, nil
	}
	if _, err := os.Stat(cwd); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RepoIdentity{}, nil
		}
		return RepoIdentity{}, fmt.Errorf("read repository identity: %w", err)
	}
	if _, ok, err := c.revParsePathContext(ctx, cwd, "--git-common-dir"); err != nil {
		return RepoIdentity{}, err
	} else if !ok {
		return RepoIdentity{}, nil
	}
	remoteURL, err := c.originURL(ctx, cwd)
	if err != nil {
		return RepoIdentity{}, err
	}
	rootCommit, err := c.rootCommit(ctx, cwd, knownRootCommit)
	if err != nil {
		return RepoIdentity{}, err
	}
	return RepoIdentity{Repository: true, RemoteURL: remoteURL, RootCommit: rootCommit}, nil
}

// originURL reads the `origin` remote, "" when the repository has none.
func (c *Core) originURL(ctx context.Context, cwd string) (string, error) {
	result, err := c.identityGit(ctx, cwd, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	switch result.exitCode {
	case 0:
		return strings.TrimSpace(result.stdout), nil
	case 2:
		// git's documented status for "No such remote".
		return "", nil
	}
	return "", commandFailure("git remote get-url origin", result)
}

// rootCommit returns the smallest of HEAD's parentless commits, "" for an
// unborn HEAD. See ReadRepoIdentity for knownRootCommit.
func (c *Core) rootCommit(ctx context.Context, cwd, knownRootCommit string) (string, error) {
	head, err := c.identityGit(ctx, cwd, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	switch head.exitCode {
	case 0:
	case 1:
		// --verify --quiet exits 1 with no output when HEAD names nothing yet.
		return "", nil
	default:
		return "", commandFailure("git rev-parse HEAD", head)
	}
	if isObjectName(knownRootCommit) {
		known, err := c.identityGit(ctx, cwd, "cat-file", "-e", knownRootCommit+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("git cat-file: %w", err)
		}
		if known.exitCode == 0 {
			return knownRootCommit, nil
		}
	}
	result, err := c.identityGit(ctx, cwd, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-list: %w", err)
	}
	if result.exitCode != 0 {
		return "", commandFailure("git rev-list --max-parents=0 HEAD", result)
	}
	smallest := ""
	for _, line := range strings.Split(result.stdout, "\n") {
		root := strings.TrimSpace(line)
		if root == "" {
			continue
		}
		if smallest == "" || root < smallest {
			smallest = root
		}
	}
	return smallest, nil
}

// identityGit runs one identity read in the C locale, bounded by ctx.
func (c *Core) identityGit(ctx context.Context, cwd string, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{ctx: ctx, binary: "git", cwd: cwd, extraEnv: localeCEnv, args: args})
}

// isObjectName reports whether s is a full SHA-1 or SHA-256 object name, so a
// stored value can never reach git as an option or a revision expression.
func isObjectName(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// commandFailure is a non-zero git exit as an error carrying git's message.
// Only stderr is quoted: the stdout of `remote get-url` is the remote URL,
// which can carry credentials.
func commandFailure(operation string, result commandResult) error {
	message := strings.TrimSpace(result.stderr)
	if message == "" {
		return fmt.Errorf("%s failed with exit status %d", operation, result.exitCode)
	}
	return fmt.Errorf("%s failed: %s", operation, message)
}
