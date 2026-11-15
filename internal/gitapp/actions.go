package gitapp

import (
	"context"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/prthread"
)

// Status reads the referenced workspace's status, asking the forge for its
// open PR, and publishes it through the workspace's status stream
// (publishStatus). ctx bounds the open-PR lookup.
func (s *Service) Status(ctx context.Context, ref WorkspaceRef) (gitops.GitStatus, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitStatus{}, err
	}
	s.core.InvalidatePRCache(workspace)
	return s.publishStatus(workspace, func() (gitops.GitStatus, error) { return s.core.Status(ctx, workspace) })
}

// CheckoutStatus reads the status of workspace, a resolved checkout, after a
// branch change, from the PR cache instead of the forge, and publishes it
// through the workspace's status stream (publishStatus), whose refresh asks
// the forge. It returns the published status, which is this read or one
// that began after it, or this read itself while a Suppress hold keeps it
// off the stream.
func (s *Service) CheckoutStatus(workspace string) (gitops.GitStatus, error) {
	return s.publishStatus(workspace, func() (gitops.GitStatus, error) { return s.core.StatusFast(workspace) })
}

// publishStatus is every status this service hands a client for a
// workspace: gitwatch.Manager.Publish orders it against the workspace's
// stream and returns what the stream publishes, or the read itself when
// nothing could publish it (nobody watches, or a Suppress hold). With no
// manager there is no stream to order against, and the status carries no
// Seq.
func (s *Service) publishStatus(workspace string, read func() (gitops.GitStatus, error)) (gitops.GitStatus, error) {
	if s.watch == nil {
		return read()
	}
	return s.watch.Publish(workspace, read)
}

func (s *Service) ListBranches(ref WorkspaceRef) ([]gitops.GitBranch, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return nil, err
	}
	return s.core.ListBranches(project)
}

// ListTags lists every tag of the workspace's repository, newest first.
func (s *Service) ListTags(ref WorkspaceRef) ([]gitops.GitTag, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return nil, err
	}
	return s.core.ListTags(project)
}

// RefreshTags force-fetches origin's tags into the workspace's repository,
// pruning none, and returns the fresh list.
func (s *Service) RefreshTags(ref WorkspaceRef) ([]gitops.GitTag, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return nil, err
	}
	if err := s.core.RefreshTags(project); err != nil {
		return nil, err
	}
	return s.core.ListTags(project)
}

func (s *Service) MaybeFetchRemotes(ref WorkspaceRef) (bool, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return false, err
	}
	return s.core.MaybeFetchRemotes(project)
}

// Commit records what the selection covers in the referenced workspace.
func (s *Service) Commit(ref WorkspaceRef, subject, body string, selection gitops.CommitSelection) (gitops.GitActionResult, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	sha, err := s.core.CommitSelected(workspace, subject, body, selection)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	return gitops.GitActionResult{Action: "commit", Branch: s.core.CurrentBranch(workspace), Commit: sha, Message: "Committed changes"}, nil
}

func (s *Service) Push(ref WorkspaceRef) (gitops.GitActionResult, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	if err := s.core.Push(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}
	return gitops.GitActionResult{Action: "push", Branch: s.core.CurrentBranch(workspace), Message: "Pushed branch"}, nil
}

func (s *Service) Pull(ref WorkspaceRef) (gitops.GitActionResult, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	if err := s.core.Pull(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}
	if s.invalidateWorkspace != nil {
		s.invalidateWorkspace(workspace)
	}
	return gitops.GitActionResult{Action: "pull", Branch: s.core.CurrentBranch(workspace), Message: "Pulled latest changes"}, nil
}

func (s *Service) CreatePR(ctx context.Context, ref WorkspaceRef, title, body string, draft bool) (gitops.GitActionResult, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	// Creating pushes and opens the PR, so from here the caller's
	// cancellation no longer applies; the lookup below runs to its own bound.
	ctx = context.WithoutCancel(ctx)
	url, err := s.core.CreatePR(ctx, workspace, title, body, "", draft)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	// Look the new PR up now: the badge shows it without waiting out the
	// cached "no PR", and the merged-PR cleanup tracks it from that answer.
	s.RefreshPR(ctx, workspace)
	return gitops.GitActionResult{
		Action:  "pr",
		Branch:  s.core.CurrentBranch(workspace),
		PRURL:   url,
		Message: "Created " + prthread.ForgeNounLong(s.core.DetectForge(workspace)),
	}, nil
}
