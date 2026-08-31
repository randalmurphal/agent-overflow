package threadapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/prthread"
	"agent-overflow/internal/store"
)

// PullRequestPort is the explicit forge/project boundary for PR-seeded thread
// creation. Root owns CLI execution and repository identity resolution.
type PullRequestPort interface {
	ResolveWorkspace(ref gitops.PRReference) string
	Load(workspace string, ref gitops.PRReference) (gitops.PRMetadata, string, error)
	EnsureProject(workspaceOrAnchor string) (store.Project, error)
}

// PullRequestOptions is the request half of CreateFromPR. It replaces what
// was a five-string positional list, so adding CreatedByDevice names the value
// at the call site instead of adding a sixth same-typed slot to miscount.
type PullRequestOptions struct {
	Project  string
	Number   int
	Provider string
	Model    string
	Forge    string
	// CreatedByDevice names the screen this call came from, or "" when the
	// backend created the thread on its own behalf.
	CreatedByDevice string
}

func (s *Service) CreateFromPR(opts PullRequestOptions, port PullRequestPort) (store.Thread, error) {
	project, number, model := opts.Project, opts.Number, opts.Model
	providerName := opts.Provider
	database, err := s.database("create thread from pull request")
	if err != nil {
		return store.Thread{}, err
	}
	models, err := s.modelPolicy("create thread from pull request")
	if err != nil {
		return store.Thread{}, err
	}
	forgeID := strings.TrimSpace(opts.Forge)
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
	if port == nil {
		return store.Thread{}, fmt.Errorf("create thread from %s: pull request source unavailable", prthread.ForgeNounLong(forgeID))
	}
	model = strings.TrimSpace(model)
	seed := models.Seed(providerName, model)
	if model == "" {
		model = seed.Model
	}
	ref := gitops.PRReference{Forge: forgeID, Namespace: namespace, Repo: repo, Number: number}
	refJSON, err := json.Marshal(ref)
	if err != nil {
		return store.Thread{}, fmt.Errorf("marshal %s reference: %w", prthread.ForgeNounLong(forgeID), err)
	}
	workspace := port.ResolveWorkspace(ref)
	metadata, diff, err := port.Load(workspace, ref)
	if err != nil {
		return store.Thread{}, err
	}
	projectAnchor := workspace
	if strings.TrimSpace(projectAnchor) == "" {
		projectAnchor = gitops.BuildPRAnchor(forgeID, namespace, repo)
	}
	projectRow, err := port.EnsureProject(projectAnchor)
	if err != nil {
		return store.Thread{}, err
	}
	now := s.deps.Now().UnixMilli()
	thread := store.Thread{
		ID:              s.newID(),
		ProjectID:       projectRow.ID,
		ProjectPath:     projectRow.Path,
		Title:           prthread.TruncateTitle(prthread.FormatTitle(forgeID, number, metadata.Title)),
		Provider:        providerName,
		WorkspacePath:   workspace,
		PRRef:           string(refJSON),
		Model:           model,
		Mode:            "chat",
		ReasoningEffort: seed.ReasoningEffort,
		FastMode:        seed.FastMode,
		ContextWindow:   seed.ContextWindow,
		RuntimeMode:     seed.RuntimeMode,
		CreatedAt:       now,
		UpdatedAt:       now,
		CreatedByDevice: opts.CreatedByDevice,
		// A PR thread with no local clone has no workspace, so nothing to
		// observe; observeOrigin reports that as unknown rather than guessing
		// from the PR's own head, which names a branch on the forge that this
		// machine may never have fetched.
		Origin: s.observeOrigin(workspace),
	}
	if err := database.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("create thread from %s: %w", prthread.ForgeNounLong(forgeID), err)
	}
	models.Remember(thread)
	if s.deps.RecentWorkspaces != nil && workspace != "" {
		s.deps.RecentWorkspaces.AddRecentWorkspace(workspace)
	}
	item := store.Item{
		ID:        s.newID(),
		ThreadID:  thread.ID,
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   prthread.BuildUserMessage(ref, metadata, diff),
		CreatedAt: now,
	}
	if err := database.InsertItem(item); err != nil {
		_ = database.DeleteThread(thread.ID)
		return store.Thread{}, fmt.Errorf("create thread from %s: persist first item: %w", prthread.ForgeNounLong(forgeID), err)
	}
	refreshed, err := database.GetThread(thread.ID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("create thread from %s: reload thread: %w", prthread.ForgeNounLong(forgeID), err)
	}
	return refreshed, nil
}
