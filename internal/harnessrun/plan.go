package harnessrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// PlanVersion is the only run-plan schema understood by this build.
const PlanVersion = 1

const (
	DefaultRunCeilingBytes        uint64 = 600 << 20
	DefaultActivePaneCeilingBytes uint64 = 900 << 20
	// time.Duration stores nanoseconds in a signed 64-bit integer. Keep every
	// portable millisecond field below the conversion limit before a caller
	// multiplies it by time.Millisecond.
	maxDurationMS int64 = (1<<63 - 1) / 1_000_000
)

// Adapter identifies one built-in managed command. There is deliberately no
// free-form argv or shell adapter in a run plan.
type Adapter string

const (
	AdapterBench      Adapter = "bench"
	AdapterProfile    Adapter = "profile"
	AdapterFunctional Adapter = "functional"
	AdapterCompare    Adapter = "compare"
)

// Ownership says who may remove the data root after a run.
type Ownership string

const (
	OwnershipFresh    Ownership = "fresh"
	OwnershipBorrowed Ownership = "borrowed"
)

// Ceiling contains limits belonging to one run. Zero means that the
// particular limit is disabled. Values are JSON numbers in their native
// units, not time.Duration strings, so manifests remain portable.
type Ceiling struct {
	MaxDurationMS   int64  `json:"maxDurationMs,omitempty"`
	MaxPrivateBytes uint64 `json:"maxPrivateBytes,omitempty"`
	MaxCPUPercent   uint32 `json:"maxCpuPercent,omitempty"`
	MaxChildren     uint32 `json:"maxChildren,omitempty"`
	MaxOutputBytes  uint64 `json:"maxOutputBytes,omitempty"`
}

func (c Ceiling) validate() error {
	if c.MaxDurationMS < 0 {
		return errors.New("maxDurationMs must be non-negative")
	}
	if c.MaxDurationMS > maxDurationMS {
		return fmt.Errorf("maxDurationMs exceeds time.Duration limit of %d milliseconds", maxDurationMS)
	}
	if c.MaxCPUPercent > 100 {
		return errors.New("maxCpuPercent must be between 0 and 100")
	}
	if c.MaxCPUPercent != 0 || c.MaxChildren != 0 || c.MaxOutputBytes != 0 {
		return errors.New("maxCpuPercent, maxChildren, and maxOutputBytes are not supported")
	}
	return nil
}

// ArtifactPlan declares a source output under DataRoot that must be durable
// before a successful run can remove its fresh root. Destination, when
// present, is a durable copy under ArtifactRoot written atomically by
// RecordArtifact. Adapter reports use RunPlan.Output instead because they
// are written directly under ArtifactRoot.
type ArtifactPlan struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Destination string `json:"destination,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

func (a ArtifactPlan) validate(root, artifactRoot string) error {
	if strings.TrimSpace(a.Name) == "" {
		return errors.New("artifact name is required")
	}
	if filepath.IsAbs(a.Path) || a.Path == "" || filepath.Clean(a.Path) == "." || filepath.Clean(a.Path) == ".." || strings.HasPrefix(filepath.Clean(a.Path), ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact %q path must be relative to the data root", a.Name)
	}
	if a.Destination != "" {
		if !filepath.IsAbs(a.Destination) {
			return fmt.Errorf("artifact %q destination must be absolute", a.Name)
		}
		if !underPath(filepath.Clean(a.Destination), filepath.Clean(artifactRoot)) {
			return fmt.Errorf("artifact %q destination must be inside the supervisor artifact root %s", a.Name, artifactRoot)
		}
	}
	return nil
}

// ArtifactRoot returns the only directory a managed run may use for outputs
// that outlive its data root. It is derived from both the selected root and
// run id, so two worktrees cannot overwrite one another's reports.
func ArtifactRoot(p RunPlan) string {
	return filepath.Join(filepath.Clean(p.DataRoot)+".artifacts", p.RunID)
}

func underPath(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// RunPlan is the immutable input to a supervisor. Unknown JSON fields are
// refused by DecodePlan. Callers should construct this value and pass it to
// New, rather than editing a manifest after creation.
type RunPlan struct {
	Version  int     `json:"version"`
	RunID    string  `json:"runId"`
	Workload string  `json:"workload"`
	DataRoot string  `json:"dataRoot"`
	Instance string  `json:"instance,omitempty"`
	PageID   string  `json:"pageId,omitempty"`
	Adapter  Adapter `json:"adapter,omitempty"`
	Thread   string  `json:"thread,omitempty"`
	Scenario string  `json:"scenario,omitempty"`
	Message  string  `json:"message,omitempty"`
	Capsule  string  `json:"capsule,omitempty"`
	// Output is the adapter's durable report path under ArtifactRoot. It is
	// separate from ArtifactPlan.Path, whose source is relative to DataRoot
	// and is copied to its Destination before a fresh root is removed.
	Output     string    `json:"output,omitempty"`
	Repeat     int       `json:"repeat,omitempty"`
	DurationMS int64     `json:"durationMs,omitempty"`
	SampleMS   int       `json:"sampleMs,omitempty"`
	BudgetsMS  []float64 `json:"budgetsMs,omitempty"`
	Meters     []string  `json:"meters,omitempty"`
	Monitors   []string  `json:"monitors,omitempty"`
	MonitorLeg string    `json:"monitorLeg,omitempty"`
	// Instrument selects the compare leg's measurement mode. It is kept on
	// the plan rather than inferred by the adapter so a durable manifest says
	// exactly what both A/B legs were asked to do.
	Instrument string `json:"instrument,omitempty"`
	Leg        string `json:"leg,omitempty"`
	Trace      bool   `json:"trace,omitempty"`
	CDP        string `json:"cdp,omitempty"`
	IntervalUS int    `json:"intervalUs,omitempty"`
	TimeoutMS  int64  `json:"timeoutMs,omitempty"`
	Pairs      int    `json:"pairs,omitempty"`
	KeepRoots  bool   `json:"keepRoots,omitempty"`
	// PreserveRoot keeps a successful fresh run root for inspection. Failed
	// fresh runs are quarantined regardless of this value.
	PreserveRoot bool           `json:"preserveRoot,omitempty"`
	BaseDir      string         `json:"baseDir,omitempty"`
	Binary       string         `json:"binary,omitempty"`
	MockProvider string         `json:"mockProvider,omitempty"`
	Window       bool           `json:"window,omitempty"`
	KeepHome     bool           `json:"keepHome,omitempty"`
	DevAssetsURL string         `json:"devAssetsUrl,omitempty"`
	Ownership    Ownership      `json:"ownership"`
	Ceiling      Ceiling        `json:"ceiling"`
	Artifacts    []ArtifactPlan `json:"artifacts,omitempty"`
}

// ApplyDefaults fills mandatory safety policy without changing explicit
// caller limits.
func ApplyDefaults(p RunPlan) RunPlan {
	if p.Adapter == "" {
		p.Adapter = AdapterBench
	}
	if p.Ceiling.MaxPrivateBytes == 0 {
		if p.Workload == "active-multi-pane" {
			p.Ceiling.MaxPrivateBytes = DefaultActivePaneCeilingBytes
		} else {
			p.Ceiling.MaxPrivateBytes = DefaultRunCeilingBytes
		}
	}
	if p.Adapter == AdapterCompare {
		// Compare needs a real page for its semantic gate. The zero value is
		// the historical omitted field, so default it to the only valid mode.
		if !p.Window {
			p.Window = true
		}
		if strings.TrimSpace(p.Instrument) == "" {
			p.Instrument = "perf"
		}
	}
	if p.Adapter == AdapterBench && p.Ownership == OwnershipFresh && strings.TrimSpace(p.Output) == "" {
		// A fresh bench removes its data root after success. Put its report in
		// the supervisor artifact root by default so a successful run cannot
		// discard the only measurement it produced.
		p.Output = filepath.Join(ArtifactRoot(p), "bench-report.json")
	}
	return p
}

func cloneRunPlan(p RunPlan) RunPlan {
	p.BudgetsMS = append([]float64(nil), p.BudgetsMS...)
	p.Meters = append([]string(nil), p.Meters...)
	p.Monitors = append([]string(nil), p.Monitors...)
	p.Artifacts = append([]ArtifactPlan(nil), p.Artifacts...)
	return p
}

// DecodePlan decodes the strict, versioned wire representation of a plan.
func DecodePlan(data []byte) (RunPlan, error) {
	if err := rejectDuplicateJSON(data); err != nil {
		return RunPlan{}, fmt.Errorf("decode run plan: %w", err)
	}
	var p RunPlan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return RunPlan{}, fmt.Errorf("decode run plan: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return RunPlan{}, errors.New("decode run plan: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return RunPlan{}, fmt.Errorf("decode run plan: trailing data: %w", err)
	}
	p = ApplyDefaults(p)
	if err := p.Validate(); err != nil {
		return RunPlan{}, err
	}
	return p, nil
}

func decodeStrict(data []byte, out any) error {
	if err := rejectDuplicateJSON(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

// rejectDuplicateJSON keeps the strict contract from being undermined by
// encoding/json's last-key-wins behavior.
func rejectDuplicateJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(dec); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func walkJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch delim := tok.(type) {
	case json.Delim:
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, ok := seen[name]; ok {
					return fmt.Errorf("duplicate JSON field %q", name)
				}
				seen[name] = struct{}{}
				if err := walkJSONValue(dec); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		case '[':
			for dec.More() {
				if err := walkJSONValue(dec); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
	}
	return err
}

// Validate enforces the plan contract before any root mutation.
func (p RunPlan) Validate() error {
	if p.Version != PlanVersion {
		return fmt.Errorf("unsupported run plan version %d (want %d)", p.Version, PlanVersion)
	}
	if strings.TrimSpace(p.RunID) == "" {
		return errors.New("runId is required")
	}
	if filepath.Base(p.RunID) != p.RunID || p.RunID == "." || p.RunID == ".." {
		return errors.New("runId must be a single path component")
	}
	if strings.TrimSpace(p.Workload) == "" {
		return errors.New("workload is required")
	}
	if p.PageID != strings.TrimSpace(p.PageID) {
		return errors.New("pageId must not have leading or trailing whitespace")
	}
	if p.Adapter == "" {
		p.Adapter = AdapterBench
	}
	switch p.Adapter {
	case AdapterBench:
		if p.Repeat < 0 {
			return errors.New("repeat must be non-negative")
		}
		if p.DurationMS < 0 || p.SampleMS < 0 {
			return errors.New("durationMs and sampleMs must be non-negative")
		}
		if p.DurationMS > maxDurationMS || int64(p.SampleMS) > maxDurationMS {
			return fmt.Errorf("durationMs and sampleMs must not exceed %d milliseconds", maxDurationMS)
		}
		for _, budget := range p.BudgetsMS {
			if budget <= 0 {
				return errors.New("budgetsMs entries must be positive")
			}
		}
	case AdapterProfile:
		if p.Ownership == OwnershipFresh {
			return errors.New("profile adapter requires borrowed ownership: a fresh root has no thread to profile")
		}
		if strings.TrimSpace(p.Thread) == "" || strings.TrimSpace(p.Scenario) == "" {
			return errors.New("profile adapter requires thread and scenario")
		}
		if p.IntervalUS < 0 || p.TimeoutMS < 0 {
			return errors.New("intervalUs and timeoutMs must be non-negative")
		}
		if p.TimeoutMS > maxDurationMS {
			return fmt.Errorf("timeoutMs must not exceed %d milliseconds", maxDurationMS)
		}
	case AdapterCompare:
		if strings.TrimSpace(p.Capsule) == "" {
			return errors.New("compare adapter requires capsule")
		}
		if !p.Window {
			return errors.New("compare adapter requires a windowed frontend")
		}
		if p.Instrument != "" && p.Instrument != "perf" && p.Instrument != "none" {
			return fmt.Errorf("compare instrument %q is unsupported (want perf or none)", p.Instrument)
		}
	case AdapterFunctional:
		if strings.TrimSpace(p.Scenario) == "" || strings.TrimSpace(p.Output) == "" {
			return errors.New("functional adapter requires scenario and output")
		}
	default:
		return fmt.Errorf("unknown run adapter %q", p.Adapter)
	}
	for name, value := range map[string]string{"instance": p.Instance, "output": p.Output, "capsule": p.Capsule, "baseDir": p.BaseDir, "binary": p.Binary, "mockProvider": p.MockProvider} {
		if value != "" && !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be absolute", name)
		}
	}
	if p.Adapter == AdapterFunctional && !filepath.IsAbs(p.Scenario) {
		return errors.New("scenario must be absolute for the functional adapter")
	}
	if p.CDP != strings.TrimSpace(p.CDP) {
		return errors.New("cdp must not have leading or trailing whitespace")
	}
	if len(p.Meters) > 0 {
		seenMeters := map[string]bool{}
		for _, meter := range p.Meters {
			if strings.TrimSpace(meter) == "" || seenMeters[meter] {
				return errors.New("meters must be unique and non-empty")
			}
			seenMeters[meter] = true
		}
	}
	if len(p.Monitors) > 0 {
		seenMonitors := map[string]bool{}
		for _, monitor := range p.Monitors {
			if strings.TrimSpace(monitor) == "" || seenMonitors[monitor] {
				return errors.New("monitors must be unique and non-empty")
			}
			seenMonitors[monitor] = true
		}
	}
	if strings.TrimSpace(p.DataRoot) == "" {
		return errors.New("dataRoot is required")
	}
	if !filepath.IsAbs(p.DataRoot) {
		return errors.New("dataRoot must be an absolute path")
	}
	if filepath.Clean(p.DataRoot) == string(filepath.Separator) {
		return errors.New("dataRoot cannot be the filesystem root")
	}
	if p.Ownership != OwnershipFresh && p.Ownership != OwnershipBorrowed {
		return fmt.Errorf("ownership must be %q or %q", OwnershipFresh, OwnershipBorrowed)
	}
	if err := p.Ceiling.validate(); err != nil {
		return fmt.Errorf("invalid ceiling: %w", err)
	}
	seen := make(map[string]struct{}, len(p.Artifacts))
	artifactRoot := ArtifactRoot(p)
	for _, a := range p.Artifacts {
		if _, ok := seen[a.Name]; ok {
			return fmt.Errorf("duplicate artifact %q", a.Name)
		}
		seen[a.Name] = struct{}{}
		if err := a.validate(filepath.Clean(p.DataRoot), artifactRoot); err != nil {
			return fmt.Errorf("invalid artifact: %w", err)
		}
	}
	if p.Output != "" && !underPath(filepath.Clean(p.Output), artifactRoot) {
		return fmt.Errorf("output must be inside the supervisor artifact root %s", artifactRoot)
	}
	return nil
}
