package harnessrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agent-overflow/internal/atomicfile"
)

// Supervisor serializes manifest changes and owns one root lease.
type Supervisor struct {
	mu             sync.Mutex
	groupsMu       sync.Mutex
	root           string
	rootIdentity   rootIdentity
	manifest       Manifest
	lease          *Lease
	groups         []ProcessGroup
	groupsStopping bool
	cleanupTimeout time.Duration
	retention      *ArtifactRegistry
	closed         bool
}

// CleanupTimeout bounds all supervisor-owned teardown work. A caller may use
// a shorter parent deadline, but a cleanup callback cannot run unbounded.
const DefaultCleanupTimeout = 10 * time.Second

type rootIdentity struct {
	info  os.FileInfo
	token string
}

func ownedRootIdentity(root string) (rootIdentity, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return rootIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return rootIdentity{}, errors.New("data root is not an owned directory")
	}
	if err := rejectSymlinkedParents(root); err != nil {
		return rootIdentity{}, err
	}
	marker := filepath.Join(root, RootIdentityFileName)
	markerInfo, err := os.Lstat(marker)
	if err != nil {
		return rootIdentity{}, fmt.Errorf("inspect data root identity: %w", err)
	}
	if markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return rootIdentity{}, errors.New("data root identity is not a regular file")
	}
	token, err := os.ReadFile(marker)
	if err != nil {
		return rootIdentity{}, fmt.Errorf("read data root identity: %w", err)
	}
	if len(token) == 0 {
		return rootIdentity{}, errors.New("data root identity is empty")
	}
	return rootIdentity{info: info, token: string(token)}, nil
}

// New validates the plan and establishes both the manifest and lease before
// any caller workload mutation. Borrowed roots acquire the lease first so a
// live run's manifest cannot be overwritten.
func New(plan RunPlan, now time.Time) (*Supervisor, error) {
	return NewWithOptions(plan, now, SupervisorOptions{})
}

// SupervisorOptions controls bounded supervisor behavior.
type SupervisorOptions struct {
	CleanupTimeout time.Duration
	// Retention is optional so library users can choose an isolated registry.
	// Production harness supervisors should pass NewDefaultArtifactRegistry.
	Retention *ArtifactRegistry
}

// NewWithOptions is New with an explicit cleanup bound, useful for callers
// with a tighter run budget and for deterministic tests.
func NewWithOptions(plan RunPlan, now time.Time, opts SupervisorOptions) (*Supervisor, error) {
	plan = ApplyDefaults(plan)
	plan = cloneRunPlan(plan)
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if opts.CleanupTimeout <= 0 {
		opts.CleanupTimeout = DefaultCleanupTimeout
	}
	// The lease is acquired before CreateManifest for both ownership modes.
	// For a fresh root this is the reservation that closes the race between
	// two supervisors deciding that an empty directory is theirs. CreateManifest
	// permits the lease file as the sole pre-existing entry.
	l, err := AcquireLease(plan.DataRoot, plan.RunID, now)
	if err != nil {
		return nil, err
	}
	m, err := CreateManifest(plan, now)
	if err != nil {
		if releaseErr := l.Release(); releaseErr != nil {
			return nil, errors.Join(err, fmt.Errorf("release run lease after manifest failure: %w", releaseErr))
		}
		return nil, err
	}
	root := filepath.Clean(plan.DataRoot)
	rootToken, tokenErr := randomToken()
	if tokenErr != nil {
		return nil, errors.Join(fmt.Errorf("create data root identity: %w", tokenErr), releaseLeaseOnCreateFailure(l))
	}
	if err := atomicfile.Write(filepath.Join(root, RootIdentityFileName), []byte(rootToken)); err != nil {
		return nil, errors.Join(fmt.Errorf("publish data root identity: %w", err), releaseLeaseOnCreateFailure(l))
	}
	rootIdentity, err := ownedRootIdentity(root)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("capture data root identity: %w", err), releaseLeaseOnCreateFailure(l))
	}
	s := &Supervisor{root: root, rootIdentity: rootIdentity, manifest: m, lease: l, cleanupTimeout: opts.CleanupTimeout, retention: opts.Retention}
	s.manifest.Phase = PhaseLease
	if err := s.persistLocked(); err != nil {
		return nil, errors.Join(err, releaseLeaseOnCreateFailure(l))
	}
	return s, nil
}

func releaseLeaseOnCreateFailure(lease *Lease) error {
	if lease == nil {
		return nil
	}
	if err := lease.Release(); err != nil {
		return fmt.Errorf("release run lease after supervisor setup failure: %w", err)
	}
	return nil
}

func (s *Supervisor) persistLocked() error {
	s.manifest.UpdatedAt = time.Now().UTC()
	return writeManifest(s.root, s.manifest)
}

func canTransition(from, to State) bool {
	switch from {
	case StateCreated:
		return to == StatePreparing || to == StateFailed
	case StatePreparing:
		return to == StateReady || to == StateFailed
	case StateReady:
		return to == StateRunning || to == StateStopping || to == StateFailed
	case StateRunning:
		return to == StateStopping || to == StateFailed
	case StateStopping:
		return to == StateSucceeded || to == StateFailed || to == StateQuarantined
	case StateFailed:
		return to == StateQuarantined
	default:
		return false
	}
}

// Transition records a legal lifecycle transition and optional phase.
func (s *Supervisor) Transition(to State, phase Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("run supervisor is closed")
	}
	if !canTransition(s.manifest.State, to) {
		return fmt.Errorf("invalid run transition %q -> %q", s.manifest.State, to)
	}
	oldState, oldPhase := s.manifest.State, s.manifest.Phase
	s.manifest.State, s.manifest.Phase = to, phase
	if to == StateStopping {
		s.groupsStopping = true
	}
	if err := s.persistLocked(); err != nil {
		s.manifest.State, s.manifest.Phase = oldState, oldPhase
		if to == StateStopping {
			s.groupsStopping = false
		}
		return err
	}
	return nil
}

// Manifest returns a snapshot safe for reporting.
func (s *Supervisor) Manifest() Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.manifest
	m.Plan = cloneRunPlan(s.manifest.Plan)
	if s.manifest.Bootstrap != nil {
		bootstrap := *s.manifest.Bootstrap
		m.Bootstrap = &bootstrap
	}
	m.Artifacts = append([]ArtifactRecord(nil), s.manifest.Artifacts...)
	m.ProcessGroups = append([]ProcessGroupRecord(nil), s.manifest.ProcessGroups...)
	return m
}

// RecordBootstrap persists the launch identity before the adapter acts.
func (s *Supervisor) RecordBootstrap(record BootstrapRecord) error {
	if record.PID <= 0 || record.DataRoot == "" || record.DataDir == "" {
		return errors.New("invalid harness bootstrap identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.groupsStopping {
		return errors.New("run supervisor is closed")
	}
	old := s.manifest.Bootstrap
	s.manifest.Bootstrap = &record
	if err := s.persistLocked(); err != nil {
		s.manifest.Bootstrap = old
		return err
	}
	return nil
}

// Fail records the primary failure. It never overwrites an existing primary
// error with cleanup noise.
func (s *Supervisor) Fail(class FailureClass, phase Phase, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("run supervisor is closed")
	}
	if cause == nil {
		cause = errors.New("run failed")
	}
	if class == FailureNone {
		class = failureClassForPhase(phase)
	}
	if s.manifest.State == StateSucceeded || s.manifest.State == StateQuarantined {
		return fmt.Errorf("cannot fail terminal run %q", s.manifest.State)
	}
	old := s.manifest
	now := time.Now().UTC()
	s.manifest.State, s.manifest.Phase, s.manifest.Failure, s.manifest.FinishedAt = StateFailed, phase, class, &now
	s.manifest.Error = cause.Error()
	if err := s.persistLocked(); err != nil {
		s.manifest = old
		return err
	}
	return nil
}

func failureClassForPhase(phase Phase) FailureClass {
	switch phase {
	case PhaseManifest:
		return FailurePlan
	case PhaseLease:
		return FailureLease
	case PhasePrepare:
		return FailureReadiness
	case PhaseAction:
		return FailureAction
	case PhaseProbe:
		return FailureProbe
	case PhaseTeardown:
		return FailureTeardown
	default:
		return FailureAction
	}
}

// Finish runs one bounded cleanup callback, preserves the primary error over
// cleanup errors, and applies the ownership disposition. The callback must
// stop children and close pages. It is run before a fresh root can be deleted.
func (s *Supervisor) Finish(parent context.Context, primary error, class FailureClass, cleanup func(context.Context) error) error {
	if parent == nil {
		parent = context.Background()
	}
	if err := s.transitionToStopping(); err != nil && primary == nil {
		primary = err
	}
	s.mu.Lock()
	cleanupTimeout := s.cleanupTimeout
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, cleanupTimeout)
	defer cancel()
	cleanupErr := s.terminateGroups(ctx)
	if cleanup != nil {
		cleanupErr = errors.Join(cleanupErr, cleanup(ctx))
	}
	if primary != nil || cleanupErr != nil {
		if class == FailureNone {
			class = FailureTeardown
		}
		if err := s.Fail(class, PhaseTeardown, primaryOr(primary, cleanupErr)); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if cleanupErr != nil {
			// A cleanup error means ownership has not been proven gone. Keep the
			// lease and the original root so a survivor cannot race a later run or
			// be deleted by retention.
			return primaryWithCleanup(primary, cleanupErr)
		}
		qErr := s.quarantineContext(ctx)
		if qErr != nil {
			cleanupErr = errors.Join(cleanupErr, qErr)
			return primaryWithCleanup(primary, cleanupErr)
		}
		releaseErr := s.releaseLease()
		cleanupErr = errors.Join(cleanupErr, releaseErr)
		return primaryWithCleanup(primary, cleanupErr)
	}
	if err := s.Complete(); err != nil {
		_ = s.Fail(FailureArtifact, PhaseFinalize, err)
		qErr := s.quarantineContext(ctx)
		if qErr != nil {
			return primaryWithCleanup(err, qErr)
		}
		releaseErr := s.releaseLease()
		return primaryWithCleanup(err, errors.Join(qErr, releaseErr))
	}
	s.mu.Lock()
	fresh := s.manifest.Plan.Ownership == OwnershipFresh
	preserveRoot := s.manifest.Plan.PreserveRoot
	s.mu.Unlock()
	if fresh && !preserveRoot {
		if err := s.verifyDestructiveRoot(); err != nil {
			return fmt.Errorf("refuse removal of fresh root: %w", err)
		}
		if err := os.RemoveAll(s.root); err != nil {
			return fmt.Errorf("remove successful fresh root: %w", err)
		}
	}
	if err := s.releaseLease(); err != nil {
		return err
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// verifyDestructiveRoot is the final ownership check before deleting or
// moving a fresh root. A path is not an identity. A failed run can outlive a
// caller that replaces the path, and a late cleanup must never remove that
// replacement or a root leased by a newer run.
func (s *Supervisor) verifyDestructiveRoot() error {
	s.mu.Lock()
	root := s.root
	expected := s.rootIdentity
	lease := s.lease
	s.mu.Unlock()
	actual, err := ownedRootIdentity(root)
	if err != nil {
		return err
	}
	if expected.info == nil || actual.info == nil || !os.SameFile(expected.info, actual.info) || expected.token != actual.token {
		return errors.New("data root identity changed")
	}
	if lease == nil {
		return errors.New("run lease is missing")
	}
	if err := lease.Verify(); err != nil {
		return fmt.Errorf("verify active run lease: %w", err)
	}
	return nil
}

func (s *Supervisor) transitionToStopping() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifest.State == StateStopping {
		return nil
	}
	if !canTransition(s.manifest.State, StateStopping) {
		return fmt.Errorf("cannot stop run from %q", s.manifest.State)
	}
	s.manifest.State, s.manifest.Phase = StateStopping, PhaseTeardown
	s.groupsStopping = true
	return s.persistLocked()
}

func (s *Supervisor) releaseLease() error {
	s.mu.Lock()
	l := s.lease
	s.mu.Unlock()
	if l == nil {
		return nil
	}
	if err := l.Release(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.lease == l {
		s.lease = nil
	}
	s.mu.Unlock()
	return nil
}

func primaryOr(primary, cleanup error) error {
	if primary != nil {
		return primary
	}
	return cleanup
}

func primaryWithCleanup(primary, cleanup error) error {
	if primary == nil {
		return cleanup
	}
	if cleanup == nil {
		return primary
	}
	return fmt.Errorf("%w (cleanup: %v)", primary, cleanup)
}
