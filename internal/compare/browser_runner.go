package compare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/harnessrun"
)

// BrowserRunnerOptions describes the real app/browser pair used for one leg.
// Binary and MockProvider are explicit because a comparison must not resolve
// either from a developer's live installation. CDP is optional. WebKitGTK
// has no CDP, but can still provide bridge semantics and frontend perf data.
type BrowserRunnerOptions struct {
	Binary        string
	MockProvider  string
	Window        bool
	CDP           string
	PageID        string
	LaunchTimeout time.Duration
	ReplayTimeout time.Duration
	SampleMs      int
	AssetDigest   string
	BuildDigest   string
	PerfMeters    []string
	// Instrument controls whether this leg arms HarnessPerfStart. A clean
	// leg must not pay the sampler's observer and query overhead.
	Instrument string
}

// BrowserRunner launches one isolated backend per Run leg. It never seeds or
// rewrites the restored database. The compare engine supplies fresh roots and
// this runner owns the corresponding process, browser page, perf run, and
// teardown.
type BrowserRunner struct{ Options BrowserRunnerOptions }

func NewBrowserRunner(options BrowserRunnerOptions) *BrowserRunner {
	return &BrowserRunner{Options: options}
}

type supervisorManifest struct {
	Version        int        `json:"version"`
	Leg            Leg        `json:"leg"`
	Pair           int        `json:"pair"`
	Instrument     string     `json:"instrument"`
	RunManifest    string     `json:"runManifest"`
	DataRoot       string     `json:"dataRoot"`
	DataDir        string     `json:"dataDir"`
	BrowserProfile string     `json:"browserProfile"`
	BackendPID     int        `json:"backendPid"`
	PageID         string     `json:"pageId,omitempty"`
	CDPTargetID    string     `json:"cdpTargetId,omitempty"`
	StartedAt      time.Time  `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
}

// launchedProcessGroup adapts the harnessclient launch ownership proof to
// the run supervisor. Terminate already escalates to a verified kill after
// its grace period, so the supervisor's Kill fallback can use the same
// identity-checked operation without introducing a PID-only escape hatch.
type launchedProcessGroup struct{ launched *harnessclient.Launched }

func (g launchedProcessGroup) Record() harnessrun.ProcessGroupRecord {
	return harnessrun.ProcessGroupRecord{ID: fmt.Sprintf("backend-%d", g.launched.PID), Owned: true, PID: g.launched.PID, GroupPID: g.launched.PID}
}

func (g launchedProcessGroup) Terminate(ctx context.Context) error {
	return g.launched.Terminate(ctx)
}

func (g launchedProcessGroup) Kill(ctx context.Context) error {
	return g.launched.Kill(ctx)
}

// evacuateRoot moves the already materialized payload aside so the run
// supervisor can claim an empty fresh root. Renames stay on one filesystem,
// and the private root has no other owner before the backend is launched.
func evacuateRoot(root string) (string, error) {
	stage, err := os.MkdirTemp(filepath.Dir(root), ".compare-payload-")
	if err != nil {
		return "", fmt.Errorf("create compare payload staging directory: %w", err)
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		_ = os.RemoveAll(stage)
		return "", fmt.Errorf("read compare root: %w", err)
	}
	var moved []string
	for _, entry := range entries {
		src := filepath.Join(root, entry.Name())
		dst := filepath.Join(stage, entry.Name())
		if err := os.Rename(src, dst); err != nil {
			for _, name := range moved {
				_ = os.Rename(filepath.Join(stage, name), filepath.Join(root, name))
			}
			_ = os.RemoveAll(stage)
			return "", fmt.Errorf("stage compare root entry %s: %w", entry.Name(), err)
		}
		moved = append(moved, entry.Name())
	}
	return stage, nil
}

func restoreRoot(stage, root string) error {
	if stage == "" {
		return nil
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return fmt.Errorf("read compare payload staging directory: %w", err)
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stage, entry.Name()), filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("restore compare root entry %s: %w", entry.Name(), err)
		}
	}
	if err := os.Remove(stage); err != nil {
		return fmt.Errorf("remove compare payload staging directory: %w", err)
	}
	return nil
}

func (r *BrowserRunner) Run(ctx context.Context, req LegRequest) (result LegResult, err error) {
	o := r.Options
	if strings.TrimSpace(o.Binary) == "" {
		return result, errors.New("compare browser runner needs an explicit backend binary")
	}
	if !o.Window {
		return result, errors.New("compare browser runner requires --window so a real frontend page is present")
	}
	launchTimeout := o.LaunchTimeout
	if launchTimeout <= 0 {
		launchTimeout = 45 * time.Second
	}
	replayTimeout := o.ReplayTimeout
	if replayTimeout <= 0 {
		replayTimeout = 2 * time.Minute
	}
	instrument := req.Instrument
	if instrument == "" {
		instrument = o.Instrument
	}
	if instrument == "" {
		instrument = "perf"
	}
	if instrument != "perf" && instrument != "none" {
		return result, fmt.Errorf("unsupported compare instrument %q", instrument)
	}
	payloadStage, err := evacuateRoot(req.Root)
	if err != nil {
		return result, err
	}
	manifestPath := filepath.Join(req.Root, "compare-supervisor.json")
	manifest := supervisorManifest{Version: CurrentVersion, Leg: req.Leg, Pair: req.Pair, Instrument: instrument, RunManifest: harnessrun.ManifestPath(req.Root), DataRoot: req.Root, DataDir: req.DataDir, BrowserProfile: req.BrowserProfile, StartedAt: time.Now().UTC(), Status: "starting"}
	writeManifest := func() error { return atomicfile.WriteJSON(manifestPath, manifest) }
	plan := harnessrun.RunPlan{Version: harnessrun.PlanVersion, RunID: fmt.Sprintf("compare-%s%d", req.Leg, req.Pair), Workload: req.Workload.Name, DataRoot: req.Root, Adapter: harnessrun.AdapterCompare, Capsule: req.CapsulePath, Leg: string(req.Leg), Pairs: 1, BaseDir: filepath.Dir(req.Root), Binary: o.Binary, MockProvider: o.MockProvider, Window: true, CDP: o.CDP, SampleMS: o.SampleMs, Meters: append([]string(nil), o.PerfMeters...), PreserveRoot: req.KeepRoot, Ownership: harnessrun.OwnershipFresh}
	retention, err := harnessrun.NewDefaultArtifactRegistry()
	if err != nil {
		_ = restoreRoot(payloadStage, req.Root)
		return result, fmt.Errorf("create compare artifact registry: %w", err)
	}
	runSupervisor, err := harnessrun.NewWithOptions(plan, time.Now().UTC(), harnessrun.SupervisorOptions{Retention: retention})
	if err != nil {
		_ = restoreRoot(payloadStage, req.Root)
		return result, fmt.Errorf("create compare run supervisor: %w", err)
	}
	var launchCleanup func(context.Context) error
	defer func() {
		finishErr := runSupervisor.Finish(context.Background(), err, harnessrun.FailureNone, launchCleanup)
		if finishErr != nil {
			if err == nil {
				err = fmt.Errorf("finish compare run supervisor: %w", finishErr)
			} else {
				err = errors.Join(err, fmt.Errorf("finish compare run supervisor: %w", finishErr))
			}
		}
		m := runSupervisor.Manifest()
		if m.Quarantine != "" {
			manifestPath = filepath.Join(m.Quarantine, filepath.Base(manifestPath))
			result.SupervisorManifest = manifestPath
			manifest.Status = "failed"
			manifest.Error = err.Error()
			if writeErr := writeManifest(); writeErr != nil && err == nil {
				err = fmt.Errorf("write supervisor manifest: %w", writeErr)
			}
		} else if err != nil {
			manifest.Status = "failed"
			manifest.Error = err.Error()
			result.SupervisorManifest = manifestPath
			if writeErr := writeManifest(); writeErr != nil && err == nil {
				err = fmt.Errorf("write supervisor manifest: %w", writeErr)
			}
		} else if !req.KeepRoot {
			result.SupervisorManifest = ""
		}
	}()
	defer func() {
		if payloadStage == "" {
			return
		}
		if restoreErr := restoreRoot(payloadStage, req.Root); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore compare payload: %w", restoreErr)
		}
		payloadStage = ""
	}()
	if err := restoreRoot(payloadStage, req.Root); err != nil {
		return result, fmt.Errorf("restore compare payload: %w", err)
	}
	payloadStage = ""
	result.SupervisorManifest = manifestPath
	if err := writeManifest(); err != nil {
		return result, fmt.Errorf("write supervisor manifest: %w", err)
	}
	if transitionErr := runSupervisor.Transition(harnessrun.StatePreparing, harnessrun.PhasePrepare); transitionErr != nil {
		return result, transitionErr
	}
	launched, err := harnessclient.Launch(ctx, harnessclient.LaunchOptions{Binary: o.Binary, DataRoot: req.Root, MockProvider: o.MockProvider, Window: true, Timeout: launchTimeout, Detach: false, StdoutPath: filepath.Join(req.Root, "compare-stdout.log"), StderrPath: filepath.Join(req.Root, "compare-stderr.log"), MemoryLimitBytes: governor.DefaultCeilingBytes})
	if err != nil {
		launchCleanup = func(context.Context) error { return cleanupLaunchedBackend(launched) }
		manifest.Status = "failed"
		manifest.Error = err.Error()
		if writeErr := writeManifest(); writeErr != nil {
			return result, fmt.Errorf("%w; write supervisor manifest: %v", err, writeErr)
		}
		return result, err
	}
	launchCleanup = func(context.Context) error { return cleanupLaunchedBackend(launched) }
	if registerErr := runSupervisor.RegisterProcessGroup(launchedProcessGroup{launched: launched}); registerErr != nil {
		return result, fmt.Errorf("register compare backend ownership: %w", registerErr)
	}
	launchCleanup = nil
	defer func() {
		if err != nil {
			manifest.Status = "failed"
			manifest.Error = err.Error()
		} else {
			manifest.Status = "done"
		}
		now := time.Now().UTC()
		manifest.FinishedAt = &now
		if writeErr := writeManifest(); err == nil && writeErr != nil {
			err = fmt.Errorf("write supervisor manifest: %w", writeErr)
		}
	}()
	if transitionErr := runSupervisor.Transition(harnessrun.StateReady, harnessrun.PhaseReady); transitionErr != nil {
		return result, transitionErr
	}
	if launched.Bootstrap.DataRoot != "" && !samePath(launched.Bootstrap.DataRoot, req.Root) {
		return result, fmt.Errorf("backend data root identity mismatch: got %s, want %s", launched.Bootstrap.DataRoot, req.Root)
	}
	if strings.TrimSpace(launched.Bootstrap.PageMarker) == "" {
		return result, errors.New("backend bootstrap has no page marker; exact frontend ownership is unavailable")
	}
	client, err := harnessclient.Dial(ctx, launched.Bootstrap, harnessclient.Options{})
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := client.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close harness connection: %w", closeErr)
		}
	}()
	caps, err := client.CheckCapabilities(ctx)
	if err != nil {
		return result, fmt.Errorf("capability handshake: %w", err)
	}
	if err := requireMethod(caps, "HarnessReplayStart"); err != nil {
		return result, err
	}
	if err := requireMethod(caps, "HarnessReplayStatus"); err != nil {
		return result, err
	}
	if err := requireMethod(caps, "HarnessUIQuery"); err != nil {
		return result, err
	}
	if err := requireMethod(caps, "HarnessInfo"); err != nil {
		return result, err
	}
	pageOrigin, err := bootstrapOrigin(launched.Bootstrap.URL)
	if err != nil {
		return result, err
	}
	pageID, err := waitForPage(ctx, client, o.PageID, launched.Bootstrap.PageMarker, pageOrigin)
	if err != nil {
		return result, err
	}
	manifest.PageID = pageID
	manifest.BackendPID = launched.PID
	if err := writeManifest(); err != nil {
		return result, fmt.Errorf("write supervisor manifest: %w", err)
	}

	var cdp *cdpclient.Conn
	var cdpTarget cdpclient.Target
	if strings.TrimSpace(o.CDP) != "" {
		endpoint, parseErr := cdpclient.ParseEndpoint(o.CDP)
		if parseErr != nil {
			return result, parseErr
		}
		cdp, cdpTarget, err = cdpclient.Attach(ctx, endpoint, launched.Bootstrap.URL)
		if err != nil {
			return result, fmt.Errorf("attach owned browser page: %w", err)
		}
		defer func() {
			if closeErr := cdp.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close owned browser page: %w", closeErr)
			}
		}()
		manifest.CDPTargetID = cdpTarget.ID
		if err := writeManifest(); err != nil {
			return result, fmt.Errorf("write supervisor manifest: %w", err)
		}
	}
	if err := checkRequiredCapabilities(caps, req.Workload.RequiredCapabilities, pageID, cdp != nil, req.EventCount > 0, instrument); err != nil {
		return result, err
	}
	if transitionErr := runSupervisor.Transition(harnessrun.StateRunning, harnessrun.PhaseAction); transitionErr != nil {
		return result, transitionErr
	}
	if err := openLogicalPanes(ctx, client, pageID, req.Panes); err != nil {
		return result, err
	}
	perfStarted := false
	perfCaptured := false
	if instrument == "perf" {
		if err := requireMethod(caps, "HarnessPerfStart"); err != nil {
			return result, fmt.Errorf("perf instrument unavailable: %w", err)
		}
		spec := perfStartSpec(pageID, o)
		if _, err := client.Call(ctx, "HarnessPerfStart", spec); err != nil {
			return result, fmt.Errorf("start perf capture: %w", err)
		}
		perfStarted = true
	}
	stopPerf := func(stopCtx context.Context) error {
		if !perfStarted {
			return nil
		}
		perfStarted = false
		_, stopErr := client.Call(stopCtx, "HarnessPerfStop")
		return stopErr
	}
	defer func() {
		if stopErr := stopPerf(context.Background()); err == nil && stopErr != nil {
			err = fmt.Errorf("stop perf capture during cleanup: %w", stopErr)
		}
	}()
	if req.EventStreamPath != "" && req.EventCount > 0 {
		if _, err := client.Call(ctx, "HarnessReplayStart", req.EventStreamPath, map[string]any{"speed": 1}); err != nil {
			return result, fmt.Errorf("start capsule replay: %w", err)
		}
		if err := waitForReplay(ctx, client, replayTimeout); err != nil {
			return result, err
		}
	}
	if perfStarted {
		settle := 1 * time.Second
		if o.SampleMs > 0 {
			settle = time.Duration(o.SampleMs) * time.Millisecond
			if settle < 250*time.Millisecond {
				settle = 250 * time.Millisecond
			}
		}
		if settle < 300*time.Millisecond {
			settle = 300 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(settle):
		}
		raw, stopErr := client.Call(ctx, "HarnessPerfStop")
		if stopErr != nil {
			return result, fmt.Errorf("stop perf capture: %w", stopErr)
		}
		perfStarted = false
		perfCaptured = true
		var perfStatus struct {
			FrontendError string `json:"frontendError"`
			MonitorsError string `json:"monitorsError"`
		}
		if err := json.Unmarshal(raw, &perfStatus); err != nil {
			return result, fmt.Errorf("decode perf capture: %w", err)
		}
		if perfStatus.FrontendError != "" {
			return result, fmt.Errorf("frontend perf capture failed: %s", perfStatus.FrontendError)
		}
		if perfStatus.MonitorsError != "" {
			return result, fmt.Errorf("frontend monitor capture failed: %s", perfStatus.MonitorsError)
		}
		result.Metrics = metricsFromPerf(raw)
		if len(result.Metrics) == 0 {
			return result, errors.New("perf capture produced no measurable samples")
		}
	}
	text, err := semanticText(ctx, client, pageID, req.Panes, cdp)
	if err != nil {
		return result, err
	}
	result.SemanticText = text
	result.AssetDigest = resolvedAssetDigest(o.AssetDigest, caps)
	result.BuildDigest = o.BuildDigest
	if result.BuildDigest == "" {
		result.BuildDigest = caps.Build.Stamp
		if result.BuildDigest == "" {
			result.BuildDigest = caps.Build.Version
		}
	}
	result.Capabilities = runnerCapabilities(caps, pageID, cdp != nil)
	if perfCaptured {
		result.Capabilities = append(result.Capabilities, "perf")
	}
	if req.EventCount > 0 {
		result.Capabilities = append(result.Capabilities, "replay")
	}
	result.PageID = pageID
	result.CDPTargetID = manifest.CDPTargetID
	result.SupervisorManifest = manifestPath
	return result, nil
}

func cleanupLaunchedBackend(launched *harnessclient.Launched) error {
	if launched == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := launched.Terminate(ctx); err != nil {
		return errors.Join(err, launched.Kill(ctx))
	}
	return nil
}

func resolvedAssetDigest(option string, caps harnessclient.HarnessCapabilities) string {
	if option != "" {
		return option
	}
	if caps.Assets.Digest != "" {
		return caps.Assets.Digest
	}
	return "unknown"
}
