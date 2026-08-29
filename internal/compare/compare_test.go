package compare

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harnessclient"

	_ "modernc.org/sqlite"
)

func TestResolvedAssetDigestUsesDigestNotFreshness(t *testing.T) {
	caps := harnessclient.HarnessCapabilities{Assets: harnessclient.HarnessAssetsCapabilities{Freshness: "match", Digest: "sha256:assets"}}
	if got := resolvedAssetDigest("", caps); got != "sha256:assets" {
		t.Fatalf("resolved digest = %q, want sha256:assets", got)
	}
	caps.Assets.Digest = ""
	if got := resolvedAssetDigest("", caps); got != "unknown" {
		t.Fatalf("unknown digest = %q, want unknown", got)
	}
	if got := resolvedAssetDigest("explicit", caps); got != "explicit" {
		t.Fatalf("explicit digest = %q, want explicit", got)
	}
}

func TestPublishCapsuleRestoresPreviousOutputOnPublishFailure(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "capsule")
	if err := os.Mkdir(output, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := publishCapsule(filepath.Join(root, "missing-stage"), output, true)
	if err == nil {
		t.Fatal("publish unexpectedly succeeded with missing stage")
	}
	got, readErr := os.ReadFile(filepath.Join(output, "manifest.json"))
	if readErr != nil || string(got) != "old" {
		t.Fatalf("previous capsule = %q, %v", got, readErr)
	}
}

func TestHashFileReportsReadErrors(t *testing.T) {
	if _, err := hashFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("hashFile accepted a missing file")
	}
}

func makeSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "agent-overflow")
	if err := os.MkdirAll(filepath.Join(data, "replay", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(data, "agent-overflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE threads (id TEXT PRIMARY KEY, session_ref TEXT, pending_fork_session_ref TEXT, pending_fork_resume_at TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO threads VALUES ('thread-1','real-session','pending-session','cursor')`,
		`CREATE TABLE ui_state (scope TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(scope,key))`,
		`INSERT INTO ui_state VALUES ('client:x','paneLayout','{"version":3,"focusedPaneId":"left","panes":[{"paneId":"left","kind":"thread","threadId":"thread-1","widthPx":480}]}',1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(data, "attachments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "attachments", "one.txt"), []byte("attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "fixtures", "fixture.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, line := range map[string]string{
		filepath.Join(data, "replay", "nested", "b.jsonl"): `{"ts":2,"threadId":"thread-1","kind":"second","data":{"n":2}}` + "\n",
		filepath.Join(data, "replay", "a.jsonl"):           `{"ts":1,"threadId":"thread-1","kind":"first","data":{"n":1}}` + "\n",
	} {
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func TestPrepareBuildsVerifiedImmutableCapsule(t *testing.T) {
	source := makeSource(t)
	out := filepath.Join(t.TempDir(), "capsule")
	t.Cleanup(func() {
		_ = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	c, err := Prepare(PrepareOptions{Source: source, Output: out, AssetDigest: "assets-v1", BuildDigest: "build-v1", Workload: WorkloadShape{Name: "stream", RequiredCapabilities: []string{"browser"}}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CapsuleSHA256 != c.CapsuleSHA256 || loaded.Events.Count != 2 {
		t.Fatalf("loaded = %+v", loaded)
	}
	if got := []int{loaded.Events.Events[0].Ordinal, loaded.Events.Events[1].Ordinal}; !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("ordinals = %v", got)
	}
	if got := []string{loaded.Events.Events[0].Kind, loaded.Events.Events[1].Kind}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("event order = %v", got)
	}
	if len(loaded.Panes) != 1 || loaded.Panes[0].ThreadID != "thread-1" {
		t.Fatalf("panes = %+v", loaded.Panes)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(out, "db.snapshot")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var refs int
	if err := db.QueryRow(`SELECT count(*) FROM threads WHERE session_ref IS NOT NULL OR pending_fork_session_ref IS NOT NULL OR pending_fork_resume_at <> ''`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Fatalf("session refs survived: %d", refs)
	}
	if mode := mustMode(t, filepath.Join(out, "manifest.json")); mode&0o222 != 0 {
		t.Fatalf("manifest is writable: %o", mode)
	}
	if _, err := os.Stat(filepath.Join(out, "attachments", "one.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareWritesEmptyEventStreamWhenSourceHasNoReplayDirectory(t *testing.T) {
	source := makeSource(t)
	if err := os.RemoveAll(filepath.Join(source, "replay")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "capsule")
	t.Cleanup(func() {
		_ = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	capsule, err := Prepare(PrepareOptions{Source: source, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if capsule.Events.Count != 0 || capsule.Events.SHA256 == "" {
		t.Fatalf("empty events = %+v", capsule.Events)
	}
	if _, err := os.Stat(filepath.Join(out, "events.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

type recordingRunner struct{ requests []LegRequest }

func (r *recordingRunner) Run(_ context.Context, req LegRequest) (LegResult, error) {
	r.requests = append(r.requests, req)
	return LegResult{Metrics: map[string]float64{"latency": float64(req.Pair)}, SemanticText: "same semantic output", Capabilities: []string{"browser"}, AssetDigest: "assets-v1", BuildDigest: "build-v1"}, nil
}

type variantRunner struct{ identity bool }

func (r variantRunner) Run(_ context.Context, req LegRequest) (LegResult, error) {
	asset, build := "assets-v1", "build-v1"
	if r.identity && req.Leg == LegB {
		asset, build = "assets-b", "build-b"
	}
	semantic := "same semantic output"
	if !r.identity && req.Leg == LegB {
		semantic = "different semantic output"
	}
	return LegResult{Metrics: map[string]float64{"latency": 1}, SemanticText: semantic, Capabilities: []string{"browser"}, AssetDigest: asset, BuildDigest: build}, nil
}

func makeCompareCapsule(t *testing.T) string {
	t.Helper()
	source := makeSource(t)
	out := filepath.Join(t.TempDir(), "capsule")
	t.Cleanup(func() {
		_ = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	if _, err := Prepare(PrepareOptions{Source: source, Output: out, AssetDigest: "unknown", BuildDigest: "unknown", Workload: WorkloadShape{RequiredCapabilities: []string{"browser"}}}); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(out, "manifest.json")
}

func TestRunRejectsSemanticMismatchAsIncomplete(t *testing.T) {
	report, err := Run(context.Background(), RunOptions{Capsule: makeCompareCapsule(t), BaseDir: t.TempDir(), Pairs: 1}, variantRunner{}, nil)
	if err == nil || report.Complete || len(report.Pairs) != 1 || report.Semantic.Equal {
		t.Fatalf("semantic mismatch = report=%+v err=%v", report, err)
	}
}

type replacingRunner struct{}

func (replacingRunner) Run(_ context.Context, req LegRequest) (LegResult, error) {
	if err := os.RemoveAll(req.Root); err != nil {
		return LegResult{}, err
	}
	if err := os.Mkdir(req.Root, 0o700); err != nil {
		return LegResult{}, err
	}
	return LegResult{Metrics: map[string]float64{"latency": 1}, SemanticText: "same semantic output", Capabilities: []string{"browser"}, AssetDigest: "unknown", BuildDigest: "unknown"}, nil
}

func TestRunRefusesRemovingReplacedDisposableRoot(t *testing.T) {
	base := t.TempDir()
	report, err := Run(context.Background(), RunOptions{Capsule: makeCompareCapsule(t), BaseDir: base, Pairs: 1}, replacingRunner{}, nil)
	if err == nil || report.Complete {
		t.Fatalf("replaced disposable root = report=%+v err=%v", report, err)
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil || len(entries) != 2 {
		t.Fatalf("replacement roots = %v, err=%v", entries, readErr)
	}
}

func TestRunPathValidationRejectsSymlinkedBaseParent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := validateRunPaths(filepath.Join(root, "capsule", "manifest.json"), filepath.Join(link, "runs"), ""); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked base parent accepted: %v", err)
	}
}

func TestRunPathValidationDefaultsMissingBaseDirToOSTemp(t *testing.T) {
	base, report, err := validateRunPaths(filepath.Join(t.TempDir(), "capsule", "manifest.json"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(base) != filepath.Clean(os.TempDir()) || report != "" {
		t.Fatalf("default compare paths = base %q report %q, want OS temp and no report", base, report)
	}
}

func TestRunPathValidationRefusesRealAppRoot(t *testing.T) {
	root, err := appdirs.Root()
	if err != nil {
		t.Fatal(err)
	}
	if err := refuseRealComparePath(root, "base directory"); err == nil {
		t.Fatal("real app data root accepted as compare path")
	}
}

func TestCopyOneRefusesCopyFailureWithoutPartialDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	destination := filepath.Join(parent, "link", "result")
	if err := copyOne(source, destination, 0o600); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("copy through symlink accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "result")); !os.IsNotExist(err) {
		t.Fatalf("copy failure created outside destination: %v", err)
	}
	finalTarget := filepath.Join(parent, "final")
	outsideFile := filepath.Join(outside, "existing")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, finalTarget); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := copyOne(source, finalTarget, 0o600); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("copy over final symlink accepted: %v", err)
	}
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "keep" {
		t.Fatalf("final symlink target changed: %q, %v", got, err)
	}
}

func TestRunRejectsABIdentityMismatchAsIncomplete(t *testing.T) {
	report, err := Run(context.Background(), RunOptions{Capsule: makeCompareCapsule(t), BaseDir: t.TempDir(), Pairs: 1}, variantRunner{identity: true}, nil)
	if err == nil || report.Complete || len(report.Pairs) != 0 || report.Legs[0].Status != "invalid" || report.Legs[1].Status != "invalid" {
		t.Fatalf("identity mismatch = report=%+v err=%v", report, err)
	}
}

func TestRunUsesPairedABOrderAndDeterministicBootstrap(t *testing.T) {
	source := makeSource(t)
	out := filepath.Join(t.TempDir(), "capsule")
	t.Cleanup(func() {
		_ = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	if _, err := Prepare(PrepareOptions{Source: source, Output: out, AssetDigest: "assets-v1", BuildDigest: "build-v1", Workload: WorkloadShape{Name: "stream", RequiredCapabilities: []string{"browser"}}}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	var partial []Report
	report, err := Run(context.Background(), RunOptions{Capsule: filepath.Join(out, "manifest.json"), BaseDir: t.TempDir(), Pairs: 8, Bootstrap: 200, KeepRoots: false}, runner, func(r Report) error { partial = append(partial, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 17 {
		t.Fatalf("partial writes = %d, want 17", len(partial))
	}
	if len(runner.requests) != 16 {
		t.Fatalf("requests = %d, want 16", len(runner.requests))
	}
	for i, req := range runner.requests {
		wantLeg := LegA
		if i%2 == 1 {
			wantLeg = LegB
		}
		if req.Leg != wantLeg || req.Pair != (i/2)+1 {
			t.Fatalf("request %d = %s%d", i, req.Leg, req.Pair)
		}
		if req.Instrument != "perf" {
			t.Fatalf("request %d instrument = %q, want perf", i, req.Instrument)
		}
	}
	if len(report.Bootstrap) != 1 || report.Bootstrap[0].Metric != "latency" {
		t.Fatalf("bootstrap = %+v", report.Bootstrap)
	}
	if !report.Complete || len(report.Pairs) != 8 {
		t.Fatalf("report = %+v", report)
	}
}

func TestRunCleanInstrumentDoesNotArmPerfByContract(t *testing.T) {
	source := makeSource(t)
	out := filepath.Join(t.TempDir(), "capsule")
	t.Cleanup(func() {
		_ = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	if _, err := Prepare(PrepareOptions{Source: source, Output: out, AssetDigest: "assets-v1", BuildDigest: "build-v1"}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	if _, err := Run(context.Background(), RunOptions{Capsule: filepath.Join(out, "manifest.json"), BaseDir: t.TempDir(), Instrument: "none"}, runner, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 2 || runner.requests[0].Instrument != "none" || runner.requests[1].Instrument != "none" {
		t.Fatalf("clean requests = %+v", runner.requests)
	}
}

func TestBootstrapOriginRequiresAnAbsoluteBackendURL(t *testing.T) {
	got, err := bootstrapOrigin("http://127.0.0.1:4321/?page=marker")
	if err != nil || got != "http://127.0.0.1:4321" {
		t.Fatalf("origin = %q, err = %v", got, err)
	}
	if _, err := bootstrapOrigin("/relative"); err == nil {
		t.Fatal("accepted relative backend URL")
	}
	if !sameOrigin("http://localhost:4321", "http://localhost:4321/?page=x") {
		t.Fatal("same origin rejected query/path")
	}
	if sameOrigin("http://localhost:4321", "https://localhost:4321") {
		t.Fatal("different schemes accepted as same origin")
	}
}

func TestRequiredCapabilitiesPreflightHonorsCleanInstrument(t *testing.T) {
	caps := harnessclient.HarnessCapabilities{Methods: []string{"HarnessPerfStart", "HarnessPerfStop"}, Queries: []string{"viewport"}}
	if err := checkRequiredCapabilities(caps, []string{"browser", "query:viewport"}, "page-1", false, false, "none"); err != nil {
		t.Fatal(err)
	}
	if err := checkRequiredCapabilities(caps, []string{"perf"}, "page-1", false, false, "none"); err == nil {
		t.Fatal("clean instrument satisfied perf capability")
	}
	if err := checkRequiredCapabilities(caps, []string{"perf"}, "page-1", false, false, "perf"); err != nil {
		t.Fatal(err)
	}
}

func TestMultiPageSelectionOnlyUsesOwnedPages(t *testing.T) {
	pages := []harnessclient.HarnessPageIdentity{
		{PageID: "foreign", Marker: "other", Origin: "http://127.0.0.1:4321"},
		{PageID: "owned-b", Marker: "marker", Origin: "http://127.0.0.1:4321"},
		{PageID: "owned-a", Marker: "marker", Origin: "http://127.0.0.1:4321"},
	}
	got, ok := selectOwnedPage(pages, "", "marker", "http://127.0.0.1:4321")
	if !ok || got != "owned-a" {
		t.Fatalf("selected page = %q, ok = %v", got, ok)
	}
	if got, ok := selectOwnedPage(pages, "foreign", "marker", "http://127.0.0.1:4321"); ok || got != "" {
		t.Fatalf("selected foreign page = %q, ok = %v", got, ok)
	}
	if got, ok := selectOwnedPage(pages, "owned-b", "marker", "http://127.0.0.1:4321"); !ok || got != "owned-b" {
		t.Fatalf("selected explicit page = %q, ok = %v", got, ok)
	}
}

func TestPerfStartSpecCarriesResolvedPageIdentity(t *testing.T) {
	spec := perfStartSpec("page-a", BrowserRunnerOptions{SampleMs: 500, PerfMeters: []string{"frames"}})
	if spec["pageId"] != "page-a" || spec["sampleMs"] != 500 {
		t.Fatalf("perf spec = %#v", spec)
	}
	if got, ok := spec["meters"].([]string); !ok || len(got) != 1 || got[0] != "frames" {
		t.Fatalf("perf meters = %#v", spec["meters"])
	}
}

func TestCompareTextReportsFirstDifference(t *testing.T) {
	gate := CompareText("alpha\nbeta", "alpha\ngamma")
	if gate.Equal || gate.FirstDifference == nil || gate.FirstDifference.Line != 2 || gate.FirstDifference.Column != 1 {
		t.Fatalf("gate = %+v", gate)
	}
	if CompareText("x", "x").FirstDifference != nil {
		t.Fatal("equal text has a difference")
	}
}

func TestSemanticTextFromViewportIgnoresDynamicViewportFields(t *testing.T) {
	got, err := semanticTextFromViewport(json.RawMessage(`{"settled":false,"sinceMutationMs":2,"panes":[{"rows":[{"textHead":"alpha"},{"textHead":"beta"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "alpha\nbeta\n" {
		t.Fatalf("semantic text = %q", got)
	}
	if _, err := semanticTextFromViewport(json.RawMessage(`{"panes":[{"rows":[{"textHead":"alpha…"}]}]}`)); err == nil {
		t.Fatal("accepted truncated semantic text")
	}
}

func TestMetricsFromPerfReadsFoldedSeries(t *testing.T) {
	raw := json.RawMessage(`{"samples":2,"backend":{"heapBytes":{"count":2,"max":42},"rssBytes":{"count":2,"max":84},"goroutines":{"count":2,"max":7}},"frontend":{"frames":{"fps":60,"p95Ms":18,"maxMs":30},"busy":{"p95Ms":4,"maxMs":9}}}`)
	got := metricsFromPerf(raw)
	for name, want := range map[string]float64{"backend.heapBytes": 42, "backend.rssBytes": 84, "backend.goroutines": 7, "frames.fps": 60, "frames.p95Ms": 18, "busy.maxMs": 9} {
		if got[name] != want {
			t.Fatalf("metric %s = %v, want %v", name, got[name], want)
		}
	}
}

func TestMetricsFromPerfKeepsMeasuredZeroAndDropsUnselectedZero(t *testing.T) {
	raw := json.RawMessage(`{"frontend":{"meters":["frames"],"unavailableMeters":[],"frames":{"fps":0,"p95Ms":0,"maxMs":0},"busy":{"ticks":0,"p95Ms":0,"maxMs":0}}}`)
	got := metricsFromPerf(raw)
	if _, ok := got["frames.fps"]; !ok {
		t.Fatal("measured zero frame metric was dropped")
	}
	if _, ok := got["busy.maxMs"]; ok {
		t.Fatal("unselected busy metric survived")
	}
}

func TestMetricSetMismatchInvalidatesPair(t *testing.T) {
	if got := metricSetMismatch(map[string]float64{"rss": 1}, map[string]float64{}); got == "" {
		t.Fatal("metric mismatch was not detected")
	}
	if got := metricSetMismatch(map[string]float64{"rss": 1}, map[string]float64{"rss": 2}); got != "" {
		t.Fatalf("matching metric sets reported mismatch: %s", got)
	}
}

func TestRunMarksReportIncompleteWhenFinalWriteFails(t *testing.T) {
	source := makeSource(t)
	out := filepath.Join(t.TempDir(), "capsule")
	t.Cleanup(func() {
		_ = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	if _, err := Prepare(PrepareOptions{Source: source, Output: out, AssetDigest: "assets-v1", BuildDigest: "build-v1"}); err != nil {
		t.Fatal(err)
	}
	var calls int
	report, err := Run(context.Background(), RunOptions{Capsule: filepath.Join(out, "manifest.json"), Pairs: 1}, &recordingRunner{}, func(r Report) error {
		calls++
		if r.FinishedAt != nil {
			return os.ErrPermission
		}
		return nil
	})
	if err == nil || report.Complete || calls == 0 {
		t.Fatalf("final write failure = report=%+v err=%v calls=%d", report, err, calls)
	}
}

func TestReadLogicalPanesRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ui_state (scope TEXT, key TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ui_state VALUES ('client', 'paneLayout', '{malformed')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := readLogicalPanes(path); err == nil {
		t.Fatal("malformed pane layout was silently ignored")
	}
}

func TestLoadRejectsSymlinkedAsset(t *testing.T) {
	source := makeSource(t)
	out := filepath.Join(t.TempDir(), "capsule")
	t.Cleanup(func() {
		_ = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	if _, err := Prepare(PrepareOptions{Source: source, Output: out}); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(out, "manifest.json")
	if err := filepath.Walk(out, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chmod(path, 0o700)
	}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(out, "attachments", "one.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Load(manifest); err == nil {
		t.Fatal("Load accepted a symlinked capsule asset")
	}
}

func TestPathWithinCoversDescendants(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "nested", "copy")
	if !pathWithin(child, root) || pathWithin(root+"-other", root) {
		t.Fatalf("pathWithin containment is incorrect: child=%v sibling=%v", pathWithin(child, root), pathWithin(root+"-other", root))
	}
}

func TestEvacuateAndRestoreRootPreservesMaterializedPayload(t *testing.T) {
	root := filepath.Join(t.TempDir(), "leg")
	if err := os.MkdirAll(filepath.Join(root, "agent-overflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "agent-overflow", "agent-overflow.db")
	if err := os.WriteFile(payload, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage, err := evacuateRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("root after evacuation = %v, err=%v", entries, err)
	}
	if err := restoreRoot(stage, root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(payload)
	if err != nil || string(got) != "db" {
		t.Fatalf("restored payload = %q, err=%v", got, err)
	}
}
