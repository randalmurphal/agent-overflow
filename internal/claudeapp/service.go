// Package claudeapp owns application-facing reads and controls for a live
// Claude session. Account probing, credential rotation, and session lifecycle
// remain with their respective owners.
package claudeapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/slicesx"
)

const (
	contextUsageTimeout = 15 * time.Second
	taskControlTimeout  = 10 * time.Second
)

// SessionLookup returns the Claude session for a thread and whether any live
// provider session exists. A nil session with active=true is a provider
// mismatch, distinct from a thread with no live session.
type SessionLookup func(threadID string) (session *claude.Session, active bool)

type Deps struct {
	Session     SessionLookup
	ConfigStore func() (*claudeconfig.Store, error)
}

type Service struct {
	deps Deps
}

func New(deps Deps) *Service { return &Service{deps: deps} }

type ContextUsageResult struct {
	Usage  *claude.ContextUsage
	Reason string
}

func (s *Service) GetContextUsage(threadID string) (ContextUsageResult, error) {
	session, active := s.session(threadID)
	if !active {
		return ContextUsageResult{Reason: "The exact breakdown needs a running Claude session. Start the thread to read it."}, nil
	}
	if session == nil {
		return ContextUsageResult{Reason: "The exact breakdown is only available on Claude threads."}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), contextUsageTimeout)
	defer cancel()
	usage, err := session.GetContextUsage(ctx)
	if err != nil {
		return ContextUsageResult{}, err
	}
	return ContextUsageResult{Usage: usage}, nil
}

func (s *Service) StopTask(threadID, taskID string) error {
	session, active := s.session(threadID)
	if !active {
		return fmt.Errorf("app: stop claude task: no active session for thread %s", threadID)
	}
	if session == nil {
		return fmt.Errorf("app: stop claude task: thread %s is not a Claude thread", threadID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskControlTimeout)
	defer cancel()
	return session.StopTask(ctx, taskID)
}

func (s *Service) BackgroundTask(threadID, toolUseID string) error {
	session, active := s.session(threadID)
	if !active {
		return fmt.Errorf("app: background claude task: no active session for thread %s", threadID)
	}
	if session == nil {
		return fmt.Errorf("app: background claude task: thread %s is not a Claude thread", threadID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskControlTimeout)
	defer cancel()
	return session.BackgroundTask(ctx, toolUseID)
}

func (s *Service) Skills(workspacePath string) ([]claudeconfig.Skill, error) {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return nil, errors.New("claude skills: workspace path required")
	}
	if !filepath.IsAbs(workspacePath) {
		return nil, fmt.Errorf("claude skills: workspace path %q must be absolute", workspacePath)
	}
	if s == nil || s.deps.ConfigStore == nil {
		return nil, errors.New("claude skills: config store unavailable")
	}
	store, err := s.deps.ConfigStore()
	if err != nil {
		return nil, err
	}
	skills, err := store.ListSkills(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("claude skills: %w", err)
	}
	return slicesx.OrEmpty(skills), nil
}

func (s *Service) session(threadID string) (*claude.Session, bool) {
	if s == nil || s.deps.Session == nil {
		return nil, false
	}
	return s.deps.Session(threadID)
}
