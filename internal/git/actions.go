package git

import (
	"fmt"
	"strconv"
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

// Push publishes the current branch. If no upstream exists, it sets one first.
func (c *Core) Push(cwd string) error {
	branch, err := c.currentBranch(cwd)
	if err != nil {
		return err
	}

	hasUpstream, err := c.branchHasUpstream(cwd)
	if err != nil {
		return err
	}
	if hasUpstream {
		_, _, err = c.Execute(cwd, "push")
		return err
	}

	remote, err := c.pushRemoteName(cwd)
	if err != nil {
		return err
	}

	_, _, err = c.Execute(cwd, "push", "--set-upstream", remote, branch)
	return err
}

// Pull fast-forwards the current branch from its upstream.
func (c *Core) Pull(cwd string) error {
	_, _, err := c.Execute(cwd, "pull", "--ff-only")
	return err
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
	_, _, err := c.Execute(cwd, "checkout", "-b", name)
	return err
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

	_, _, err := c.Execute(cwd, "branch", name)
	return err
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

func (c *Core) currentBranch(cwd string) (string, error) {
	stdout, _, err := c.Execute(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}

	branch := strings.TrimSpace(stdout)
	if branch == "" || branch == "HEAD" {
		return "", fmt.Errorf("cannot operate on detached HEAD")
	}
	return branch, nil
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

// StashPushIncludeUntracked stashes the working tree (staged + unstaged +
// untracked) under message. Returns created=false when git reports nothing
// to stash; the caller skips the carry-over in that case.
func (c *Core) StashPushIncludeUntracked(cwd, message string) (bool, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return false, fmt.Errorf("git stash message is required")
	}
	stdout, _, err := c.Execute(cwd, "stash", "push", "-u", "-m", message)
	if err != nil {
		return false, err
	}
	if strings.Contains(stdout, "No local changes to save") {
		return false, nil
	}
	return true, nil
}

// findStashRefByMessage scans the stash list for the entry whose message ends
// with the supplied marker and returns its ref (e.g. "stash@{0}"). Looking up
// by message means concurrent carry-overs in the same repo never collide.
//
// Match is HasSuffix only — `git stash push -m <message>` produces a
// reflog subject of `On <branch>: <message>`, so the marker always
// sits at the end. A Contains fallback would let an unrelated stash
// whose message happens to contain our hex token (e.g. an external
// `git stash push -m "ao-carry-deadbeef wip"` from outside the app)
// resolve preferentially over the intended ref.
func (c *Core) findStashRefByMessage(cwd, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("git stash message is required")
	}
	stdout, _, err := c.Execute(cwd, "stash", "list", "--format=%gd %s")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, " ")
		if idx <= 0 {
			continue
		}
		ref := line[:idx]
		desc := strings.TrimSpace(line[idx+1:])
		if strings.HasSuffix(desc, message) {
			return ref, nil
		}
	}
	return "", fmt.Errorf("stash entry %q not found", message)
}

// StashApplyByMessage looks up the stash entry by message and applies it.
// `apply` (not `pop`) leaves the stash entry intact so a failed apply doesn't
// destroy the snapshot.
func (c *Core) StashApplyByMessage(cwd, message string) error {
	ref, err := c.findStashRefByMessage(cwd, message)
	if err != nil {
		return err
	}
	_, stderr, err := c.Execute(cwd, "stash", "apply", ref)
	if err != nil {
		trimmed := strings.TrimSpace(stderr)
		if trimmed == "" {
			trimmed = err.Error()
		}
		return fmt.Errorf("git stash apply %s: %s", ref, trimmed)
	}
	return nil
}

// StashDropByMessage drops the stash entry whose message matches.
func (c *Core) StashDropByMessage(cwd, message string) error {
	ref, err := c.findStashRefByMessage(cwd, message)
	if err != nil {
		return err
	}
	_, _, err = c.Execute(cwd, "stash", "drop", ref)
	return err
}

// CountUnpushedCommits returns the number of commits on branch that have not
// been pushed to origin/<branch>. Returns hasUpstream=false when no upstream
// is configured (in which case the count is meaningless and the caller should
// treat the branch as "no remote"). The implementation queries the symbolic
// upstream so non-origin remotes work too.
func (c *Core) CountUnpushedCommits(cwd, branch string) (count int, hasUpstream bool, err error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return 0, false, nil
	}
	upstream, ok := c.upstreamFor(cwd, branch)
	if !ok {
		return 0, false, nil
	}
	stdout, _, err := c.Execute(cwd, "rev-list", "--count", branch, "^"+upstream)
	if err != nil {
		return 0, true, err
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(stdout))
	if parseErr != nil {
		return 0, true, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(stdout), parseErr)
	}
	return value, true, nil
}

// upstreamFor resolves the upstream ref for branch (e.g. "origin/main").
// Returns false when no upstream is configured.
func (c *Core) upstreamFor(cwd, branch string) (string, bool) {
	result, err := c.run(cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}")
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
// unstaged, untracked) in cwd via `git status --porcelain`.
func (c *Core) CountWorkingTreeChanges(cwd string) (int, error) {
	stdout, _, err := c.Execute(cwd, "status", "--porcelain")
	if err != nil {
		return 0, err
	}
	count := 0
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
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
	result, err := c.run(cwd, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil && result.exitCode == 0
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
