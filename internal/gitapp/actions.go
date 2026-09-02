package gitapp

import (
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/prthread"
)

// Status returns a fresh status for the referenced workspace and schedules the
// same refresh through the shared workspace stream.
func (s *Service) Status(ref WorkspaceRef) (gitops.GitStatus, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitStatus{}, err
	}
	s.core.InvalidatePRCache(workspace)
	status, err := s.core.Status(workspace)
	if err != nil {
		return gitops.GitStatus{}, err
	}
	s.RequestRefresh(workspace)
	return status, nil
}

func (s *Service) ListBranches(ref WorkspaceRef) ([]gitops.GitBranch, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return nil, err
	}
	return s.core.ListBranches(project)
}

func (s *Service) MaybeFetchRemotes(ref WorkspaceRef) (bool, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return false, err
	}
	return s.core.MaybeFetchRemotes(project)
}

func (s *Service) Commit(ref WorkspaceRef, subject, body string) (gitops.GitActionResult, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	if err := s.core.StageAll(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}
	sha, err := s.core.Commit(workspace, subject, body)
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

func (s *Service) CreatePR(ref WorkspaceRef, title, body string, draft bool) (gitops.GitActionResult, error) {
	_, workspace, err := s.ResolveWorkspace(ref)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	url, err := s.core.CreatePR(workspace, title, body, "", draft)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	s.core.InvalidatePRCache(workspace)
	return gitops.GitActionResult{
		Action:  "pr",
		Branch:  s.core.CurrentBranch(workspace),
		PRURL:   url,
		Message: "Created " + prthread.ForgeNounLong(s.core.DetectForge(workspace)),
	}, nil
}
