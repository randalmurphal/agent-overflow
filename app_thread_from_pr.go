package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// MaxInlinedPRDiffBytes caps the number of patch bytes we inline into the
// first user message on a PR-seeded thread. Oversized PRs (vendored dep bumps,
// generated lockfile churn) used to be inlined verbatim and cause SQLite row
// or frontend render explosions. Beyond this threshold we truncate and append
// a marker so the agent sees explicit evidence of the omission instead of a
// silently-cut patch.
const MaxInlinedPRDiffBytes = 256 * 1024

// PRReference identifies a GitHub pull request by owner/repo/number.
type PRReference struct {
	Owner  string
	Repo   string
	Number int
}

// OwnerRepo returns "owner/repo" for passing to gh subcommands.
func (r PRReference) OwnerRepo() string {
	return r.Owner + "/" + r.Repo
}

// prMetadata mirrors the fields we request from `gh pr view --json`.
type prMetadata struct {
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	HeadRefName string     `json:"headRefName"`
	BaseRefName string     `json:"baseRefName"`
	URL         string     `json:"url"`
	Files       []prFile   `json:"files"`
	Author      prAuthor   `json:"author"`
	State       string     `json:"state"`
	AdditionalJSON map[string]any `json:"-"`
}

type prFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type prAuthor struct {
	Login string `json:"login"`
}

var (
	// Matches:
	//   https://github.com/owner/repo/pull/123
	//   http://github.com/owner/repo/pull/123
	//   github.com/owner/repo/pull/123
	prURLPattern = regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/pull/(\d+)(?:[/?#].*)?$`)
	// Matches: owner/repo#123
	prShortPattern = regexp.MustCompile(`^([^/\s]+)/([^/\s#]+)#(\d+)$`)
)

// ParsePRReference accepts any of the supported PR input shapes. Whitespace is
// trimmed; otherwise the input is rejected with a structured error so the UI
// can show it verbatim.
func ParsePRReference(input string) (PRReference, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return PRReference{}, errors.New("PR reference is empty")
	}

	if m := prURLPattern.FindStringSubmatch(trimmed); m != nil {
		num, err := strconv.Atoi(m[3])
		if err != nil {
			return PRReference{}, fmt.Errorf("PR number %q is not an integer: %w", m[3], err)
		}
		if num <= 0 {
			return PRReference{}, fmt.Errorf("PR number must be positive, got %d", num)
		}
		return PRReference{Owner: m[1], Repo: m[2], Number: num}, nil
	}

	if m := prShortPattern.FindStringSubmatch(trimmed); m != nil {
		num, err := strconv.Atoi(m[3])
		if err != nil {
			return PRReference{}, fmt.Errorf("PR number %q is not an integer: %w", m[3], err)
		}
		if num <= 0 {
			return PRReference{}, fmt.Errorf("PR number must be positive, got %d", num)
		}
		return PRReference{Owner: m[1], Repo: m[2], Number: num}, nil
	}

	return PRReference{}, fmt.Errorf(
		"unrecognised PR reference %q: expected https://github.com/OWNER/REPO/pull/N or OWNER/REPO#N",
		trimmed,
	)
}

// CreateThreadFromPR creates a new thread seeded with a GitHub PR's metadata +
// diff as the first user message. Relies on the `gh` CLI being available on
// PATH; returns a structured error with installation hint if it isn't.
//
// Parameters:
//   - ownerRepo: OWNER/REPO pair (e.g. "agent-overflow/agent-overflow")
//   - number:    PR number
//   - providerName + model: provider + model for the new thread
//
// If the user has a local clone of the target repo registered in
// settings.RecentWorkspaces, that path is auto-selected as the workspace.
// Otherwise the caller is expected to pick a workspace; we still create the
// thread but WorkspacePath is left empty and the UI can prompt.
func (a *App) CreateThreadFromPR(
	ownerRepo string,
	number int,
	providerName string,
	model string,
) (store.Thread, error) {
	owner, repo, err := splitOwnerRepo(ownerRepo)
	if err != nil {
		return store.Thread{}, err
	}
	if number <= 0 {
		return store.Thread{}, fmt.Errorf("PR number must be positive, got %d", number)
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return store.Thread{}, errors.New("provider is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return store.Thread{}, errors.New("model is required")
	}

	if err := ensureGhAvailable(); err != nil {
		return store.Thread{}, err
	}

	ref := PRReference{Owner: owner, Repo: repo, Number: number}
	meta, err := fetchPRMetadata(ref)
	if err != nil {
		return store.Thread{}, err
	}
	diff, err := fetchPRDiff(ref)
	if err != nil {
		return store.Thread{}, err
	}

	workspace := a.resolveRepoWorkspace(ref)
	projectPath := a.detectProjectPath(workspace)
	now := time.Now().UnixMilli()

	title := fmt.Sprintf("PR #%d: %s", number, strings.TrimSpace(meta.Title))
	if len(title) > 120 {
		title = title[:117] + "..."
	}

	thread := store.Thread{
		ID:              uuid.NewString(),
		Title:           title,
		Provider:        providerName,
		WorkspacePath:   workspace,
		Model:           model,
		ProjectPath:     projectPath,
		InteractionMode: "default",
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := a.store.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("create thread from PR: %w", err)
	}
	if a.settings != nil && workspace != "" {
		a.settings.AddRecentWorkspace(workspace)
	}

	userContent := buildPRUserMessage(ref, meta, diff)
	userItem := store.Item{
		ID:        uuid.NewString(),
		ThreadID:  thread.ID,
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "user",
		Summary:   userContent,
		CreatedAt: now,
	}
	if err := a.store.InsertItem(userItem); err != nil {
		// Roll back the thread so we don't leave a half-constructed row.
		_ = a.store.DeleteThread(thread.ID)
		return store.Thread{}, fmt.Errorf("create thread from PR: persist first item: %w", err)
	}

	refreshed, err := a.store.GetThread(thread.ID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("create thread from PR: reload thread: %w", err)
	}
	return refreshed, nil
}

// splitOwnerRepo parses "owner/repo" into its parts, rejecting malformed input.
func splitOwnerRepo(ownerRepo string) (string, string, error) {
	ownerRepo = strings.TrimSpace(ownerRepo)
	parts := strings.Split(ownerRepo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("ownerRepo must be in the form OWNER/REPO, got %q", ownerRepo)
	}
	return parts[0], parts[1], nil
}

// ensureGhAvailable surfaces a helpful error when `gh` is not installed rather
// than letting the user see a generic exec failure.
func ensureGhAvailable() error {
	if _, err := lookPath("gh"); err != nil {
		return fmt.Errorf(
			"GitHub CLI (gh) is not on PATH. Install it from https://cli.github.com and run 'gh auth login' to continue: %w",
			err,
		)
	}
	return nil
}

// lookPath is a package-level function reference so tests can inject a fake gh
// without depending on shell PATH lookup order.
var lookPath = exec.LookPath

// ghCommand is a package-level function reference that returns a *exec.Cmd for
// the given gh subcommand + args. Tests override this to capture the invocation
// and produce canned output.
var ghCommand = func(args ...string) *exec.Cmd {
	return exec.Command("gh", args...)
}

func fetchPRMetadata(ref PRReference) (prMetadata, error) {
	cmd := ghCommand(
		"pr", "view",
		"--repo", ref.OwnerRepo(),
		strconv.Itoa(ref.Number),
		"--json", "title,body,headRefName,baseRefName,files,url,author,state",
	)
	out, err := cmd.Output()
	if err != nil {
		return prMetadata{}, wrapGhError("gh pr view", err)
	}
	var meta prMetadata
	if err := json.Unmarshal(out, &meta); err != nil {
		return prMetadata{}, fmt.Errorf("gh pr view returned malformed JSON: %w", err)
	}
	return meta, nil
}

func fetchPRDiff(ref PRReference) (string, error) {
	cmd := ghCommand(
		"pr", "diff",
		"--repo", ref.OwnerRepo(),
		strconv.Itoa(ref.Number),
	)
	out, err := cmd.Output()
	if err != nil {
		return "", wrapGhError("gh pr diff", err)
	}
	return string(out), nil
}

// wrapGhError preserves stderr output from gh so the user sees actionable
// messages (e.g. "could not resolve to a Repository with the name").
func wrapGhError(subcommand string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%s failed: %s", subcommand, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s failed: %w", subcommand, err)
}

// resolveRepoWorkspace looks for a local clone of OWNER/REPO in the user's
// recent workspaces. Returns the first matching path, or "" if nothing matches.
// We match on the basename (`/.../repo`) to survive users putting checkouts in
// non-canonical locations.
func (a *App) resolveRepoWorkspace(ref PRReference) string {
	if a.settings == nil {
		return ""
	}
	suffix := "/" + ref.Repo
	for _, ws := range a.settings.Get().RecentWorkspaces {
		ws = strings.TrimSpace(strings.TrimRight(ws, "/"))
		if ws == "" {
			continue
		}
		if strings.HasSuffix(ws, suffix) || strings.HasSuffix(ws, "/"+ref.Owner+"/"+ref.Repo) {
			return ws
		}
	}
	return ""
}

// buildPRUserMessage composes the first user message we persist on the new
// thread. Keeps the PR title + author + bodies compact, then dumps the patch
// into a fenced code block so providers can reason about the actual changes.
func buildPRUserMessage(ref PRReference, meta prMetadata, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# PR #%d: %s\n\n", ref.Number, strings.TrimSpace(meta.Title))
	if meta.URL != "" {
		fmt.Fprintf(&b, "Link: %s\n", meta.URL)
	}
	if meta.Author.Login != "" {
		fmt.Fprintf(&b, "Author: @%s\n", meta.Author.Login)
	}
	if meta.BaseRefName != "" || meta.HeadRefName != "" {
		fmt.Fprintf(&b, "Branches: %s → %s\n", meta.HeadRefName, meta.BaseRefName)
	}
	if len(meta.Files) > 0 {
		fmt.Fprintf(&b, "Files changed: %d\n", len(meta.Files))
	}
	b.WriteString("\n")
	body := strings.TrimSpace(meta.Body)
	if body != "" {
		b.WriteString("## Description\n\n")
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	b.WriteString("## Patch\n\n```diff\n")
	b.WriteString(strings.TrimRight(truncatePRDiff(diff), "\n"))
	b.WriteString("\n```\n")
	return b.String()
}

// truncatePRDiff clips diff output at MaxInlinedPRDiffBytes and appends a
// clear marker recording how many bytes were dropped. Shorter inputs are
// returned unchanged.
func truncatePRDiff(diff string) string {
	if len(diff) <= MaxInlinedPRDiffBytes {
		return diff
	}
	omitted := len(diff) - MaxInlinedPRDiffBytes
	return fmt.Sprintf(
		"%s\n\n<!-- diff truncated at %d KB; %d bytes omitted -->",
		diff[:MaxInlinedPRDiffBytes],
		MaxInlinedPRDiffBytes/1024,
		omitted,
	)
}
