package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// GitPR describes a pull request returned by GitHub CLI.
type GitPR struct {
	URL    string `json:"url"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// CreatePR opens a pull request via GitHub CLI and returns the created URL.
func (c *Core) CreatePR(cwd, title, body string) (string, error) {
	if strings.TrimSpace(title) == "" {
		return "", errors.New("pull request title is required")
	}

	result, err := c.runBinary("gh", cwd, "pr", "create", "--title", title, "--body", body)
	if err != nil {
		return "", normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("gh pr create failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	url := strings.TrimSpace(result.stdout)
	if url == "" {
		return "", errors.New("gh pr create returned empty URL")
	}
	return url, nil
}

// ListOpenPRs returns open pull requests for the given head branch.
func (c *Core) ListOpenPRs(cwd, head string) ([]GitPR, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("pull request head branch is required")
	}

	result, err := c.runBinary(
		"gh",
		cwd,
		"pr",
		"list",
		"--head",
		head,
		"--state",
		"open",
		"--json",
		"url,number,title,state",
	)
	if err != nil {
		return nil, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("gh pr list failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	stdout := strings.TrimSpace(result.stdout)
	if stdout == "" {
		return nil, nil
	}

	var pulls []GitPR
	if err := json.Unmarshal([]byte(stdout), &pulls); err != nil {
		return nil, fmt.Errorf("decode gh pr list output: %w", err)
	}
	return pulls, nil
}

func normalizeGitHubCLIError(err error) error {
	var execErr *exec.Error
	if errors.As(err, &execErr) || errors.Is(err, exec.ErrNotFound) {
		return errors.New("GitHub CLI (`gh`) is not installed or not on PATH")
	}
	return err
}

func commandOutputMessage(stdout, stderr string) string {
	if message := strings.TrimSpace(stderr); message != "" {
		return message
	}
	if message := strings.TrimSpace(stdout); message != "" {
		return message
	}
	return "command failed"
}
