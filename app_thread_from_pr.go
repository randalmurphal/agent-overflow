package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/prthread"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

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
		return store.Thread{}, fmt.Errorf("%s number must be positive, got %d", prthread.ForgeNoun(forgeID), number)
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return store.Thread{}, errors.New("provider is required")
	}
	model = strings.TrimSpace(model)
	seed := a.seedChatModelProfile(providerName, model)
	if model == "" {
		model = seed.Model
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

	title := prthread.TruncateTitle(prthread.FormatTitle(forgeID, number, meta.Title))

	thread := store.Thread{
		ID:              uuid.NewString(),
		ProjectID:       projectRow.ID,
		ProjectPath:     projectRow.Path,
		Title:           title,
		Provider:        providerName,
		WorkspacePath:   workspace,
		Model:           model,
		Mode:            "chat",
		ReasoningEffort: seed.ReasoningEffort,
		FastMode:        seed.FastMode,
		ContextWindow:   seed.ContextWindow,
		RuntimeMode:     seed.RuntimeMode,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := a.store.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("create thread from %s: %w", prthread.ForgeNounLong(forgeID), err)
	}
	a.rememberChatModelProfile(thread)
	if a.settings != nil && workspace != "" {
		a.settings.AddRecentWorkspace(workspace)
	}

	userContent := prthread.BuildUserMessage(ref, meta, diff)
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
		return store.Thread{}, fmt.Errorf("create thread from %s: persist first item: %w", prthread.ForgeNounLong(forgeID), err)
	}

	refreshed, err := a.store.GetThread(thread.ID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("create thread from %s: reload thread: %w", prthread.ForgeNounLong(forgeID), err)
	}
	return refreshed, nil
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
