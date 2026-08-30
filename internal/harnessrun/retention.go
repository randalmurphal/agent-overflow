package harnessrun

// The retention registry is deliberately separate from the per-run manifest.
// A manifest belongs to one data root and can disappear with that root. The
// registry is host-global, so independent worktrees make one quota decision.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/harness/instanceinfo"
)

const (
	RetentionSchemaVersion = 1
	RegistryFileName       = "registry.json"
	RegistryLockName       = "registry.lock"

	// These defaults cap retained failed runs without making a normal bench
	// lose its evidence. The policy is host-global, not per worktree.
	DefaultRetentionMaxBytes int64 = 512 << 20
	DefaultRetentionMaxRuns        = 32
)

type RetentionPolicy struct {
	MaxBytes int64 `json:"maxBytes"`
	MaxRuns  int   `json:"maxRuns"`
}

var DefaultRetentionPolicy = RetentionPolicy{
	MaxBytes: DefaultRetentionMaxBytes,
	MaxRuns:  DefaultRetentionMaxRuns,
}

func (p RetentionPolicy) validate() error {
	if p.MaxBytes < 0 {
		return errors.New("retention maxBytes must be non-negative")
	}
	if p.MaxRuns < 0 {
		return errors.New("retention maxRuns must be non-negative")
	}
	if p.MaxBytes == 0 && p.MaxRuns == 0 {
		return errors.New("retention policy cannot disable both limits")
	}
	return nil
}

// DefaultRegistryDir returns the cache location shared by all checkouts on a
// host. It is cache state, not app data, and contains only harness artifacts.
func DefaultRegistryDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve harness retention cache: %w", err)
	}
	return filepath.Join(base, "agent-overflow", "harness-run-artifacts"), nil
}

// ArtifactEntry is the registry's identity receipt for one quarantined run.
// Root and ManifestSHA256 are checked again before any deletion.
type ArtifactEntry struct {
	RunID          string       `json:"runId"`
	Workload       string       `json:"workload"`
	DataRoot       string       `json:"dataRoot"`
	Root           string       `json:"root"`
	ManifestPath   string       `json:"manifestPath"`
	ManifestSHA256 string       `json:"manifestSha256"`
	Bytes          int64        `json:"bytes"`
	State          State        `json:"state"`
	Failure        FailureClass `json:"failureClass,omitempty"`
	Pinned         bool         `json:"pinned,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
	Deleting       bool         `json:"deleting,omitempty"`
}

type retentionFile struct {
	SchemaVersion int             `json:"schemaVersion"`
	Policy        RetentionPolicy `json:"policy"`
	TotalBytes    int64           `json:"totalBytes"`
	Entries       []ArtifactEntry `json:"entries,omitempty"`
}

type RegistryOptions struct {
	// Directory is intended for tests and explicitly isolated harnesses. An
	// empty value selects DefaultRegistryDir.
	Directory string
	Policy    RetentionPolicy
}

// ArtifactRegistry coordinates host-global failed-run retention. Construct a
// registry explicitly at the command or supervisor boundary. That keeps
// package tests and custom harnesses from touching a user's cache by default.
type ArtifactRegistry struct {
	dir    string
	policy RetentionPolicy
}

// Registry is a short compatibility name for callers that use the generic
// term for this host-global index.
type Registry = ArtifactRegistry

func NewArtifactRegistry(directory string, policy RetentionPolicy) (*ArtifactRegistry, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("artifact registry directory is required")
	}
	if policy == (RetentionPolicy{}) {
		policy = DefaultRetentionPolicy
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact registry directory: %w", err)
	}
	directory = instanceinfo.NormalizeSystemPath(directory)
	if err := rejectRealAppRoot(directory); err != nil {
		return nil, err
	}
	if err := rejectRegistryPath(directory); err != nil {
		return nil, err
	}
	return &ArtifactRegistry{dir: filepath.Clean(directory), policy: policy}, nil
}

func NewRegistry(directory string, policy RetentionPolicy) (*ArtifactRegistry, error) {
	return NewArtifactRegistry(directory, policy)
}

func NewDefaultArtifactRegistry() (*ArtifactRegistry, error) {
	dir, err := DefaultRegistryDir()
	if err != nil {
		return nil, err
	}
	return NewArtifactRegistry(dir, DefaultRetentionPolicy)
}

// OpenArtifactRegistry applies the default host location when Directory is
// empty. It is the constructor used by command wiring that exposes policy as
// options instead of positional arguments.
func OpenArtifactRegistry(opts RegistryOptions) (*ArtifactRegistry, error) {
	directory := opts.Directory
	if strings.TrimSpace(directory) == "" {
		var err error
		directory, err = DefaultRegistryDir()
		if err != nil {
			return nil, err
		}
	}
	if err := rejectRegistryPath(directory); err != nil {
		return nil, err
	}
	if opts.Policy == (RetentionPolicy{}) {
		// An existing registry is authoritative for its quota. This lets
		// offline tools open an explicitly isolated test or operator registry
		// without duplicating its policy on every invocation.
		var file retentionFile
		if data, readErr := os.ReadFile(filepath.Join(directory, RegistryFileName)); readErr == nil {
			if decodeErr := decodeStrict(data, &file); decodeErr == nil {
				opts.Policy = file.Policy
			}
		} else if !errors.Is(readErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("read artifact registry policy: %w", readErr)
		}
	}
	return NewArtifactRegistry(directory, opts.Policy)
}

func rejectRegistryPath(directory string) error {
	directory = filepath.Clean(directory)
	if err := rejectSymlinkedParents(directory); err != nil {
		return fmt.Errorf("artifact registry: %w", err)
	}
	if info, err := os.Lstat(directory); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("artifact registry directory is a symlink")
		}
		if !info.IsDir() {
			return errors.New("artifact registry path is not a directory")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect artifact registry: %w", err)
	}
	return nil
}

func (r *ArtifactRegistry) RegistryPath() string { return filepath.Join(r.dir, RegistryFileName) }

func (r *ArtifactRegistry) withLock(ctx context.Context, fn func(*retentionFile) error) (err error) {
	lock, err := r.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	doc, err := r.load()
	if err != nil {
		return err
	}
	if fnErr := fn(&doc); fnErr != nil {
		return fnErr
	}
	return r.save(doc)
}

func (r *ArtifactRegistry) load() (retentionFile, error) {
	var doc retentionFile
	data, err := os.ReadFile(r.RegistryPath())
	if errors.Is(err, fs.ErrNotExist) {
		doc = retentionFile{SchemaVersion: RetentionSchemaVersion, Policy: r.policy}
		return doc, nil
	}
	if err != nil {
		return retentionFile{}, fmt.Errorf("read artifact registry: %w", err)
	}
	if err := decodeStrict(data, &doc); err != nil {
		return retentionFile{}, fmt.Errorf("decode artifact registry: %w", err)
	}
	if doc.SchemaVersion != RetentionSchemaVersion {
		return retentionFile{}, fmt.Errorf("unsupported artifact registry version %d (want %d)", doc.SchemaVersion, RetentionSchemaVersion)
	}
	if err := doc.Policy.validate(); err != nil {
		return retentionFile{}, fmt.Errorf("artifact registry policy: %w", err)
	}
	if doc.Policy != r.policy {
		return retentionFile{}, fmt.Errorf("artifact registry policy mismatch: file=%+v requested=%+v", doc.Policy, r.policy)
	}
	if err := validateEntries(doc); err != nil {
		return retentionFile{}, err
	}
	return doc, nil
}

func validateEntries(doc retentionFile) error {
	seen := make(map[string]struct{}, len(doc.Entries))
	var total int64
	for _, e := range doc.Entries {
		if e.RunID == "" || filepath.Base(e.RunID) != e.RunID || e.RunID == "." || e.RunID == ".." {
			return fmt.Errorf("artifact registry entry has invalid run id %q", e.RunID)
		}
		if _, ok := seen[e.RunID]; ok {
			return fmt.Errorf("artifact registry has duplicate run id %q", e.RunID)
		}
		seen[e.RunID] = struct{}{}
		if e.Bytes < 0 {
			return fmt.Errorf("artifact registry entry %q has negative size", e.RunID)
		}
		total += e.Bytes
	}
	if total != doc.TotalBytes {
		return fmt.Errorf("artifact registry byte accounting mismatch: total=%d entries=%d", doc.TotalBytes, total)
	}
	return nil
}

func (r *ArtifactRegistry) save(doc retentionFile) error {
	if err := validateEntries(doc); err != nil {
		return err
	}
	doc.SchemaVersion = RetentionSchemaVersion
	doc.Policy = r.policy
	if err := atomicfile.WriteJSON(r.RegistryPath(), doc); err != nil {
		return fmt.Errorf("write artifact registry: %w", err)
	}
	if err := syncDir(r.dir); err != nil {
		return fmt.Errorf("durably publish artifact registry: %w", err)
	}
	return nil
}

// RegisterQuarantine indexes a supervisor-owned failed fresh run. The
// manifest must already be in the quarantined root.
func (r *ArtifactRegistry) RegisterQuarantine(manifest Manifest, root string) error {
	return r.RegisterQuarantineContext(context.Background(), manifest, root)
}

// RegisterQuarantineContext is RegisterQuarantine with a caller-owned lock
// deadline. The supervisor uses this so a stuck host-global registry cannot
// defeat run cleanup bounds.
func (r *ArtifactRegistry) RegisterQuarantineContext(ctx context.Context, manifest Manifest, root string) error {
	if r == nil {
		return errors.New("artifact registry is nil")
	}
	if manifest.Plan.Ownership != OwnershipFresh || manifest.State != StateQuarantined {
		return errors.New("only quarantined fresh runs may enter artifact retention")
	}
	root, err := canonicalOwnedRoot(root)
	if err != nil {
		return err
	}
	if err := verifyManifestIdentity(manifest, root); err != nil {
		return err
	}
	bytes, err := directoryBytesContext(ctx, root)
	if err != nil {
		return fmt.Errorf("measure quarantined run: %w", err)
	}
	manifestSHA, _, err := digestFile(filepath.Join(root, ManifestFileName))
	if err != nil {
		return fmt.Errorf("digest quarantined manifest: %w", err)
	}
	now := time.Now().UTC()
	entry := ArtifactEntry{
		RunID: manifest.Plan.RunID, Workload: manifest.Plan.Workload,
		DataRoot: filepath.Clean(manifest.Plan.DataRoot), Root: root,
		ManifestPath: filepath.Join(root, ManifestFileName), ManifestSHA256: manifestSHA,
		Bytes: bytes, State: manifest.State, Failure: manifest.Failure,
		CreatedAt: manifest.StartedAt, UpdatedAt: now,
	}
	return r.withLock(ctx, func(doc *retentionFile) error {
		for _, existing := range doc.Entries {
			if existing.RunID == entry.RunID {
				return fmt.Errorf("artifact registry already contains run %q", entry.RunID)
			}
		}
		doc.Entries = append(doc.Entries, entry)
		doc.TotalBytes += bytes
		return nil
	})
}

func (r *ArtifactRegistry) Pin(runID string) error   { return r.setPinned(runID, true) }
func (r *ArtifactRegistry) Unpin(runID string) error { return r.setPinned(runID, false) }

func (r *ArtifactRegistry) setPinned(runID string, pinned bool) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("run id is required")
	}
	return r.withLock(context.Background(), func(doc *retentionFile) error {
		for i := range doc.Entries {
			if doc.Entries[i].RunID == runID {
				doc.Entries[i].Pinned = pinned
				doc.Entries[i].UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		return fmt.Errorf("artifact registry has no run %q", runID)
	})
}

// List returns entries oldest first. The returned slice is detached from the
// registry and safe for callers to sort or annotate.
func (r *ArtifactRegistry) List() ([]ArtifactEntry, error) {
	var entries []ArtifactEntry
	err := r.withLock(context.Background(), func(doc *retentionFile) error {
		entries = append([]ArtifactEntry(nil), doc.Entries...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortEntries(entries)
	return entries, nil
}

func (r *ArtifactRegistry) Policy() RetentionPolicy {
	if r == nil {
		return RetentionPolicy{}
	}
	return r.policy
}

type CleanOptions struct {
	DryRun bool
}

type CleanResult struct {
	DryRun      bool            `json:"dryRun"`
	BeforeBytes int64           `json:"beforeBytes"`
	AfterBytes  int64           `json:"afterBytes"`
	BeforeRuns  int             `json:"beforeRuns"`
	AfterRuns   int             `json:"afterRuns"`
	Pruned      []ArtifactEntry `json:"pruned,omitempty"`
	Skipped     []ArtifactEntry `json:"skipped,omitempty"`
}

// Clean removes the oldest unpinned, inactive, unleased entries until both
// policy limits fit. Verification happens while the registry lock is held and
// immediately before removal. A dry run performs all checks but removes no
// root or registry row.
func (r *ArtifactRegistry) Clean(opts CleanOptions) (result CleanResult, err error) {
	return r.CleanContext(context.Background(), opts)
}

// CleanContext is Clean with a cancellation boundary for command shutdown or
// a caller's shorter retention budget.
func (r *ArtifactRegistry) CleanContext(ctx context.Context, opts CleanOptions) (result CleanResult, err error) {
	result.DryRun = opts.DryRun
	lock, err := r.lock(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	doc, err := r.load()
	if err != nil {
		return result, err
	}
	result.BeforeBytes, result.BeforeRuns = doc.TotalBytes, len(doc.Entries)
	// Recover a crash between the durable deleting marker and root removal.
	// A missing marked root has already been deleted, so only its accounting
	// receipt needs to be retired. A present root remains a normal candidate.
	for i := 0; i < len(doc.Entries); {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		entry := doc.Entries[i]
		if !entry.Deleting {
			i++
			continue
		}
		if _, statErr := os.Lstat(entry.Root); errors.Is(statErr, fs.ErrNotExist) {
			doc.TotalBytes -= entry.Bytes
			doc.Entries = append(doc.Entries[:i], doc.Entries[i+1:]...)
			continue
		} else if statErr != nil {
			return result, fmt.Errorf("inspect interrupted retained run %q: %w", entry.RunID, statErr)
		}
		i++
	}
	order := append([]ArtifactEntry(nil), doc.Entries...)
	sortEntries(order)
	for _, candidate := range order {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !overPolicy(&doc, r.policy) {
			break
		}
		idx := entryIndex(doc.Entries, candidate.RunID)
		if idx < 0 {
			continue
		}
		entry := doc.Entries[idx]
		if entry.Pinned {
			result.Skipped = append(result.Skipped, entry)
			continue
		}
		if err := verifyRetainable(entry); err != nil {
			// An active or leased root is a normal skip. Identity and
			// filesystem failures are returned, never silently pruned.
			if errors.Is(err, errRetentionProtected) {
				result.Skipped = append(result.Skipped, entry)
				continue
			}
			return result, fmt.Errorf("verify retained run %q: %w", entry.RunID, err)
		}
		if !opts.DryRun {
			entry.Deleting = true
			doc.Entries[idx] = entry
			if err := r.save(doc); err != nil {
				return result, err
			}
			if err := os.RemoveAll(entry.Root); err != nil {
				entry.Deleting = false
				doc.Entries[idx] = entry
				rollbackErr := r.save(doc)
				return result, fmt.Errorf("remove retained run %q: %w", entry.RunID, errors.Join(err, rollbackErr))
			}
		}
		result.Pruned = append(result.Pruned, entry)
		// Simulate accounting for dry-run, and commit it in memory for the
		// next candidate in either mode.
		doc.TotalBytes -= entry.Bytes
		doc.Entries = append(doc.Entries[:idx], doc.Entries[idx+1:]...)
	}
	result.AfterBytes, result.AfterRuns = doc.TotalBytes, len(doc.Entries)
	if !opts.DryRun {
		if err := r.save(doc); err != nil {
			return result, err
		}
	}
	return result, nil
}

func overPolicy(doc *retentionFile, policy RetentionPolicy) bool {
	return (policy.MaxRuns > 0 && len(doc.Entries) > policy.MaxRuns) || (policy.MaxBytes > 0 && doc.TotalBytes > policy.MaxBytes)
}

func sortEntries(entries []ArtifactEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].RunID < entries[j].RunID
		}
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
}

func entryIndex(entries []ArtifactEntry, runID string) int {
	for i := range entries {
		if entries[i].RunID == runID {
			return i
		}
	}
	return -1
}
