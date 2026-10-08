package git

import (
	"agent-overflow/internal/repoidentity"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// RepoIdentity contains a transient Git origin and its verified forge identity.
// RemoteURL never crosses the project service boundary. IdentitySource is a
// local cache invalidation stamp, never a key for matching different projects.
type RepoIdentity struct {
	RepositoryID   string
	IdentitySource string
	LookupError    string
	// LookupRetryable marks a LookupError that came from the forge or its CLI
	// being unavailable, which a later lookup can clear with no change to the
	// checkout.
	LookupRetryable bool
	Repository      bool
	RemoteURL       string
}

// ReadRepoIdentity reads the local origin without walking commit history.
// Missing paths, plain directories and failed Git commands remain distinct.
func (c *Core) ReadRepoIdentity(ctx context.Context, cwd string) (RepoIdentity, error) {
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
	remote, err := c.originURL(ctx, cwd)
	if err != nil {
		return RepoIdentity{}, err
	}
	return RepoIdentity{Repository: true, RemoteURL: remote}, nil
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

// identityGit runs one identity read in the C locale, bounded by ctx.
func (c *Core) identityGit(ctx context.Context, cwd string, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{ctx: ctx, binary: "git", cwd: cwd, extraEnv: localeCEnv, args: args})
}

// commandFailure is a non-zero git exit as an error carrying git's message.
// Only stderr is quoted: the stdout of `remote get-url` is the remote URL,
// which can carry credentials.
func commandFailure(operation string, result commandResult) error {
	message := repoidentity.RedactText(strings.TrimSpace(result.stderr))
	if message == "" {
		return fmt.Errorf("%s failed with exit status %d", operation, result.exitCode)
	}
	return fmt.Errorf("%s failed: %s", operation, message)
}
