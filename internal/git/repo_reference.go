package git

import (
	"context"
	"errors"
	"fmt"
)

// RepoReference identifies a repository on a forge host: a PRReference
// without its number. Repository-scoped reads that take no checkout
// (ListProjectPRs, ListPipelines, ReadPipeline, StreamCIJobLog) address
// the repository by it; its fields mean what PRReference's do.
type RepoReference struct {
	Forge     string // "github" | "gitlab"
	Host      string // URL.host spelling, as PRReference.Host
	Namespace string // "owner" or "group/sub/..."
	Repo      string
}

// Repository is the repository r's PR lives in.
func (r PRReference) Repository() RepoReference {
	return RepoReference{Forge: r.Forge, Host: r.Host, Namespace: r.Namespace, Repo: r.Repo}
}

// Project returns "namespace/repo", the repository path within Host.
func (r RepoReference) Project() string {
	return PRReference{Namespace: r.Namespace, Repo: r.Repo}.Project()
}

// Key is the repository's entity key: "forge:namespace/repo" on the
// forge's public host and "forge@host:namespace/repo" on any other, the
// PR key (PRReference.Key) without its number. Keys are compared, never
// parsed.
func (r RepoReference) Key() string {
	if publicForgeHosts[r.Forge] == r.Host {
		return fmt.Sprintf("%s:%s", r.Forge, r.Project())
	}
	return fmt.Sprintf("%s@%s:%s", r.Forge, r.Host, r.Project())
}

// Validate reports whether r names a repository a forge operation can
// address: a supported forge, a lowercase host, and a project path that
// satisfies SplitProjectForForge.
func (r RepoReference) Validate() error {
	if _, ok := publicForgeHosts[r.Forge]; !ok {
		return fmt.Errorf("unsupported forge %q: %w", r.Forge, ErrUnsupportedForge)
	}
	if err := validatePRHost(r.Host); err != nil {
		return err
	}
	_, _, err := SplitProjectForForge(r.Forge, r.Project())
	return err
}

// ErrOriginNotForge matches every *OriginUnknownError: the checkout's
// origin remote is missing or does not name a repository on a supported
// forge, so it has no forge repository. A checkout git could not read
// fails with another error.
var ErrOriginNotForge = errors.New("origin is not a known forge repository")

// OriginRepository is the forge repository cwd's origin remote names,
// from the origin identity forge detection caches (read again when none
// is cached). A checkout whose origin names no repository on a
// supported forge fails with an *OriginUnknownError, which matches
// ErrOriginNotForge; one whose origin git failed to read (not a
// repository, a git or I/O failure) fails with that cause, which does
// not. The host is the remote's hostname, lowercase.
func (c *Core) OriginRepository(ctx context.Context, cwd string) (RepoReference, error) {
	if err := ctx.Err(); err != nil {
		return RepoReference{}, err
	}
	forge, host, project, err := c.originCoordinates(cwd)
	if err != nil {
		return RepoReference{}, err
	}
	namespace, repo, err := SplitProjectForForge(forge, project)
	if err != nil {
		return RepoReference{}, &OriginUnknownError{Cwd: cwd, Reason: "the origin URL does not name a " + forge + " repository"}
	}
	return RepoReference{Forge: forge, Host: host, Namespace: namespace, Repo: repo}, nil
}
