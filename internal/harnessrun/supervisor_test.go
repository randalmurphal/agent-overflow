package harnessrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPlan(root string, ownership Ownership) RunPlan {
	return RunPlan{Version: PlanVersion, RunID: "run-1", Workload: "stream", DataRoot: root, Ownership: ownership}
}

func TestDecodePlanIsVersionedAndStrict(t *testing.T) {
	valid := `{"version":1,"runId":"r","workload":"w","dataRoot":"/tmp/r","pageId":"page-1","ownership":"fresh","ceiling":{"maxDurationMs":100}}`
	plan, err := DecodePlan([]byte(valid))
	if err != nil {
		t.Fatalf("valid plan: %v", err)
	}
	if plan.PageID != "page-1" {
		t.Fatalf("pageId = %q, want page-1", plan.PageID)
	}
	for _, input := range []string{
		`{"version":1,"runId":"r","workload":"w","dataRoot":"/tmp/r","ownership":"fresh","extra":1}`,
		valid + " garbage",
		valid + ` {"version":1}`,
		`{"version":1,"version":1,"runId":"r","workload":"w","dataRoot":"/tmp/r","ownership":"fresh"}`,
		`{"version":2,"runId":"r","workload":"w","dataRoot":"/tmp/r","ownership":"fresh"}`,
	} {
		if _, err := DecodePlan([]byte(input)); err == nil {
			t.Errorf("DecodePlan(%q) accepted invalid input", input)
		}
	}
}

func TestRunPlanRejectsWhitespacePageID(t *testing.T) {
	p := testPlan(t.TempDir(), OwnershipFresh)
	p.PageID = " page-1"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "pageId") {
		t.Fatalf("pageId validation = %v", err)
	}
}

func TestRunPlanAdaptersValidateTypedInputs(t *testing.T) {
	base := func(adapter Adapter) RunPlan {
		return RunPlan{Version: PlanVersion, RunID: "r", Workload: "w", DataRoot: "/tmp/run", Ownership: OwnershipFresh, Adapter: adapter}
	}
	profile := base(AdapterProfile)
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "borrowed ownership") {
		t.Fatalf("fresh profile validation = %v", err)
	}
	profile.Ownership = OwnershipBorrowed
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "thread and scenario") {
		t.Fatalf("profile validation = %v", err)
	}
	profile.Thread = "thread-1"
	profile.Scenario = "paced-stream"
	profile.CDP = "http://127.0.0.1:9226"
	if err := profile.Validate(); err != nil {
		t.Fatalf("profile names and CDP endpoints are not filesystem paths: %v", err)
	}
	compare := base(AdapterCompare)
	if err := compare.Validate(); err == nil || !strings.Contains(err.Error(), "capsule") {
		t.Fatalf("compare validation = %v", err)
	}
	unknown := base("shell")
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "unknown run adapter") {
		t.Fatalf("unknown validation = %v", err)
	}
	functional := base(AdapterFunctional)
	functional.Scenario = "relative-flow.json"
	functional.Output = filepath.Join(ArtifactRoot(functional), "report.json")
	if err := functional.Validate(); err == nil || !strings.Contains(err.Error(), "scenario must be absolute") {
		t.Fatalf("functional scenario validation = %v", err)
	}
}

func TestApplyDefaultsKeepsManagedBenchReportAfterFreshRootRemoval(t *testing.T) {
	plan := RunPlan{Version: PlanVersion, RunID: "bench", Workload: "burst-stream", DataRoot: "/tmp/bench", Ownership: OwnershipFresh, Adapter: AdapterBench}
	plan = ApplyDefaults(plan)
	want := filepath.Join(ArtifactRoot(plan), "bench-report.json")
	if plan.Output != want {
		t.Fatalf("managed bench output = %q, want %q", plan.Output, want)
	}
}

func TestApplyDefaultsCompareCarriesWindowAndInstrument(t *testing.T) {
	plan := RunPlan{Version: PlanVersion, RunID: "compare", Workload: "replay", DataRoot: "/tmp/compare", Ownership: OwnershipFresh, Adapter: AdapterCompare, Capsule: "/tmp/capsule/manifest.json"}
	plan = ApplyDefaults(plan)
	if !plan.Window || plan.Instrument != "perf" {
		t.Fatalf("compare defaults = window %v instrument %q", plan.Window, plan.Instrument)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("compare defaults invalid: %v", err)
	}
}

func TestComparePlanRejectsUnknownInstrument(t *testing.T) {
	plan := RunPlan{Version: PlanVersion, RunID: "compare", Workload: "replay", DataRoot: "/tmp/compare", Ownership: OwnershipFresh, Adapter: AdapterCompare, Capsule: "/tmp/capsule/manifest.json", Window: true, Instrument: "trace"}
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "instrument") {
		t.Fatalf("unknown compare instrument validation = %v", err)
	}
}

func TestRunPlanRejectsMillisecondDurationOverflow(t *testing.T) {
	for name, mutate := range map[string]func(*RunPlan){
		"ceiling":        func(p *RunPlan) { p.Ceiling.MaxDurationMS = maxDurationMS + 1 },
		"bench duration": func(p *RunPlan) { p.DurationMS = maxDurationMS + 1 },
		"bench sample":   func(p *RunPlan) { p.SampleMS = int(maxDurationMS + 1) },
		"profile timeout": func(p *RunPlan) {
			p.Adapter, p.Thread, p.Scenario, p.TimeoutMS = AdapterProfile, "thread-1", "scenario", maxDurationMS+1
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPlan(t.TempDir(), OwnershipFresh)
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("overflow validation = %v", err)
			}
		})
	}
}

func TestQuarantineRefusesSymlinkParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	external := filepath.Join(base, "external")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, root+QuarantineSuffix); err != nil {
		t.Fatal(err)
	}
	s, err := New(testPlan(root, OwnershipFresh), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), errors.New("boom"), FailureAction, nil); err == nil {
		t.Fatal("symlink quarantine parent was accepted")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("failed root was removed: %v", err)
	}
	if entries, err := os.ReadDir(external); err != nil || len(entries) != 0 {
		t.Fatalf("external quarantine target changed: %v, %d entries", err, len(entries))
	}
}

func TestSupervisorTransitionsRejectSkips(t *testing.T) {
	s, err := New(testPlan(filepath.Join(t.TempDir(), "root"), OwnershipFresh), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(StateRunning, PhaseAction); err == nil {
		t.Fatal("created -> running was accepted")
	}
	for _, step := range []struct {
		state State
		phase Phase
	}{
		{StatePreparing, PhasePrepare}, {StateReady, PhaseReady}, {StateRunning, PhaseAction}, {StateStopping, PhaseTeardown},
	} {
		if err := s.Transition(step.state, step.phase); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Complete(); err != nil {
		t.Fatal(err)
	}
	if got := s.Manifest().State; got != StateSucceeded {
		t.Fatalf("state = %q", got)
	}
}

func TestFreshSupervisorCanPreserveSuccessfulRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	plan := testPlan(root, OwnershipFresh)
	plan.PreserveRoot = true
	s, err := New(plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		state State
		phase Phase
	}{{StatePreparing, PhasePrepare}, {StateReady, PhaseReady}, {StateRunning, PhaseAction}} {
		if err := s.Transition(step.state, step.phase); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(context.Background(), nil, FailureNone, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("preserved root missing: %v", err)
	}
}

func TestLeaseConcurrentAndStale(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	now := time.Now().UTC()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var leases []*Lease
	var errs []error
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := AcquireLeaseWithOptions(root, "r", now, LeaseOptions{StaleAfter: time.Hour})
			mu.Lock()
			defer mu.Unlock()
			leases = append(leases, l)
			errs = append(errs, err)
		}()
	}
	wg.Wait()
	var won int
	for i := range errs {
		if errs[i] == nil && leases[i] != nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("concurrent lease winners = %d, errs=%v", won, errs)
	}
	for _, l := range leases {
		if l != nil {
			_ = l.Release()
		}
	}
	old := LeaseRecord{Token: "old", RunID: "old-run", PID: 1, AcquiredAt: now.Add(-2 * time.Hour)}
	data, _ := json.Marshal(old)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLeaseWithOptions(root, "new", now, LeaseOptions{StaleAfter: time.Hour}); err != nil {
		t.Fatalf("stale lease was not recovered: %v", err)
	}
}

func TestFreshSupervisorsReserveBeforeManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	plans := []RunPlan{testPlan(root, OwnershipFresh), testPlan(root, OwnershipFresh)}
	plans[1].RunID = "run-2"
	results := make(chan struct {
		sup *Supervisor
		err error
	}, len(plans))
	var wg sync.WaitGroup
	for _, plan := range plans {
		plan := plan
		wg.Add(1)
		go func() {
			defer wg.Done()
			sup, err := New(plan, time.Now().UTC())
			results <- struct {
				sup *Supervisor
				err error
			}{sup: sup, err: err}
		}()
	}
	wg.Wait()
	close(results)
	var winner *Supervisor
	var failures int
	for result := range results {
		if result.err == nil {
			if winner != nil {
				t.Fatal("two fresh supervisors acquired one root")
			}
			winner = result.sup
			continue
		}
		failures++
		if !strings.Contains(result.err.Error(), "leased") {
			t.Fatalf("losing fresh supervisor error = %v", result.err)
		}
	}
	if winner == nil || failures != 1 {
		t.Fatalf("fresh supervisor results winner=%v failures=%d", winner != nil, failures)
	}
	if err := winner.Finish(context.Background(), errors.New("test cleanup"), FailureAction, nil); err == nil {
		t.Fatal("winner cleanup unexpectedly succeeded")
	}
}

func TestArtifactChecksumAndExternalDurability(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	p := testPlan(root, OwnershipFresh)
	destination := filepath.Join(ArtifactRoot(p), "report.json")
	p.Artifacts = []ArtifactPlan{{Name: "report", Path: "out/report.json", Destination: destination, Required: true}}
	s, err := New(p, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "out/report.json")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("durable report")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtifact("report"); err != nil {
		t.Fatal(err)
	}
	m := s.Manifest()
	if len(m.Artifacts) != 1 || m.Artifacts[0].Status != ArtifactDurable {
		t.Fatalf("artifact = %+v", m.Artifacts)
	}
	h := sha256.Sum256(content)
	if m.Artifacts[0].SHA256 != hex.EncodeToString(h[:]) {
		t.Fatalf("checksum = %q", m.Artifacts[0].SHA256)
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != string(content) {
		t.Fatalf("external artifact = %q, %v", got, err)
	}
	if err := s.Transition(StatePreparing, PhasePrepare); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(StateReady, PhaseReady); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(StateRunning, PhaseAction); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), nil, FailureNone, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful fresh root remains: %v", err)
	}
}

func TestFinishRefusesReplacedFreshRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	s, err := New(testPlan(root, OwnershipFresh), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	lease := s.lease.Record()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LeasePath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), errors.New("failed"), FailureAction, nil); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("replaced root finish = %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("replacement root was removed: %v", err)
	}
}

func TestPlanRejectsOutputOutsideArtifactRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	p := testPlan(root, OwnershipFresh)
	p.Output = filepath.Join(t.TempDir(), "real-output.json")
	if err := p.Validate(); err == nil {
		t.Fatal("accepted output outside supervisor artifact root")
	}
	p.Output = filepath.Join(ArtifactRoot(p), "report.json")
	p.Artifacts = []ArtifactPlan{{Name: "report", Path: "report.json", Destination: filepath.Join(t.TempDir(), "outside.json")}}
	if err := p.Validate(); err == nil {
		t.Fatal("accepted artifact destination outside supervisor artifact root")
	}
}

func TestRequiredArtifactBlocksSuccessAndQuarantines(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	p := testPlan(root, OwnershipFresh)
	p.Artifacts = []ArtifactPlan{{Name: "missing", Path: "report.json", Required: true}}
	s, err := New(p, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), nil, FailureNone, nil); err == nil {
		t.Fatal("missing required artifact allowed success")
	}
	if _, err := os.Stat(filepath.Join(root+QuarantineSuffix, "run-1", ManifestFileName)); err != nil {
		t.Fatalf("missing artifact was not quarantined: %v", err)
	}
}

func TestFreshArtifactWithoutDestinationIsNotReportedDurable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	p := testPlan(root, OwnershipFresh)
	p.Artifacts = []ArtifactPlan{{Name: "report", Path: "report.json", Required: true}}
	s, err := New(p, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.json"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(StatePreparing, PhasePrepare); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(StateReady, PhaseReady); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(StateRunning, PhaseAction); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtifact("report"); err == nil {
		t.Fatal("fresh artifact without a destination was reported durable")
	}
	if err := s.Finish(context.Background(), nil, FailureNone, nil); err == nil {
		t.Fatal("fresh artifact without a destination allowed success")
	}
	if _, err := os.Stat(filepath.Join(root+QuarantineSuffix, "run-1", ManifestFileName)); err != nil {
		t.Fatalf("artifact failure was not quarantined: %v", err)
	}
	manifest, err := ReadManifest(filepath.Join(root+QuarantineSuffix, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Artifacts) != 1 || manifest.Artifacts[0].Status == ArtifactDurable {
		t.Fatalf("artifact status = %+v, want non-durable", manifest.Artifacts)
	}
}

func TestFailureQuarantinesFreshButNeverBorrowed(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh")
	s, err := New(testPlan(fresh, OwnershipFresh), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), errors.New("action broke"), FailureAction, nil); err == nil || !strings.Contains(err.Error(), "action broke") {
		t.Fatalf("finish error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh+QuarantineSuffix, "run-1", ManifestFileName)); err != nil {
		t.Fatalf("quarantined manifest: %v", err)
	}
	borrowed := t.TempDir()
	if err := os.WriteFile(filepath.Join(borrowed, "keep"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := testPlan(borrowed, OwnershipBorrowed)
	p.RunID = "borrowed"
	bs, err := New(p, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.Finish(context.Background(), errors.New("failed"), FailureAction, nil); err == nil {
		t.Fatal("borrowed failure was accepted as success")
	}
	if _, err := os.Stat(filepath.Join(borrowed, "keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(borrowed + QuarantineSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("borrowed root quarantined: %v", err)
	}
}

func TestFinishPreservesPrimaryOverCleanup(t *testing.T) {
	s, err := New(testPlan(filepath.Join(t.TempDir(), "root"), OwnershipFresh), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	primary := errors.New("provider disconnected")
	cleanup := errors.New("close page failed")
	err = s.Finish(context.Background(), primary, FailureProviderDisconnect, func(context.Context) error { return cleanup })
	if !errors.Is(err, primary) {
		t.Fatalf("primary error lost: %v", err)
	}
	if !strings.Contains(err.Error(), cleanup.Error()) {
		t.Fatalf("cleanup error lost: %v", err)
	}
}

func TestCleanupContextIsBounded(t *testing.T) {
	s, err := New(testPlan(filepath.Join(t.TempDir(), "root"), OwnershipFresh), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	seenDeadline := false
	_ = s.Finish(context.Background(), errors.New("failed"), FailureAction, func(ctx context.Context) error { _, seenDeadline = ctx.Deadline(); return nil })
	if !seenDeadline {
		t.Fatal("cleanup context had no deadline")
	}
}

type testGroup struct{ stopped, killed bool }

func (g *testGroup) Record() ProcessGroupRecord {
	return ProcessGroupRecord{ID: "group-1", Owned: true, PID: 7, GroupPID: 7}
}
func (g *testGroup) Terminate(context.Context) error {
	g.stopped = true
	return errors.New("term refused")
}
func (g *testGroup) Kill(context.Context) error { g.killed = true; return nil }

func TestProcessGroupOwnershipAndEscalation(t *testing.T) {
	s, err := New(testPlan(filepath.Join(t.TempDir(), "root"), OwnershipFresh), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	g := new(testGroup)
	if err := s.RegisterProcessGroup(g); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterProcessGroup(g); err == nil {
		t.Fatal("duplicate process group accepted")
	}
	if err := s.Finish(context.Background(), errors.New("run failed"), FailureAction, nil); err == nil {
		t.Fatal("failed run returned nil")
	}
	if !g.stopped || !g.killed {
		t.Fatalf("group stop/kill = %v/%v", g.stopped, g.killed)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.root), filepath.Base(s.root))); err != nil {
		t.Fatalf("cleanup failure removed owned root: %v", err)
	}
	if _, err := os.Stat(LeasePath(s.root)); err != nil {
		t.Fatalf("cleanup failure released lease: %v", err)
	}
}

func TestLiveOwnerCannotLoseAnOldLease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	now := time.Now().UTC()
	lease, err := AcquireLease(root, "old", now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	record := lease.Record()
	record.HeartbeatAt = now.Add(-48 * time.Hour)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LeasePath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLeaseWithOptions(root, "new", now, LeaseOptions{StaleAfter: time.Hour}); err == nil {
		t.Fatal("live owner lease was recovered solely because its heartbeat was old")
	}
}

func TestSupervisorSnapshotsDoNotExposeMutablePlanState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	plan := testPlan(root, OwnershipFresh)
	plan.Artifacts = []ArtifactPlan{{Name: "report", Path: "report.json", Destination: filepath.Join(ArtifactRoot(plan), "report.json")}}
	s, err := New(plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	plan.Artifacts[0].Path = "outside"
	snapshot := s.Manifest()
	snapshot.Plan.Artifacts[0].Path = "outside-again"
	if got := s.Manifest().Plan.Artifacts[0].Path; got != "report.json" {
		t.Fatalf("supervisor plan path was externally mutated to %q", got)
	}
	if err := s.Finish(context.Background(), errors.New("stop"), FailureAction, nil); err == nil {
		t.Fatal("failed run returned nil")
	}
}
