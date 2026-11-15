package providerupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// The providers this service checks, in the order every []Status lists them.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

var providerOrder = []string{ProviderClaude, ProviderCodex}

const (
	checkInterval = time.Hour
	// checkTimeout bounds one provider's lookup: `claude doctor` plus one
	// small HTTP answer.
	checkTimeout = time.Minute
)

// Binary is one provider installation's executables.
type Binary struct {
	// Path is the binary sessions spawn, as configured or resolved.
	Path string
	// Launcher is the npm launcher that starts Path, "" when none. Codex's
	// own updater learns its install method from the launcher.
	Launcher string
	// ResolvedPath is the file behind Path, symlinks followed, "" when it
	// does not resolve. Its location names the install's updater.
	ResolvedPath string
}

// Command is one provider CLI invocation.
type Command struct {
	Binary string
	Args   []string
	// Env overrides the inherited environment.
	Env map[string]string
	Dir string
}

// Output is a finished command's exit status and the end of its stdout and
// stderr, interleaved as written.
type Output struct {
	Text     string
	ExitCode int
}

// Runner runs a command and everything it starts to completion. A non-zero
// exit is an Output, not an error, and ExitCode is negative for a command
// ended by a signal. An error means the command could not run, or ctx ended
// it (ctx's error), or wraps ErrCommandSurvived when ending it failed.
type Runner func(ctx context.Context, command Command) (Output, error)

// ErrCommandSurvived is a Runner's error for a command ctx ended that could
// not be stopped and is still running.
var ErrCommandSurvived = errors.New("the command could not be stopped and is still running")

// Deps are the capabilities the app supplies. Every field is required.
type Deps struct {
	// Binary resolves a provider's executables at call time.
	Binary func(providerName string) Binary
	// Installed is the version token of the binary sessions spawn, "" when
	// it cannot be read.
	Installed func(providerName string) string
	// Env is the environment overrides for the provider's commands.
	Env func(providerName string) map[string]string
	// WorkDir is the directory commands run in.
	WorkDir func() (string, error)
	Runner  Runner
	// AfterRun runs once an update command exits, before the installed
	// version is read again.
	AfterRun func()
	// Client is the HTTP client for the release hosts, resolved per request.
	Client func() *http.Client
	Emit   func([]Status)
	Now    func() time.Time
	// Ticks replaces the hourly ticker when non-nil.
	Ticks <-chan time.Time
}

// Refusals of Run and Notes.
var (
	ErrRunning      = errors.New("an update of this provider is already running")
	ErrNoUpdate     = errors.New("no newer version of this provider is known")
	ErrShuttingDown = errors.New("the app is shutting down")
	// ErrUpdaterSurvived refuses every later run of a provider whose update
	// command outlived its run, for the life of the process.
	ErrUpdaterSurvived = errors.New("a previous update command could not be stopped and may still be running")
)

// Service owns provider CLI update checks, release notes and update runs.
// Its state is in memory only: what the last check proved and the last run.
type Service struct {
	deps Deps
	// life ends at Stop; every lookup, notes fetch and run derives from it.
	life context.Context
	stop context.CancelFunc

	// publish orders frames: a state change and its emission happen under
	// it, so frames leave in mutation order. Taken before mu.
	publish sync.Mutex
	mu      sync.Mutex
	// providers is fixed at New; the states it points to are guarded by mu.
	providers map[string]*providerState
	started   bool
	stopped   bool
	// work joins the loop, checks, notes lookups and update runs. Members
	// join through admitLocked.
	work sync.WaitGroup
}

type providerState struct {
	// checkMu serializes the provider's checks, each from its first read
	// through its published result.
	checkMu sync.Mutex
	// notesMu serializes the provider's notes lookups across the cache read,
	// the fetch and the cache write.
	notesMu sync.Mutex
	// status, survived and notes are guarded by Service.mu.
	status Status
	// survived is set once an update command outlived its run.
	survived bool
	notes    *notesCache
}

// checked is one finished check of a provider. A run executes the update of
// the check it made, never what a later check left in the status.
type checked struct {
	installed string
	latest    string
	available bool
	err       error
	// binary is the install the check examined.
	binary Binary
	// update installs latest on that install. Set when err is nil.
	update Command
}

// New returns a Service with no check made. Call Start to begin the hourly
// check and Stop to end it.
func New(deps Deps) *Service {
	life, stop := context.WithCancel(context.Background())
	s := &Service{deps: deps, life: life, stop: stop, providers: make(map[string]*providerState, len(providerOrder))}
	for _, name := range providerOrder {
		s.providers[name] = &providerState{status: Status{Provider: name, Run: Run{Phase: PhaseIdle}}}
	}
	return s
}

// admitLocked joins one piece of work to the set Stop waits for, and
// refuses once Stop has run. The caller holds mu and calls work.Done when
// the work ends.
func (s *Service) admitLocked() error {
	if s.stopped {
		return ErrShuttingDown
	}
	s.work.Add(1)
	return nil
}

// admit is admitLocked for a caller that does not hold mu.
func (s *Service) admit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admitLocked()
}

// Snapshot returns every provider's current Status without checking.
func (s *Service) Snapshot() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) snapshotLocked() []Status {
	out := make([]Status, len(providerOrder))
	for i, name := range providerOrder {
		out[i] = s.providers[name].status
	}
	return out
}

// Check looks up every provider again, the providers concurrently, and
// returns the result. Each lookup is bounded by checkTimeout and ends at
// Stop. After Stop it looks nothing up.
func (s *Service) Check() []Status {
	var wg sync.WaitGroup
	for _, name := range providerOrder {
		wg.Go(func() { s.checkProvider(name) })
	}
	wg.Wait()
	return s.Snapshot()
}

// checkProvider checks the provider once, publishes the result and returns
// it. A check already running for the provider finishes first, so the
// result published last is from the check that started last. Once Stop has
// run nothing is published and the result is ErrShuttingDown.
func (s *Service) checkProvider(providerName string) checked {
	if err := s.admit(); err != nil {
		return checked{err: err}
	}
	defer s.work.Done()

	providerState := s.providers[providerName]
	providerState.checkMu.Lock()
	defer providerState.checkMu.Unlock()
	ctx, cancel := context.WithTimeout(s.life, checkTimeout)
	result := s.lookup(ctx, providerName)
	cancel()
	published := false
	s.update(func() bool {
		if s.stopped {
			return false
		}
		published = true
		status := &providerState.status
		status.InstalledVersion = result.installed
		status.LatestVersion, status.Available, status.CheckError = result.latest, result.available, ""
		if result.err != nil {
			status.CheckError = result.err.Error()
		}
		status.CheckedAt = s.deps.Now().UnixMilli()
		return true
	})
	if !published {
		return checked{installed: result.installed, err: ErrShuttingDown}
	}
	return result
}

// lookup checks one install: the binary and environment read once, used
// for every command and request of the check and for the update it plans,
// and read again at its end. An install that changed meanwhile fails the
// check, since its answer may describe the old install and its commands
// may have reached either.
func (s *Service) lookup(ctx context.Context, providerName string) checked {
	binary, env := s.deps.Binary(providerName), s.deps.Env(providerName)
	installed := s.deps.Installed(providerName)
	if installed == "" {
		return checked{err: fmt.Errorf("could not read the installed %s version", providerName)}
	}
	latest, update, err := s.plan(ctx, providerName, binary, env)
	if err != nil {
		return checked{installed: installed, err: err}
	}
	available, err := newer(latest, installed)
	if err != nil {
		return checked{installed: installed, err: err}
	}
	nowBinary, nowInstalled := s.deps.Binary(providerName), s.deps.Installed(providerName)
	if nowBinary != binary || nowInstalled != installed {
		return checked{installed: nowInstalled, err: fmt.Errorf("the installed %s changed during the check; check again", providerName)}
	}
	return checked{installed: installed, latest: latest, available: available, binary: binary, update: update}
}

// update applies change under mu and, when it reports a change, emits the
// whole list in order with every other change.
func (s *Service) update(change func() bool) {
	s.publish.Lock()
	defer s.publish.Unlock()
	s.mu.Lock()
	changed := change()
	snapshot := s.snapshotLocked()
	s.mu.Unlock()
	if changed {
		s.deps.Emit(snapshot)
	}
}

// Start begins the periodic check: one now, then one per tick. A second
// call, or one after Stop, does nothing.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.admitLocked() != nil {
		return
	}
	s.started = true
	ticks := s.deps.Ticks
	go func() {
		defer s.work.Done()
		if ticks == nil {
			ticker := time.NewTicker(checkInterval)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for {
			s.Check()
			select {
			case <-s.life.Done():
				return
			case <-ticks:
			}
		}
	}()
}

// Stop ends the loop, cancels checks, notes lookups and a running update
// command, and waits for all of them. Later checks, runs and notes lookups
// are refused. Safe to call more than once.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.stop()
	s.work.Wait()
}

func unknownProvider(providerName string) error {
	return fmt.Errorf("unknown provider %q", providerName)
}

// command builds one invocation of executable with env, in the work
// directory. Callers pass the install their check read, never fresh Deps
// reads.
func (s *Service) command(providerName, executable string, env map[string]string, args ...string) (Command, error) {
	if executable == "" {
		return Command{}, fmt.Errorf("no %s binary is configured", providerName)
	}
	dir, err := s.deps.WorkDir()
	if err != nil {
		return Command{}, fmt.Errorf("resolve the directory to run %s in: %w", providerName, err)
	}
	return Command{Binary: executable, Args: args, Env: env, Dir: dir}, nil
}
