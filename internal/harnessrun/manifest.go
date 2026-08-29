package harnessrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/atomicfile"
)

const (
	ManifestFileName     = "run-manifest.json"
	LeaseFileName        = "run-lease.json"
	RootIdentityFileName = "run-root-identity"
	QuarantineSuffix     = ".quarantine"
)

// State is the durable run state. Transitions are checked rather than
// trusting callers to only write sensible strings.
type State string

const (
	StateCreated     State = "created"
	StatePreparing   State = "preparing"
	StateReady       State = "ready"
	StateRunning     State = "running"
	StateStopping    State = "stopping"
	StateSucceeded   State = "succeeded"
	StateFailed      State = "failed"
	StateQuarantined State = "quarantined"
)

// Phase identifies the part of a run which produced a failure.
type Phase string

const (
	PhaseManifest Phase = "manifest"
	PhaseLease    Phase = "lease"
	PhasePrepare  Phase = "prepare"
	PhaseReady    Phase = "ready"
	PhaseAction   Phase = "action"
	PhaseProbe    Phase = "probe"
	PhaseTeardown Phase = "teardown"
	PhaseFinalize Phase = "finalize"
)

// FailureClass is stable enough for reports and gate logic to consume.
type FailureClass string

const (
	FailureNone      FailureClass = ""
	FailurePlan      FailureClass = "plan"
	FailureLease     FailureClass = "lease"
	FailureReadiness FailureClass = "readiness"
	FailureAction    FailureClass = "action"
	FailureProbe     FailureClass = "probe"
	// FailureSafetyCeiling covers both a run's private-memory ceiling and the
	// host available-memory floor enforced by the governor.
	FailureSafetyCeiling      FailureClass = "safety-ceiling"
	FailureBrowserReset       FailureClass = "browser-reset"
	FailureLauncherExit       FailureClass = "launcher-exit"
	FailureProviderDisconnect FailureClass = "provider-disconnect"
	FailureTeardown           FailureClass = "teardown"
	FailureArtifact           FailureClass = "artifact"
	FailureQuarantine         FailureClass = "quarantine"
)

// ArtifactStatus describes what the supervisor proved about an artifact.
type ArtifactStatus string

const (
	ArtifactPending ArtifactStatus = "pending"
	ArtifactDurable ArtifactStatus = "durable"
	ArtifactMissing ArtifactStatus = "missing"
	ArtifactFailed  ArtifactStatus = "failed"
)

type ArtifactRecord struct {
	Name        string         `json:"name"`
	Path        string         `json:"path"`
	Destination string         `json:"destination,omitempty"`
	Status      ArtifactStatus `json:"status"`
	SHA256      string         `json:"sha256,omitempty"`
	Bytes       int64          `json:"bytes,omitempty"`
	Error       string         `json:"error,omitempty"`
	RecordedAt  time.Time      `json:"recordedAt,omitempty"`
}

type LeaseRecord struct {
	Token        string    `json:"token"`
	RunID        string    `json:"runId"`
	PID          int       `json:"pid"`
	OwnerBirthID string    `json:"ownerBirthId,omitempty"`
	AcquiredAt   time.Time `json:"acquiredAt"`
	HeartbeatAt  time.Time `json:"heartbeatAt,omitempty"`
}

// Manifest is the durable report prefix. It is written atomically on every
// state change, so a killed supervisor leaves a parseable partial report.
type Manifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Plan          RunPlan              `json:"plan"`
	State         State                `json:"state"`
	Phase         Phase                `json:"phase"`
	Failure       FailureClass         `json:"failureClass,omitempty"`
	Error         string               `json:"error,omitempty"`
	StartedAt     time.Time            `json:"startedAt"`
	UpdatedAt     time.Time            `json:"updatedAt"`
	FinishedAt    *time.Time           `json:"finishedAt,omitempty"`
	Artifacts     []ArtifactRecord     `json:"artifacts,omitempty"`
	Quarantine    string               `json:"quarantine,omitempty"`
	ProcessGroups []ProcessGroupRecord `json:"processGroups,omitempty"`
	Bootstrap     *BootstrapRecord     `json:"bootstrap,omitempty"`
}

// BootstrapRecord is non-secret launch identity evidence. Authentication
// tokens stay in the harness instance file and never enter a run manifest.
type BootstrapRecord struct {
	DataRoot string `json:"dataRoot"`
	DataDir  string `json:"dataDir"`
	URL      string `json:"url,omitempty"`
	PID      int    `json:"pid"`
	Version  string `json:"version,omitempty"`
}

type ProcessGroupRecord struct {
	ID       string `json:"id"`
	Owned    bool   `json:"owned"`
	PID      int    `json:"pid,omitempty"`
	GroupPID int    `json:"groupPid,omitempty"`
}

func manifestPath(root string) string { return filepath.Join(root, ManifestFileName) }

// ManifestPath returns the durable manifest path for a data root.
func ManifestPath(root string) string { return manifestPath(root) }

func writeManifest(root string, m Manifest) error {
	if err := atomicfile.WriteJSON(manifestPath(root), m); err != nil {
		return err
	}
	return syncDir(root)
}

// ReadManifest reads a partial or terminal report. A corrupt manifest is an
// error, never treated as an absent run.
func ReadManifest(root string) (Manifest, error) {
	data, err := os.ReadFile(manifestPath(root))
	if err != nil {
		return Manifest{}, fmt.Errorf("read run manifest: %w", err)
	}
	var m Manifest
	if err := decodeStrict(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode run manifest: %w", err)
	}
	if m.SchemaVersion != PlanVersion {
		return Manifest{}, fmt.Errorf("unsupported run manifest version %d (want %d)", m.SchemaVersion, PlanVersion)
	}
	if err := m.Plan.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("manifest plan: %w", err)
	}
	return m, nil
}

// CreateManifest creates a root if needed and atomically publishes the first
// manifest. It is the only supported entry point before caller mutations.
func CreateManifest(plan RunPlan, now time.Time) (Manifest, error) {
	plan = ApplyDefaults(plan)
	plan = cloneRunPlan(plan)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if err := plan.Validate(); err != nil {
		return Manifest{}, err
	}
	root := filepath.Clean(plan.DataRoot)
	if err := rejectRealAppRoot(root); err != nil {
		return Manifest{}, fmt.Errorf("data root: %w", err)
	}
	if plan.Ownership == OwnershipFresh {
		if err := rejectSymlinkedParents(root); err != nil {
			return Manifest{}, fmt.Errorf("fresh data root: %w", err)
		}
		if st, err := os.Lstat(root); err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return Manifest{}, errors.New("fresh data root is a symlink")
			}
			if !st.IsDir() {
				return Manifest{}, errors.New("fresh data root is not a directory")
			}
			entries, readErr := os.ReadDir(root)
			if readErr != nil {
				return Manifest{}, fmt.Errorf("inspect fresh data root: %w", readErr)
			}
			if len(entries) != 0 {
				// New acquires the supervisor lease before publishing any other
				// root state. That lease is the only entry a fresh root may have
				// when manifest creation begins.
				if len(entries) != 1 || entries[0].Name() != LeaseFileName {
					return Manifest{}, errors.New("fresh data root already contains files; use borrowed ownership")
				}
				leaseInfo, leaseErr := os.Lstat(filepath.Join(root, LeaseFileName))
				if leaseErr != nil || leaseInfo.Mode()&os.ModeSymlink != 0 || !leaseInfo.Mode().IsRegular() {
					return Manifest{}, errors.New("fresh data root has an unsafe supervisor lease")
				}
				lease, readErr := readLease(filepath.Join(root, LeaseFileName))
				if readErr != nil || lease.RunID != plan.RunID {
					return Manifest{}, errors.New("fresh data root is leased by another run")
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Manifest{}, fmt.Errorf("inspect fresh data root: %w", err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return Manifest{}, fmt.Errorf("create fresh data root: %w", err)
		}
	} else if _, err := os.Stat(root); err != nil {
		return Manifest{}, fmt.Errorf("borrowed data root: %w", err)
	}
	if err := createArtifactRoot(plan); err != nil {
		return Manifest{}, err
	}
	artifacts := make([]ArtifactRecord, 0, len(plan.Artifacts))
	for _, a := range plan.Artifacts {
		artifacts = append(artifacts, ArtifactRecord{Name: a.Name, Path: a.Path, Destination: a.Destination, Status: ArtifactPending})
	}
	m := Manifest{SchemaVersion: PlanVersion, Plan: plan, State: StateCreated, Phase: PhaseManifest, StartedAt: now, UpdatedAt: now, Artifacts: artifacts}
	if err := writeManifest(root, m); err != nil {
		return Manifest{}, fmt.Errorf("create run manifest: %w", err)
	}
	return m, nil
}
