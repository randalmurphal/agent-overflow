package highlightapp

import (
	"errors"
	"sync/atomic"
	"time"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/store"
)

// ContextRequest identifies the review source used to prime a patch parse.
type ContextRequest struct {
	Scope         string
	CommitSHA     string
	HeadSHA       string
	Path          string
	Patch         string
	EditPayloadID string
	EditTurnIndex int
}

// Config contains the App-owned boundaries used by highlighting coordination.
type Config struct {
	Store             *store.Store
	IsShuttingDown    func() bool
	ShutdownError     error
	ResolveContext    func(workspace, threadID string, req ContextRequest, maxBytes int64) (string, error)
	ReadWorkspaceFile func(path string, maxBytes int64) (string, error)
	EmitLiveCode      func(LiveCodeEvent)
	EmitDiffSeed      func(DiffSeedEvent)
	Now               func() time.Time
}

// Service owns every mutable highlight-app concern.
type Service struct {
	config      Config
	cache       *highlight.Cache
	live        liveHighlighter
	diffWorkers atomic.Int32
}

func New(config Config) *Service {
	if config.IsShuttingDown == nil {
		config.IsShuttingDown = func() bool { return false }
	}
	if config.ShutdownError == nil {
		config.ShutdownError = errors.New("highlight service is shutting down")
	}
	if config.EmitLiveCode == nil {
		config.EmitLiveCode = func(LiveCodeEvent) {}
	}
	if config.EmitDiffSeed == nil {
		config.EmitDiffSeed = func(DiffSeedEvent) {}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	s := &Service{config: config, cache: highlight.NewCache()}
	s.live.init()
	return s
}

func (s *Service) Code(langName, source string) (Result, error) {
	if s.config.IsShuttingDown() {
		return Result{}, s.config.ShutdownError
	}
	lang := highlight.LangFromName(langName)
	if len(source) > highlight.MaxRequestBytes {
		return Result{Lang: lang.String(), Truncated: true}, nil
	}
	res := s.cache.Code(lang, source)
	return Result{Lang: lang.String(), Lines: res.Lines, Truncated: res.Truncated, Incomplete: res.Incomplete}, nil
}

func (s *Service) Patch(path, patch string) (Result, error) {
	if s.config.IsShuttingDown() {
		return Result{}, s.config.ShutdownError
	}
	lang := highlight.LangFromPath(path)
	if len(patch) > highlight.MaxRequestBytes {
		return Result{Lang: lang.String(), Truncated: true}, nil
	}
	res := s.cache.Patch(lang, patch)
	return Result{Lang: lang.String(), Lines: res.Lines, Truncated: res.Truncated, Incomplete: res.Incomplete}, nil
}

// PatchWithContext primes a patch parse from file content the caller has
// already located. workspace is a RESOLVED checkout directory — this package
// never turns a thread id into one, because which directory a request means is
// a per-scope question the App answers before calling in. threadID is empty
// for the checkout scopes and carries the thread only for the edits scope,
// whose new side is that thread's own persisted snapshot.
func (s *Service) PatchWithContext(workspace, threadID string, req ContextRequest) (Result, error) {
	if s.config.IsShuttingDown() {
		return Result{}, s.config.ShutdownError
	}
	lang := highlight.LangFromPath(req.Path)
	if len(req.Patch) > highlight.MaxRequestBytes {
		return Result{Lang: lang.String(), Truncated: true}, nil
	}
	var content string
	var err error
	if s.config.ResolveContext != nil {
		content, err = s.config.ResolveContext(workspace, threadID, req, highlight.MaxPrimeBytes)
	}
	var res highlight.Result
	primed := err == nil && content != ""
	if primed {
		res = s.cache.PatchWithContext(lang, req.Patch, content)
	} else {
		res = s.cache.Patch(lang, req.Patch)
	}
	return Result{Lang: lang.String(), Lines: res.Lines, Truncated: res.Truncated, Incomplete: res.Incomplete, Primed: primed}, nil
}
