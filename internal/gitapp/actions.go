package gitapp

import (
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/prthread"
)

// Status returns a fresh thread-workspace status and schedules the same refresh
// through the shared workspace stream.
func (s *Service) Status(threadID string) (gitops.GitStatus, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return gitops.GitStatus{}, err
	}
	_, workspace, err := s.ResolveThreadPaths(thread)
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

func (s *Service) StatusFastForProject(projectID string) (gitops.GitStatus, error) {
	project, err := s.ProjectPath(projectID)
	if err != nil {
		return gitops.GitStatus{}, err
	}
	return s.core.StatusFast(project)
}

func (s *Service) WorkingTreeDiff(threadID string) (string, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	_, workspace, err := s.ResolveThreadPaths(thread)
	if err != nil {
		return "", err
	}
	return s.core.WorkingTreeDiff(workspace)
}

func (s *Service) ListBranches(threadID string) ([]gitops.GitBranch, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}
	project, _, err := s.ResolveThreadPaths(thread)
	if err != nil {
		return nil, err
	}
	return s.core.ListBranches(project)
}

func (s *Service) ListBranchesForProject(projectID string) ([]gitops.GitBranch, error) {
	project, err := s.ProjectPath(projectID)
	if err != nil {
		return nil, err
	}
	return s.core.ListBranches(project)
}

func (s *Service) MaybeFetchRemotes(threadID string) (bool, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return false, err
	}
	project, _, err := s.ResolveThreadPaths(thread)
	if err != nil {
		return false, err
	}
	return s.core.MaybeFetchRemotes(project)
}

func (s *Service) MaybeFetchRemotesForProject(projectID string) (bool, error) {
	project, err := s.ProjectPath(projectID)
	if err != nil {
		return false, err
	}
	return s.core.MaybeFetchRemotes(project)
}

func (s *Service) Commit(threadID, subject, body string) (gitops.GitActionResult, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	_, workspace, err := s.ResolveThreadPaths(thread)
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

func (s *Service) StageAll(threadID string) error {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return err
	}
	_, workspace, err := s.ResolveThreadPaths(thread)
	if err != nil {
		return err
	}
	return s.core.StageAll(workspace)
}

func (s *Service) Push(threadID string) (gitops.GitActionResult, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	_, workspace, err := s.ResolveThreadPaths(thread)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	if err := s.core.Push(workspace); err != nil {
		return gitops.GitActionResult{}, err
	}
	return gitops.GitActionResult{Action: "push", Branch: s.core.CurrentBranch(workspace), Message: "Pushed branch"}, nil
}

func (s *Service) Pull(threadID string) (gitops.GitActionResult, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	_, workspace, err := s.ResolveThreadPaths(thread)
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

func (s *Service) CreateBranch(threadID, name string) error {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return err
	}
	project, _, err := s.ResolveThreadPaths(thread)
	if err != nil {
		return err
	}
	return s.core.CreateBranch(project, name)
}

func (s *Service) CreatePR(threadID, title, body string, draft bool) (gitops.GitActionResult, error) {
	thread, err := s.store.GetThread(threadID)
	if err != nil {
		return gitops.GitActionResult{}, err
	}
	_, workspace, err := s.ResolveThreadPaths(thread)
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
