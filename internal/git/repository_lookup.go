package git

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/repoidentity"
)

type repositoryLookupResult struct {
	id, problem string
	until       time.Time
}
type repositoryLookups struct {
	mu      sync.Mutex
	cache   map[string]repositoryLookupResult
	pending map[string]chan struct{}
}

// ResolveRepository enriches local Git coordinates using the owning computer's
// forge login. Failures remain visible; the transient origin is discarded.
// No response body or CLI stderr is copied into persistent diagnostics.
func (c *Core) ResolveRepository(ctx context.Context, cwd string, identity RepoIdentity) RepoIdentity {
	caller := ctx
	raw := identity.RemoteURL
	identity.RemoteURL = ""
	locator := repoidentity.Locator(raw)
	if locator == "" {
		if identity.Repository {
			identity.LookupError = "This checkout has no supported forge origin. Repository identity could not be verified."
		}
		return identity
	}
	// This checkout-local stamp must also be available when admission or SSH
	// resolution fails. It never matches different projects or computers.
	identity.IdentitySource = fmt.Sprintf("%x", sha256.Sum256([]byte(locator)))
	host, project, _ := strings.Cut(locator, "/")
	forge := classifyOriginURL("https://"+locator, c.gitLabHostsSnapshot())
	if forge == "" && !isSSHRemote(raw) {
		identity.IdentitySource = ""
		identity.LookupError = "Repository identity is not available for this forge."
		return identity
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	select {
	case c.repositoryLookupSlots <- struct{}{}:
		defer func() { <-c.repositoryLookupSlots }()
	case <-ctx.Done():
		identity.LookupError = "Repository identity lookup was cancelled or timed out."
		return identity
	}
	if forge == "" && isSSHRemote(raw) {
		resolved, err := c.resolveRepositorySSHHost(ctx, cwd, raw, host)
		if err != nil {
			identity.LookupError = "Could not resolve the repository's SSH host. Check this computer's SSH configuration."
			return identity
		}
		if resolved != "" && resolved != host {
			host = resolved
			if host == "ssh.github.com" {
				host = "github.com"
			}
			locator = host + "/" + project
			if host == "github.com" {
				locator = strings.ToLower(locator)
				project = strings.ToLower(project)
			}
			forge = classifyOriginURL("https://"+locator, c.gitLabHostsSnapshot())
		}
	}
	if forge == "" {
		identity.IdentitySource = ""
		identity.LookupError = "Repository identity is not available for this forge."
		return identity
	}
	key := forge + ":" + locator
	for {
		c.repositoryLookups.mu.Lock()
		if hit, ok := c.repositoryLookups.cache[key]; ok && time.Now().Before(hit.until) {
			c.repositoryLookups.mu.Unlock()
			identity.RepositoryID, identity.LookupError = hit.id, hit.problem
			return identity
		}
		if pending := c.repositoryLookups.pending[key]; pending != nil {
			c.repositoryLookups.mu.Unlock()
			select {
			case <-ctx.Done():
				identity.LookupError = "Repository identity lookup was cancelled or timed out."
				return identity
			case <-pending:
				continue
			}
		}
		if c.repositoryLookups.pending == nil {
			c.repositoryLookups.pending = make(map[string]chan struct{})
		}
		pending := make(chan struct{})
		c.repositoryLookups.pending[key] = pending
		c.repositoryLookups.mu.Unlock()
		result := c.lookupRepository(ctx, cwd, forge, host, project)
		c.repositoryLookups.mu.Lock()
		if caller.Err() == nil && (result.id != "" || result.problem != "") {
			if c.repositoryLookups.cache == nil {
				c.repositoryLookups.cache = make(map[string]repositoryLookupResult)
			}
			// Cache size is independent of the number of repositories ever visited.
			if len(c.repositoryLookups.cache) >= 128 {
				oldestKey := ""
				var oldest time.Time
				for k, v := range c.repositoryLookups.cache {
					if oldestKey == "" || v.until.Before(oldest) {
						oldestKey, oldest = k, v.until
					}
				}
				delete(c.repositoryLookups.cache, oldestKey)
			}
			result.until = time.Now().Add(5 * time.Minute)
			if result.id == "" {
				// Share failures across pending readers and the picker's inspect /
				// register pair. Keep retries after login or network recovery quick.
				result.until = time.Now().Add(5 * time.Second)
			}
			c.repositoryLookups.cache[key] = result
		}
		delete(c.repositoryLookups.pending, key)
		close(pending)
		c.repositoryLookups.mu.Unlock()
		identity.RepositoryID, identity.LookupError = result.id, result.problem
		return identity
	}
}

func (c *Core) lookupRepository(ctx context.Context, cwd, forge, host, project string) repositoryLookupResult {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// A GitHub subresource can also carry an `id`, but it is not a repository.
	// Decode once before validating and escaping the API path.
	decoded, err := url.PathUnescape(project)
	if err != nil {
		return repositoryLookupResult{problem: "The origin URL does not name a valid forge repository."}
	}
	parts := strings.Split(decoded, "/")
	if len(parts) < 2 || (forge == "github" && len(parts) != 2) {
		return repositoryLookupResult{problem: "The origin URL does not name a valid forge repository."}
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return repositoryLookupResult{problem: "The origin URL does not name a valid forge repository."}
		}
	}
	binary, endpoint := "gh", "repos/"+url.PathEscape(parts[0])+"/"+url.PathEscape(parts[1])
	if forge == "gitlab" {
		binary, endpoint = "glab", "projects/"+url.PathEscape(decoded)
	}
	result, err := c.runSpec(commandSpec{ctx: ctx, binary: binary, cwd: cwd, args: []string{"api", "--hostname", host, endpoint}})
	if err != nil || result.exitCode != 0 {
		problem := fmt.Sprintf("Could not verify repository identity with %s on %s. Check its login and network connection, then retry.", binary, host)
		if ctx.Err() != nil {
			problem = "Repository identity lookup timed out or was cancelled. Retry when the computer is available."
		}
		return repositoryLookupResult{problem: problem}
	}
	var body struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &body); err != nil {
		return repositoryLookupResult{problem: "The forge returned an invalid repository identity."}
	}
	id, err := body.ID.Int64()
	if err != nil || id <= 0 {
		return repositoryLookupResult{problem: "The forge returned no repository ID."}
	}
	return repositoryLookupResult{id: fmt.Sprintf("%s:%s:%d", forge, host, id)}
}

var sshIdentityHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func isSSHRemote(raw string) bool {
	return strings.HasPrefix(raw, "ssh://") || (!strings.Contains(raw, "://") && strings.Contains(raw, ":"))
}

func (c *Core) resolveRepositorySSHHost(ctx context.Context, cwd, raw, host string) (string, error) {
	if !sshIdentityHost.MatchString(host) {
		return "", fmt.Errorf("invalid SSH host")
	}
	args := []string{"-G", "-o", "CanonicalizeHostname=no", "-o", "PermitLocalCommand=no"}
	if strings.HasPrefix(raw, "ssh://") {
		if u, err := url.Parse(raw); err == nil && u.User != nil {
			args = append(args, "-l", u.User.Username())
		}
	} else if at := strings.Index(raw, "@"); at >= 0 {
		args = append(args, "-l", raw[:at])
	}
	args = append(args, host)
	result, err := c.runSpec(commandSpec{ctx: ctx, binary: "ssh", cwd: cwd, args: args})
	if err != nil || result.exitCode != 0 {
		return "", fmt.Errorf("SSH configuration unavailable")
	}
	for _, line := range strings.Split(result.stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "hostname" && sshIdentityHost.MatchString(fields[1]) {
			return strings.ToLower(fields[1]), nil
		}
	}
	return "", fmt.Errorf("SSH configuration contains no hostname")
}
