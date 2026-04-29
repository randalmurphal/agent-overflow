package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GitPR describes a pull request returned by a forge CLI. Fields are
// shared across forges (gh and glab both expose URL/number/title/state
// equivalents); per-forge wrappers map their native JSON shapes onto
// this struct.
type GitPR struct {
	URL    string `json:"url"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// githubForge implements Forge using the gh CLI. All operations route
// through the owning Core's runBinary for timeout + size-cap discipline.
type githubForge struct {
	core *Core
}

func (f *githubForge) ID() string         { return "github" }
func (f *githubForge) BinaryName() string { return "gh" }

// CreatePR opens a pull request via GitHub CLI and returns the created URL.
// When draft is true the PR is opened as a draft (gh pr create --draft).
func (f *githubForge) CreatePR(cwd, title, body string, draft bool) (string, error) {
	if strings.TrimSpace(title) == "" {
		return "", errors.New("pull request title is required")
	}

	args := []string{"pr", "create", "--title", title, "--body", body}
	if draft {
		args = append(args, "--draft")
	}
	result, err := f.core.runBinary("gh", cwd, args...)
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
func (f *githubForge) ListOpenPRs(cwd, head string) ([]GitPR, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("pull request head branch is required")
	}

	result, err := f.core.runBinary(
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
	// gh emits states uppercase ("OPEN" / "CLOSED" / "MERGED"); collapse
	// to the canonical lowercase vocabulary so callers don't have to
	// branch on forge.
	for i := range pulls {
		pulls[i].State = NormalizePRState(pulls[i].State)
	}
	return pulls, nil
}

// ViewPR fetches PR metadata via `gh pr view --json ...`. project is
// "owner/repo"; cwd may be empty when there is no local clone.
func (f *githubForge) ViewPR(cwd, project string, number int) (PRMetadata, error) {
	if strings.TrimSpace(project) == "" {
		return PRMetadata{}, errors.New("project (owner/repo) is required")
	}
	if number <= 0 {
		return PRMetadata{}, fmt.Errorf("PR number must be positive, got %d", number)
	}

	result, err := f.core.runBinary(
		"gh",
		cwd,
		"pr", "view",
		"--repo", project,
		strconv.Itoa(number),
		"--json", "title,body,headRefName,baseRefName,files,url,author,state",
	)
	if err != nil {
		return PRMetadata{}, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return PRMetadata{}, fmt.Errorf("gh pr view failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	var raw struct {
		Title       string   `json:"title"`
		Body        string   `json:"body"`
		HeadRefName string   `json:"headRefName"`
		BaseRefName string   `json:"baseRefName"`
		URL         string   `json:"url"`
		Files       []PRFile `json:"files"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		return PRMetadata{}, fmt.Errorf("gh pr view returned malformed JSON: %w", err)
	}
	return PRMetadata{
		Title:       raw.Title,
		Body:        raw.Body,
		HeadRefName: raw.HeadRefName,
		BaseRefName: raw.BaseRefName,
		URL:         raw.URL,
		AuthorLogin: raw.Author.Login,
		State:       NormalizePRState(raw.State),
		Files:       raw.Files,
	}, nil
}

// Diff returns the unified diff for a PR via `gh pr diff`.
func (f *githubForge) Diff(cwd, project string, number int) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project (owner/repo) is required")
	}
	if number <= 0 {
		return "", fmt.Errorf("PR number must be positive, got %d", number)
	}

	result, err := f.core.runBinary(
		"gh",
		cwd,
		"pr", "diff",
		"--repo", project,
		strconv.Itoa(number),
	)
	if err != nil {
		return "", normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("gh pr diff failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
	return result.stdout, nil
}

// CreatePR is a thin wrapper that dispatches to the forge detected for
// cwd. Returns ErrUnsupportedForge (via nullForge) when the origin
// remote is missing or its host is not a recognised forge.
func (c *Core) CreatePR(cwd, title, body string, draft bool) (string, error) {
	return c.forgeFor(cwd).CreatePR(cwd, title, body, draft)
}

// ListOpenPRs is a thin wrapper that dispatches to the forge detected
// for cwd. See CreatePR for the dispatch model.
func (c *Core) ListOpenPRs(cwd, head string) ([]GitPR, error) {
	return c.forgeFor(cwd).ListOpenPRs(cwd, head)
}

func normalizeGitHubCLIError(err error) error {
	if _, ok := errors.AsType[*exec.Error](err); ok || errors.Is(err, exec.ErrNotFound) {
		return errors.New(
			"GitHub CLI (`gh`) is not installed or not on PATH. Install from https://cli.github.com and run 'gh auth login' to continue",
		)
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
