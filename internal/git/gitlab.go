package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// gitlabForge implements Forge using the glab CLI. All operations
// route through the owning Core's runBinary for timeout + size-cap
// discipline.
type gitlabForge struct {
	core *Core
}

func (f *gitlabForge) ID() string         { return "gitlab" }
func (f *gitlabForge) BinaryName() string { return "glab" }

// CreatePR opens an MR via glab and returns the created URL. The
// `draft` flag becomes `glab mr create --draft`. We rely on glab's
// default "use the current branch" behaviour rather than reading
// HEAD ourselves — same model gh uses, and avoids a hard dep on git
// being on PATH inside tests that exercise the missing-glab path.
func (f *gitlabForge) CreatePR(cwd, title, body string, draft bool) (string, error) {
	if strings.TrimSpace(title) == "" {
		return "", errors.New("merge request title is required")
	}
	args := []string{
		"mr", "create",
		"--title", title,
		"--description", body,
		"--yes",
		"--no-editor",
	}
	if draft {
		args = append(args, "--draft")
	}
	result, err := f.core.runBinary("glab", cwd, args...)
	if err != nil {
		return "", normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("glab mr create failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	url := extractMRCreateURL(result.stdout)
	if url == "" {
		return "", errors.New("glab mr create returned empty URL")
	}
	return url, nil
}

// ListOpenPRs returns open merge requests for the given source branch.
// glab's default state is `opened`, matching gh's `--state open` model
// — we omit the state flag rather than passing one.
func (f *gitlabForge) ListOpenPRs(cwd, head string) ([]GitPR, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("merge request source branch is required")
	}
	result, err := f.core.runBinary(
		"glab",
		cwd,
		"mr", "list",
		"--source-branch", head,
		"--output", "json",
	)
	if err != nil {
		return nil, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("glab mr list failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
	stdout := strings.TrimSpace(result.stdout)
	if stdout == "" || stdout == "[]" || stdout == "null" {
		return nil, nil
	}

	// glab exposes web_url and iid (project-internal MR number) which
	// we map onto the forge-agnostic GitPR shape. State values are
	// lowercase ("opened" / "closed" / "merged" / "locked").
	var raw []struct {
		WebURL string `json:"web_url"`
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return nil, fmt.Errorf("decode glab mr list output: %w", err)
	}
	pulls := make([]GitPR, 0, len(raw))
	for _, r := range raw {
		pulls = append(pulls, GitPR{
			URL:    r.WebURL,
			Number: r.IID,
			Title:  r.Title,
			// glab returns "opened"/"closed"/"merged"/"locked"; collapse
			// "opened" → "open" so callers see one canonical vocabulary.
			State: NormalizePRState(r.State),
		})
	}
	return pulls, nil
}

// ViewPR fetches MR metadata via `glab mr view <n> -R <project> --output json`.
// The `-R` flag accepts arbitrary subgroup paths (e.g. group/sub/repo).
func (f *gitlabForge) ViewPR(cwd, project string, number int) (PRMetadata, error) {
	if strings.TrimSpace(project) == "" {
		return PRMetadata{}, errors.New("project (namespace/repo) is required")
	}
	if number <= 0 {
		return PRMetadata{}, fmt.Errorf("MR number must be positive, got %d", number)
	}
	result, err := f.core.runBinary(
		"glab",
		cwd,
		"mr", "view",
		strconv.Itoa(number),
		"-R", project,
		"--output", "json",
	)
	if err != nil {
		return PRMetadata{}, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return PRMetadata{}, fmt.Errorf("glab mr view failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	// glab field naming differs from gh: description not body,
	// source_branch / target_branch instead of headRefName / baseRefName,
	// author.username instead of author.login. `mr view` does not emit
	// per-file change stats by default — Files stays empty and the
	// downstream "Files changed: N" line is gracefully omitted.
	var raw struct {
		Title        string `json:"title"`
		Description  string `json:"description"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		WebURL       string `json:"web_url"`
		State        string `json:"state"`
		Author       struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		return PRMetadata{}, fmt.Errorf("glab mr view returned malformed JSON: %w", err)
	}
	return PRMetadata{
		Title:       raw.Title,
		Body:        raw.Description,
		HeadRefName: raw.SourceBranch,
		BaseRefName: raw.TargetBranch,
		URL:         raw.WebURL,
		AuthorLogin: raw.Author.Username,
		State:       NormalizePRState(raw.State),
	}, nil
}

// Diff returns the unified diff for an MR via `glab mr diff -R <project>`.
func (f *gitlabForge) Diff(cwd, project string, number int) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project (namespace/repo) is required")
	}
	if number <= 0 {
		return "", fmt.Errorf("MR number must be positive, got %d", number)
	}
	result, err := f.core.runBinary(
		"glab",
		cwd,
		"mr", "diff",
		strconv.Itoa(number),
		"-R", project,
	)
	if err != nil {
		return "", normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("glab mr diff failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
	return result.stdout, nil
}

// extractMRCreateURL pulls the WebURL from glab's `mr create` stdout.
// The non-TTY path documented in glab source emits just the WebURL,
// but defensively pick the last URL-looking line so a minor TTY/state
// banner before it doesn't break parsing. Returns "" for stdout that
// contains no URL-like content so the caller surfaces an empty-URL
// error rather than handing a banner string to the user.
func extractMRCreateURL(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "\n") {
		if isURLLike(trimmed) {
			return trimmed
		}
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if isURLLike(line) {
			return line
		}
	}
	return ""
}

func isURLLike(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func normalizeGitLabCLIError(err error) error {
	var execErr *exec.Error
	if errors.As(err, &execErr) || errors.Is(err, exec.ErrNotFound) {
		return errors.New(
			"GitLab CLI (`glab`) is not installed or not on PATH. Install from https://gitlab.com/gitlab-org/cli and run 'glab auth login' to continue",
		)
	}
	return err
}
