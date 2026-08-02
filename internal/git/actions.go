package git

import (
	"fmt"
	"strings"
	"unicode"
)

// StageAll runs `git add -A` to stage all tracked and untracked changes.
// Callers should be aware this stages everything, including untracked files.
func (c *Core) StageAll(cwd string) error {
	_, _, err := c.Execute(cwd, "add", "-A")
	return err
}

// Commit creates a commit from whatever is currently staged, returning the new
// commit SHA. It does NOT stage changes automatically -- call StageAll first if
// the intent is to commit everything.
func (c *Core) Commit(cwd, subject, body string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", fmt.Errorf("git commit subject is required")
	}

	if _, _, err := c.Execute(cwd, commitArgs(subject, body)...); err != nil {
		return "", err
	}

	stdout, _, err := c.Execute(cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// Push publishes the current branch on behalf of a person who pressed a
// button and is waiting for the result. If no upstream exists, it sets one
// first. A remote that needs credentials is allowed to prompt — that is the
// working flow for a GUI credential helper.
func (c *Core) Push(cwd string) error {
	return c.push(cwd, true)
}

// PushUnattended is Push for automation running with nobody watching (the
// workflows engine's disposition step). A credential prompt there is not a
// question anyone can answer: the dialog would sit behind the app window
// until our 45s subprocess timeout fires, or forever if a GUI helper owns
// it. Failing fast turns that into a run error the disposition surfaces.
func (c *Core) PushUnattended(cwd string) error {
	return c.push(cwd, false)
}

// push is the single implementation behind Push and PushUnattended. The two
// differ ONLY in whether a credential prompt may appear: argv is resolved
// once here so the attended and unattended paths cannot drift apart.
func (c *Core) push(cwd string, allowCredentialPrompt bool) error {
	args, err := c.pushArgs(cwd)
	if err != nil {
		return err
	}
	_, _, err = c.executeSpec(commandSpec{
		binary:                "git",
		cwd:                   cwd,
		allowCredentialPrompt: allowCredentialPrompt,
		args:                  args,
	})
	return err
}

// pushArgs resolves the push argv: a bare `push` when the branch already
// has an upstream, otherwise one that sets it. The current-branch read runs
// first in both cases so a detached HEAD is refused before we shell out.
func (c *Core) pushArgs(cwd string) ([]string, error) {
	branch, err := c.currentBranch(cwd)
	if err != nil {
		return nil, err
	}
	hasUpstream, err := c.branchHasUpstream(cwd)
	if err != nil {
		return nil, err
	}
	if hasUpstream {
		return []string{"push"}, nil
	}
	remote, err := c.pushRemoteName(cwd)
	if err != nil {
		return nil, err
	}
	return []string{"push", "--set-upstream", remote, branch}, nil
}

// Pull fast-forwards the current branch from its upstream.
func (c *Core) Pull(cwd string) error {
	_, _, err := c.executeInteractive(cwd, "pull", "--ff-only")
	return err
}

// SyncBranch enforces FF-only sync from upstream. Current branch goes
// through `git pull --ff-only` (touches HEAD/index/working tree);
// other branches go through `git fetch <remote> <remoteBranch>:<branch>`,
// which git natively refuses unless the update is fast-forward (the
// refspec has no leading `+`).
func (c *Core) SyncBranch(cwd, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("git sync branch name is required")
	}
	if err := validateBranchName(branch); err != nil {
		return err
	}

	upstream, ok := c.upstreamFor(cwd, branch)
	if !ok {
		return fmt.Errorf("branch %q has no upstream configured", branch)
	}

	if current, err := c.currentBranch(cwd); err == nil && current == branch {
		return c.Pull(cwd)
	}

	remote, remoteBranch, ok := splitUpstreamRef(upstream, c.listRemoteNames(cwd))
	if !ok {
		return fmt.Errorf("cannot parse upstream ref %q for branch %q", upstream, branch)
	}
	// Re-validate remoteBranch even though it came from upstream config:
	// the trust boundary stays uniform with the caller's `branch` input,
	// and a malformed `.git/config` produces a clean error instead of
	// reaching argv.
	if err := validateBranchName(remoteBranch); err != nil {
		return fmt.Errorf("invalid upstream branch in %q: %w", upstream, err)
	}

	refspec := fmt.Sprintf("%s:%s", remoteBranch, branch)
	_, _, err := c.executeInteractive(cwd, "fetch", remote, refspec)
	return err
}

// splitUpstreamRef splits "<remote>/<branch>" using the repo's known
// remote names, since remote names can legally contain slashes.
// Returns false when none of the configured remotes prefixes the ref
// or when the trailing branch part is empty.
func splitUpstreamRef(ref string, remoteNames []string) (string, string, bool) {
	for _, r := range remoteNames {
		prefix := r + "/"
		if strings.HasPrefix(ref, prefix) {
			remoteBranch := strings.TrimPrefix(ref, prefix)
			if remoteBranch == "" {
				return "", "", false
			}
			return r, remoteBranch, true
		}
	}
	return "", "", false
}

// MaybeFetchRemotes runs `git fetch --all` against cwd's repository iff
// the last successful fetch is older than FetchStaleWindow. Returns
// (true, nil) when a fetch actually ran, (false, nil) when the cache was
// fresh and skipped. A fetch failure is returned but does not update the
// staleness clock, so the next call will retry.
//
// The freshness clock is shared with FetchRemotesBackground and
// PruneRemotes and keyed by the canonical git common dir, so every
// worktree of a repository — and the background cadence — collapse onto
// one fetch per window (`git fetch` mutates `refs/remotes/*`, which
// lives in the shared common dir).
//
// Not single-flighted, unlike the background cadence: this is one
// user-initiated warm-up per picker open, and joining it to an in-flight
// origin-only background fetch would silently narrow it from --all.
// Two concurrent fetches of one repo are safe (git locks per ref); the
// shared window makes the overlap window at most one fetch long.
//
// This is a network command nobody explicitly asked for (the branch
// picker warms it on open), so it deliberately keeps the non-interactive
// default: against a credential-requiring remote it fails fast with a
// readable error instead of raising a GUI credential dialog the user
// cannot connect to any action of theirs.
func (c *Core) MaybeFetchRemotes(cwd string) (bool, error) {
	key, err := c.CommonDir(cwd)
	if err != nil {
		return false, err
	}
	if c.fetchIsFresh(key) {
		return false, nil
	}

	if _, _, err := c.Execute(cwd, "fetch", "--all"); err != nil {
		return false, err
	}

	c.stampFetchCache(key)
	return true, nil
}

// PruneRemotes runs `git fetch --all --prune` and refreshes the
// staleness clock so the subsequent picker open doesn't double-fetch.
// Surfaces fetch errors to the caller for toast display.
func (c *Core) PruneRemotes(cwd string) error {
	key, err := c.CommonDir(cwd)
	if err != nil {
		return err
	}

	if _, _, err := c.executeInteractive(cwd, "fetch", "--all", "--prune"); err != nil {
		return err
	}

	c.stampFetchCache(key)
	return nil
}

// stampFetchCache records the current time as the last successful fetch
// for a repository (key: canonical git common dir), sweeping any entries
// older than 2× FetchStaleWindow so the map stays bounded by
// recently-active repos rather than the lifetime total. The 2× floor
// keeps a freshly-stamped sibling repo from being dropped mid-window.
func (c *Core) stampFetchCache(key string) {
	now := c.nowFn()
	c.fetchCacheMu.Lock()
	c.fetchCache[key] = now
	floor := now.Add(-2 * FetchStaleWindow)
	for k, last := range c.fetchCache {
		if last.Before(floor) {
			delete(c.fetchCache, k)
		}
	}
	c.fetchCacheMu.Unlock()
}

// InvalidateFetchCache drops the staleness clock for cwd's repo so the
// next MaybeFetchRemotes / FetchRemotesBackground call refetches.
// Callers that mutate remote-tracking refs out of band should use this
// to keep the picker honest. Mirrors InvalidatePRCache /
// InvalidateForgeCache.
func (c *Core) InvalidateFetchCache(cwd string) {
	key, err := c.CommonDir(cwd)
	if err != nil {
		return
	}
	c.fetchCacheMu.Lock()
	delete(c.fetchCache, key)
	c.fetchCacheMu.Unlock()
}

// Checkout switches to an existing branch.
func (c *Core) Checkout(cwd, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("git checkout branch is required")
	}
	if err := validateBranchName(branch); err != nil {
		return err
	}

	if local, ok := c.localBranchFromRemote(cwd, branch); ok {
		if err := validateBranchName(local); err != nil {
			return err
		}
		if c.branchExists(cwd, local) {
			_, _, err := c.Execute(cwd, "checkout", local)
			return err
		}
		_, _, err := c.Execute(cwd, "checkout", "--track", branch)
		return err
	}

	_, _, err := c.Execute(cwd, "checkout", branch)
	return err
}

// CheckoutNewBranch runs `git checkout -b <name>`, creating the branch
// at HEAD and switching to it in one step. Validates the name through
// the same gate Checkout / CreateBranch use so the App layer can't
// smuggle a flag-shaped string into argv (e.g. `--orphan`, `-f`).
func (c *Core) CheckoutNewBranch(cwd, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("git checkout new branch name is required")
	}
	if err := validateBranchName(name); err != nil {
		return err
	}
	if err := c.ensureBranchDoesNotExist(cwd, name); err != nil {
		return err
	}
	_, stderr, err := c.Execute(cwd, "checkout", "-b", name)
	return c.branchCreationError(cwd, err, stderr, name)
}

// CreateBranch creates a branch without switching to it.
func (c *Core) CreateBranch(cwd, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("git branch name is required")
	}
	if err := validateBranchName(name); err != nil {
		return err
	}
	if err := c.ensureBranchDoesNotExist(cwd, name); err != nil {
		return err
	}

	_, stderr, err := c.Execute(cwd, "branch", name)
	return c.branchCreationError(cwd, err, stderr, name)
}

// RenameBranch renames a local branch, appending a numeric suffix if the
// desired target already exists.
func (c *Core) RenameBranch(cwd, oldBranch, newBranch string) (string, error) {
	oldBranch = strings.TrimSpace(oldBranch)
	if oldBranch == "" {
		return "", fmt.Errorf("git old branch name is required")
	}
	newBranch = strings.TrimSpace(newBranch)
	if newBranch == "" {
		return "", fmt.Errorf("git new branch name is required")
	}
	if err := validateBranchName(oldBranch); err != nil {
		return "", err
	}
	if err := validateBranchName(newBranch); err != nil {
		return "", err
	}
	if oldBranch == newBranch {
		return newBranch, nil
	}

	target := newBranch
	for suffix := 1; c.branchExists(cwd, target); suffix++ {
		target = fmt.Sprintf("%s-%d", newBranch, suffix)
		if suffix >= 100 {
			return "", fmt.Errorf("could not find an available branch name for %q", newBranch)
		}
	}

	_, _, err := c.Execute(cwd, "branch", "-m", "--", oldBranch, target)
	if err != nil {
		return "", err
	}
	return target, nil
}

func commitArgs(subject, body string) []string {
	args := []string{"commit", "-m", subject}
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		args = append(args, "-m", trimmed)
	}
	return args
}

func (c *Core) branchHasUpstream(cwd string) (bool, error) {
	result, err := c.run(cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return false, err
	}
	if result.exitCode != 0 {
		return false, nil
	}
	return strings.TrimSpace(result.stdout) != "", nil
}

// validateBranchName rejects branch names that could be misinterpreted as
// flags or contain sequences unsafe for git ref names.
func validateBranchName(name string) error {
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid branch name %q: must not start with -", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("invalid branch name %q: must not contain ..", name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("invalid branch name %q: must not contain NUL", name)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid branch name %q: must not contain control characters", name)
		}
	}
	return nil
}

func (c *Core) pushRemoteName(cwd string) (string, error) {
	if c.originRemoteExists(cwd) {
		return "origin", nil
	}

	remotes := c.listRemoteNames(cwd)
	if len(remotes) == 0 {
		return "", fmt.Errorf("cannot push because no git remote is configured")
	}
	return remotes[0], nil
}

func (c *Core) branchExists(cwd, branch string) bool {
	exists, err := c.branchExistsChecked(cwd, branch)
	return err == nil && exists
}

func (c *Core) EnsureLocalBranchDoesNotExist(cwd, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("git branch name is required")
	}
	if err := validateBranchName(name); err != nil {
		return err
	}
	return c.ensureBranchDoesNotExist(cwd, name)
}

func branchAlreadyExistsError(name string) error {
	return fmt.Errorf("branch %q already exists", name)
}

func (c *Core) ensureBranchDoesNotExist(cwd, name string) error {
	exists, err := c.branchExistsChecked(cwd, name)
	if err != nil {
		return fmt.Errorf("check branch %q: %w", name, err)
	}
	if exists {
		return branchAlreadyExistsError(name)
	}
	return nil
}

func (c *Core) branchExistsChecked(cwd, branch string) (bool, error) {
	result, err := c.run(cwd, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	switch result.exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		message := strings.TrimSpace(result.stderr)
		if message == "" {
			message = fmt.Sprintf("git show-ref refs/heads/%s exited with code %d", branch, result.exitCode)
		}
		return false, fmt.Errorf("%s", message)
	}
}

func (c *Core) branchCreationError(cwd string, err error, stderr, name string) error {
	if err == nil {
		return nil
	}
	if c.branchAlreadyExistsAfterCreationError(cwd, stderr, name) {
		return branchAlreadyExistsError(name)
	}
	return err
}

func (c *Core) branchAlreadyExistsAfterCreationError(cwd, stderr, name string) bool {
	if gitReportsBranchAlreadyExists(stderr, name) {
		return true
	}
	exists, err := c.branchExistsChecked(cwd, name)
	return err == nil && exists
}

func gitReportsBranchAlreadyExists(stderr, name string) bool {
	message := strings.ToLower(stderr)
	if !strings.Contains(message, "exists") {
		return false
	}
	normalizedName := strings.ToLower(strings.TrimSpace(name))
	if normalizedName != "" && !strings.Contains(message, normalizedName) {
		return false
	}
	return strings.Contains(message, "branch") || strings.Contains(message, "refs/heads/")
}

func (c *Core) localBranchFromRemote(cwd, branch string) (string, bool) {
	for _, remote := range c.listRemoteNames(cwd) {
		prefix := remote + "/"
		if !strings.HasPrefix(branch, prefix) {
			continue
		}
		local := strings.TrimPrefix(branch, prefix)
		if local == "" || strings.HasSuffix(local, "/HEAD") {
			return "", false
		}
		return local, true
	}
	return "", false
}
