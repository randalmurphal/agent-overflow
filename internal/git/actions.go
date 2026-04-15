package git

import (
	"fmt"
	"strings"
)

// Commit stages all changes and creates a commit, returning the new commit SHA.
func (c *Core) Commit(cwd, subject, body string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", fmt.Errorf("git commit subject is required")
	}

	if _, _, err := c.Execute(cwd, "add", "-A"); err != nil {
		return "", err
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

	_, _, err := c.Execute(cwd, "checkout", branch)
	return err
}

// CreateBranch creates a branch without switching to it.
func (c *Core) CreateBranch(cwd, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("git branch name is required")
	}

	_, _, err := c.Execute(cwd, "branch", name)
	return err
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
