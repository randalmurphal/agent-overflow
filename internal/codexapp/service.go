// Package codexapp owns application-facing Codex controls and cached global
// reads. Thread send/rollback transactions, account switching, and session
// lifecycle remain with their existing owners.
package codexapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/codexusage"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/slicesx"
)

const backgroundTerminalStopTimeout = 10 * time.Second

type AccountSelection struct {
	ID    string
	Email string
}

// SessionLookup returns the Codex session for a thread and whether any live
// provider session exists. A nil session with active=true is a provider
// mismatch, distinct from a thread with no live session.
type SessionLookup func(threadID string) (session *codex.Session, active bool)

type Deps struct {
	Session          SessionLookup
	SessionForBinary func(binary string) *codex.Session
	AnySession       func() (session *codex.Session, accountID string)
	Binary           func() string
	CustomEnv        func() map[string]string
	ProbeEnv         func() map[string]string
	ProbeWorkDir     func() string
	LifeContext      func() context.Context
	ActiveAccount    func() AccountSelection
	SkillsCache      *codexskills.Cache
	UsageCache       *codexusage.Cache
}

type Service struct {
	deps        Deps
	skillsCache *codexskills.Cache
	usageCache  *codexusage.Cache
}

func New(deps Deps) *Service {
	skillsCache := deps.SkillsCache
	if skillsCache == nil {
		skillsCache = codexskills.New()
	}
	usageCache := deps.UsageCache
	if usageCache == nil {
		usageCache = codexusage.New()
	}
	return &Service{deps: deps, skillsCache: skillsCache, usageCache: usageCache}
}

func (s *Service) CleanBackgroundTerminals(threadID string) error {
	session, active := s.session(threadID)
	if !active {
		return fmt.Errorf("app: clean codex background terminals: no active session for thread %s", threadID)
	}
	if session == nil {
		return fmt.Errorf("app: clean codex background terminals: thread %s is not a Codex thread", threadID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalStopTimeout)
	defer cancel()
	return session.CleanBackgroundTerminals(ctx)
}

func (s *Service) TerminateBackgroundTerminal(threadID, processID string) (bool, error) {
	session, active := s.session(threadID)
	if !active {
		return false, fmt.Errorf("app: terminate codex background terminal: no active session for thread %s", threadID)
	}
	if session == nil {
		return false, fmt.Errorf("app: terminate codex background terminal: thread %s is not a Codex thread", threadID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalStopTimeout)
	defer cancel()
	return session.TerminateBackgroundTerminal(ctx, processID)
}

// StopSubagent interrupts the live child turn owned by launchID. Codex's
// typed ownership map prevents a caller-supplied id from targeting the root or
// an unrelated child. false, nil means the child was already terminal.
func (s *Service) StopSubagent(threadID, launchID string) (bool, error) {
	session, active := s.session(threadID)
	if !active {
		return false, fmt.Errorf("app: stop Codex subagent: no active session for thread %s", threadID)
	}
	if session == nil {
		return false, fmt.Errorf("app: stop Codex subagent: thread %s is not a Codex thread", threadID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTerminalStopTimeout)
	defer cancel()
	return session.InterruptSubagent(ctx, launchID)
}

func (s *Service) Skills(ctx context.Context, workspacePath string, forceReload bool) (codexskills.CwdSkills, error) {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return codexskills.CwdSkills{}, errors.New("codex skills: workspace path required")
	}
	if !filepath.IsAbs(workspacePath) {
		return codexskills.CwdSkills{}, fmt.Errorf("codex skills: workspace path %q must be absolute", workspacePath)
	}
	binary := s.binary()
	key := codexskills.Key(binary, workspacePath)
	fetch := func(ctx context.Context) (codexskills.CwdSkills, error) {
		return s.readSkills(ctx, binary, workspacePath, forceReload)
	}
	var (
		skills codexskills.CwdSkills
		err    error
	)
	if forceReload {
		skills, err = s.skillsCache.Refresh(ctx, key, fetch)
	} else {
		skills, err = s.skillsCache.Get(ctx, key, fetch)
	}
	if err != nil {
		return codexskills.CwdSkills{}, err
	}
	skills.Skills = slicesx.OrEmpty(skills.Skills)
	skills.Errors = slicesx.OrEmpty(skills.Errors)
	return skills, nil
}

func (s *Service) readSkills(ctx context.Context, binary, cwd string, forceReload bool) (codexskills.CwdSkills, error) {
	cwds := []string{cwd}
	if s.deps.SessionForBinary != nil {
		if session := s.deps.SessionForBinary(binary); session != nil {
			entries, err := session.ListSkills(ctx, cwds, forceReload)
			if err == nil {
				return skillsEntryForCwd(entries, cwd)
			}
			log.Printf("codex skills: live session read failed, falling back to a fresh process: %v", err)
		}
	}
	if strings.TrimSpace(binary) == "" {
		return codexskills.CwdSkills{}, errors.New("codex skills: codex binary not configured")
	}
	env := map[string]string(nil)
	if s.deps.CustomEnv != nil {
		env = s.deps.CustomEnv()
	}
	fetcher := &codex.SkillsFetcher{Binary: binary, WorkDir: cwd, Env: env}
	entries, err := fetcher.Fetch(ctx, cwds, forceReload)
	if err != nil {
		return codexskills.CwdSkills{}, err
	}
	return skillsEntryForCwd(entries, cwd)
}

func skillsEntryForCwd(entries []codexskills.CwdSkills, cwd string) (codexskills.CwdSkills, error) {
	for _, entry := range entries {
		if entry.Cwd == cwd {
			return entry, nil
		}
	}
	if len(entries) == 1 {
		return entries[0], nil
	}
	return codexskills.CwdSkills{}, fmt.Errorf(
		"codex skills: response carried %d entries, none matching %q", len(entries), cwd,
	)
}

// ResetSkills invalidates every cached cwd after Codex's payload-free
// skills/changed notification.
func (s *Service) ResetSkills() { s.skillsCache.Reset() }

type AccountUsageResult struct {
	Usage        codex.AccountUsage
	AccountEmail string
}

func (s *Service) AccountUsage() (*AccountUsageResult, error) {
	selection := AccountSelection{}
	if s.deps.ActiveAccount != nil {
		selection = s.deps.ActiveAccount()
	}
	binary := s.binary()
	key := binary + "\x00" + selection.ID
	ctx := context.Background()
	if s.deps.LifeContext != nil {
		ctx = s.deps.LifeContext()
	}
	usage, err := s.usageCache.Get(ctx, key, func(ctx context.Context) (codex.AccountUsage, error) {
		return s.readAccountUsage(ctx, binary, selection.ID)
	})
	if err != nil {
		if errors.Is(err, codex.ErrAccountUsageUnavailable) {
			log.Printf("codex account usage unavailable: %v", err)
			return nil, nil
		}
		return nil, err
	}
	if usage.Empty() {
		return nil, nil
	}
	return &AccountUsageResult{Usage: usage, AccountEmail: strings.TrimSpace(selection.Email)}, nil
}

func (s *Service) readAccountUsage(ctx context.Context, binary, accountID string) (codex.AccountUsage, error) {
	if s.deps.AnySession != nil {
		if session, sessionAccountID := s.deps.AnySession(); session != nil && sessionAccountID == accountID {
			usage, err := session.ReadAccountUsage(ctx)
			if err == nil || errors.Is(err, codex.ErrAccountUsageUnavailable) {
				return usage, err
			}
			log.Printf("codex account usage: live session read failed, falling back to a fresh process: %v", err)
		}
	}
	if strings.TrimSpace(binary) == "" {
		return codex.AccountUsage{}, errors.New("codex account usage: codex binary not configured")
	}
	workDir := ""
	if s.deps.ProbeWorkDir != nil {
		workDir = s.deps.ProbeWorkDir()
	}
	env := map[string]string(nil)
	if s.deps.ProbeEnv != nil {
		env = s.deps.ProbeEnv()
	}
	fetcher := &codex.AccountUsageFetcher{Binary: binary, WorkDir: workDir, Env: env}
	return fetcher.Fetch(ctx)
}

func (s *Service) session(threadID string) (*codex.Session, bool) {
	if s == nil || s.deps.Session == nil {
		return nil, false
	}
	return s.deps.Session(threadID)
}

func (s *Service) binary() string {
	if s == nil || s.deps.Binary == nil {
		return ""
	}
	return s.deps.Binary()
}
