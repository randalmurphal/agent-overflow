package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	gitops "agent-overflow/internal/git"
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

// CreateThreadFromPR creates a new thread seeded with a PR/MR's metadata +
// diff as the first user message. Routes through the appropriate forge CLI
// (`gh` for GitHub, `glab` for GitLab) detected from the `forge` parameter.
//
// Parameters:
//   - project:       "owner/repo" for GitHub, "namespace/.../repo" for GitLab
//   - number:        PR / MR number
//   - providerName + model: provider + model for the new thread
//   - forge:         "github" (default for empty) or "gitlab"
//
// If the user has a local clone of the target repo registered in
// settings.RecentWorkspaces, that path is auto-selected as the workspace.
// Otherwise the caller is expected to pick a workspace; we still create the
// thread but WorkspacePath is left empty and the UI can prompt.
func (a *App) CreateThreadFromPR(
	project string,
	number int,
	providerName string,
	model string,
	forge string,
) (store.Thread, error) {
	forgeID := strings.TrimSpace(forge)
	if forgeID == "" {
		forgeID = "github"
	}
	if forgeID != "github" && forgeID != "gitlab" {
		return store.Thread{}, fmt.Errorf("unsupported forge %q (expected github or gitlab)", forgeID)
	}

	namespace, repo, err := gitops.SplitProjectForForge(forgeID, project)
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

	ref := gitops.PRReference{
		Forge:     forgeID,
		Namespace: namespace,
		Repo:      repo,
		Number:    number,
	}
	forgeImpl := a.gitCore().ForgeByID(forgeID)
	// The view + diff calls don't need a local clone — gh --repo and
	// glab -R both query authenticated state directly. Pass the
	// resolved workspace as cwd when available so any forge CLI that
	// reads local config (e.g. glab's project resolution) finds it.
	workspace := a.resolveRepoWorkspace(ref)
	meta, err := forgeImpl.ViewPR(workspace, ref.Project(), ref.Number)
	if err != nil {
		return store.Thread{}, err
	}
	diff, err := forgeImpl.Diff(workspace, ref.Project(), ref.Number)
	if err != nil {
		return store.Thread{}, err
	}

	// When the user has no local clone, fall back to a forge-prefixed
	// pseudo anchor derived from the ref so every thread still belongs
	// to a project row. The frontend can later prompt the user to pick
	// a real workspace.
	projectAnchor := workspace
	if strings.TrimSpace(projectAnchor) == "" {
		projectAnchor = gitops.BuildPRAnchor(forgeID, namespace, repo)
	}
	projectRow, err := a.ensureProjectForWorkspace(projectAnchor)
	if err != nil {
		return store.Thread{}, err
	}
	now := time.Now().UnixMilli()

	title := truncatePRTitle(formatPRThreadTitle(forgeID, number, meta.Title))

	thread := store.Thread{
		ID:            uuid.NewString(),
		ProjectID:     projectRow.ID,
		ProjectPath:   projectRow.Path,
		Title:         title,
		Provider:      providerName,
		WorkspacePath: workspace,
		Model:         model,
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
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
		Kind:      "user_text",
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

// formatPRThreadTitle renders the sidebar title prefix per forge:
// "PR #N" for GitHub, "MR !N" for GitLab — matching each forge's
// native conventions for referencing change requests.
func formatPRThreadTitle(forgeID string, number int, prTitle string) string {
	prTitle = strings.TrimSpace(prTitle)
	if forgeID == "gitlab" {
		return fmt.Sprintf("MR !%d: %s", number, prTitle)
	}
	return fmt.Sprintf("PR #%d: %s", number, prTitle)
}

// resolveRepoWorkspace looks for a local clone of the PR/MR's repo in
// the user's recent workspaces. Returns the first matching path, or ""
// if nothing matches. Match is on basename (`/.../repo`) to survive
// users putting checkouts in non-canonical locations.
func (a *App) resolveRepoWorkspace(ref gitops.PRReference) string {
	if a.settings == nil {
		return ""
	}
	suffix := "/" + ref.Repo
	fullSuffix := "/" + ref.Project()
	for _, ws := range a.settings.Get().RecentWorkspaces {
		ws = strings.TrimSpace(strings.TrimRight(ws, "/"))
		if ws == "" {
			continue
		}
		if strings.HasSuffix(ws, suffix) || strings.HasSuffix(ws, fullSuffix) {
			return ws
		}
	}
	return ""
}

// buildPRUserMessage composes the first user message we persist on the
// new thread. Keeps the PR title + author + bodies compact, then dumps
// the patch into a fenced code block so providers can reason about the
// actual changes.
func buildPRUserMessage(ref gitops.PRReference, meta gitops.PRMetadata, diff string) string {
	var b strings.Builder
	header := "PR"
	numberSigil := "#"
	if ref.Forge == "gitlab" {
		header = "MR"
		numberSigil = "!"
	}
	fmt.Fprintf(&b, "# %s %s%d: %s\n\n", header, numberSigil, ref.Number, strings.TrimSpace(meta.Title))
	if meta.URL != "" {
		fmt.Fprintf(&b, "Link: %s\n", meta.URL)
	}
	if meta.AuthorLogin != "" {
		fmt.Fprintf(&b, "Author: @%s\n", meta.AuthorLogin)
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
	truncated := strings.TrimRight(truncatePRDiff(diff), "\n")
	fence := fenceForContent(truncated)
	b.WriteString("## Patch\n\n")
	b.WriteString(fence)
	b.WriteString("diff\n")
	b.WriteString(truncated)
	b.WriteString("\n")
	b.WriteString(fence)
	b.WriteString("\n")
	return b.String()
}

// fenceForContent returns a backtick fence long enough to avoid colliding
// with any backtick run inside content. Standard markdown requires the
// closing fence to be at least as long as the opening one, and a content
// run that matches the fence will close it prematurely. We pick a fence
// strictly longer than the longest run we find (minimum 3 = standard
// triple-backtick) so the diff survives verbatim.
func fenceForContent(content string) string {
	longest := 0
	run := 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	size := longest + 1
	if size < 3 {
		size = 3
	}
	return strings.Repeat("`", size)
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

// maxPRTitleRunes caps thread titles at 120 user-perceived characters (runes).
// The SQLite column is wide, but the sidebar row truncates anything that
// doesn't fit — a 120-rune ceiling keeps the title readable while leaving
// room for the "PR #N: " prefix in the common case.
const maxPRTitleRunes = 120

// truncatePRTitle shortens a thread title to at most maxPRTitleRunes runes,
// appending an ellipsis marker. Crucially, it truncates on rune boundaries
// so multibyte codepoints (CJK, combining marks, emoji) don't end up split
// into an invalid UTF-8 sequence.
func truncatePRTitle(title string) string {
	if utf8.RuneCountInString(title) <= maxPRTitleRunes {
		return title
	}

	const suffix = "..."
	keep := maxPRTitleRunes - utf8.RuneCountInString(suffix)
	if keep < 1 {
		keep = 1
	}

	count := 0
	end := 0
	for i := range title {
		if count == keep {
			end = i
			break
		}
		count++
	}
	if count < keep {
		// The whole string fit inside keep runes — shouldn't happen given
		// the length check above, but be defensive.
		return title
	}
	return title[:end] + suffix
}
