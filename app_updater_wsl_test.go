package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/wsldistro"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

// --- fixtures ---------------------------------------------------------------

// eventRecorder captures App.emit traffic the way the production transport bus
// would see it, and lets a test block until a channel it cares about arrives.
type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
	waits  []chan recordedEvent
}

type recordedEvent struct {
	channel string
	data    any
}

func newEventRecorder(a *App) *eventRecorder {
	r := &eventRecorder{}
	a.testEmitHook = func(name string, data any) {
		r.mu.Lock()
		ev := recordedEvent{channel: name, data: data}
		r.events = append(r.events, ev)
		waits := r.waits
		r.waits = nil
		r.mu.Unlock()
		for _, w := range waits {
			w <- ev
		}
	}
	return r
}

// mark returns a cursor into the recorded stream. Any test that runs more than
// one download/handoff cycle must take one and use awaitAfter: await scans from
// the beginning and would hand back the PREVIOUS cycle's event immediately,
// letting the test race ahead of the work it thinks it waited for.
func (r *eventRecorder) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// await blocks until an event on channel arrives (including one already
// recorded), so tests never race the download goroutine.
func (r *eventRecorder) await(t *testing.T, channel string, timeout time.Duration) any {
	t.Helper()
	return r.awaitAfter(t, channel, 0, timeout)
}

// awaitAfter is await restricted to events recorded at or past the cursor.
func (r *eventRecorder) awaitAfter(t *testing.T, channel string, from int, timeout time.Duration) any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		r.mu.Lock()
		for _, ev := range r.events[min(from, len(r.events)):] {
			if ev.channel == channel {
				r.mu.Unlock()
				return ev.data
			}
		}
		next := make(chan recordedEvent, 1)
		r.waits = append(r.waits, next)
		r.mu.Unlock()

		select {
		case <-next:
			// Re-scan: the woken event may not be the one we want.
		case <-deadline:
			t.Fatalf("timed out waiting for %s past cursor %d; saw %v", channel, from, r.channels())
			return nil
		}
	}
}

// refute fails if an event on channel has arrived by the time window elapses.
func (r *eventRecorder) refute(t *testing.T, channel string, window time.Duration) {
	t.Helper()
	time.Sleep(window)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.channel == channel {
			t.Fatalf("unexpected %s event: %+v", channel, ev.data)
		}
	}
}

func (r *eventRecorder) channels() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.channel)
	}
	return out
}

// wslTestDeadlines injects the two install deadlines rather than sleeping
// through the production 10s / 3m ones. noDeadlines pushes both far enough out
// that they can never fire, which is what every test NOT about a deadline
// wants: an unexpectedly firing timer would otherwise unwind the install under
// the test's feet and turn a real defect into a confusing timing failure.
type wslTestDeadlines struct{ ack, backstop time.Duration }

var noDeadlines = wslTestDeadlines{ack: time.Hour, backstop: time.Hour}

// newWSLTestApp wires the full production chain (real github.Provider →
// targetableProvider → verifiedProvider → real updater.Updater) against the
// mock release server, configured exactly as initWSLUpdaterIn configures it:
// platform "wsl", arch "amd64", WindowNone, and the WSL host.
func newWSLTestApp(t *testing.T, srv *httptest.Server, current string, deadlines wslTestDeadlines) (*App, *eventRecorder, *wslUpdateMode) {
	t.Helper()
	mode := &wslUpdateMode{
		stagingDir:      filepath.Join(t.TempDir(), selfupdate.StagingDirName),
		markerDir:       t.TempDir(),
		ackTimeout:      deadlines.ack,
		backstopTimeout: deadlines.backstop,
	}
	a := &App{updater: appUpdaterState{wsl: mode}}
	rec := newEventRecorder(a)

	gh, err := github.New(github.Config{
		Repository:    testRepo,
		ChecksumAsset: "SHASUMS256",
		BaseURL:       srv.URL,
		HTTPClient:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	req := updater.CheckRequest{CurrentVersion: current, Platform: wslUpdaterPlatform, Arch: "amd64"}
	tp := newTargetableProvider(gh, testRepo, "SHASUMS256", req, srv.Client())
	tp.baseURL = srv.URL
	u := updater.New(wslUpdaterHost{app: a})
	if err := u.Init(updater.Config{
		CurrentVersion: req.CurrentVersion,
		Platform:       req.Platform,
		Arch:           req.Arch,
		Providers:      []updater.Provider{verifiedProvider{inner: tp}},
		Window:         updater.WindowNone,
	}); err != nil {
		t.Fatalf("init updater: %v", err)
	}
	a.updater.handle = u
	a.updater.provider = tp
	return a, rec, mode
}

// wslReleases is the mock feed the flow tests run against: one release newer
// than 0.0.1 that ships the wsl asset plus a checksum sidecar.
func wslReleases() []relSpec {
	return []relSpec{{tag: "v0.0.8", name: "WSL release", withPlatform: true, withWSL: true, withChecksum: true}}
}

func readMarker(t *testing.T, dir string) *selfupdate.Marker {
	t.Helper()
	m, err := selfupdate.LoadMarker(dir)
	if err != nil {
		t.Fatalf("load marker: %v", err)
	}
	return m
}

func (a *App) busySnapshot() bool {
	a.updater.mu.Lock()
	defer a.updater.mu.Unlock()
	return a.updater.busy
}

// --- initWSLUpdater gating --------------------------------------------------

func TestInitWSLUpdaterSkipsDevBuild(t *testing.T) {
	// An unstamped build has no release to compare against, so the whole
	// feature must stay off — including the marker reconciliation, which would
	// otherwise accuse the launcher of losing an update it never staged.
	t.Setenv(wsldistro.AppDataEnv, t.TempDir())
	a := &App{}
	initWSLUpdaterIn(a, "dev", t.TempDir())
	if a.updater.handle != nil || a.updater.wsl != nil {
		t.Fatalf("dev build must leave the updater unconfigured: updater=%v wslUpdate=%v", a.updater.handle, a.updater.wsl)
	}
}

func TestInitWSLUpdaterRequiresLauncherEnv(t *testing.T) {
	// No AGENT_OVERFLOW_WIN_APPDATA means nobody spawned us through the
	// launcher: there is no Windows-side staging dir and no listener for the
	// install directive, so downloading would produce an artifact nothing can
	// ever install.
	t.Setenv(wsldistro.AppDataEnv, "")
	a := &App{}
	initWSLUpdaterIn(a, "0.0.10", t.TempDir())
	if a.updater.handle != nil || a.updater.wsl != nil {
		t.Fatal("WSL self-update must stay unsupported without the launcher-injected AppData path")
	}
}

func TestInitWSLUpdaterRequiresMarkerDir(t *testing.T) {
	// Without a resolvable app data dir there is nowhere to record the install
	// intent, so the next boot could never tell a successful swap from a lost
	// one. Refuse the whole feature rather than run it blind.
	t.Setenv(wsldistro.AppDataEnv, t.TempDir())
	a := &App{}
	initWSLUpdaterIn(a, "0.0.10", "")
	if a.updater.handle != nil || a.updater.wsl != nil {
		t.Fatal("WSL self-update must stay unsupported without a marker dir")
	}
}

func TestInitWSLUpdaterConfiguresWSLTarget(t *testing.T) {
	appData := t.TempDir()
	t.Setenv(wsldistro.AppDataEnv, appData)
	markerDir := t.TempDir()

	a := &App{}
	initWSLUpdaterIn(a, "0.0.10", markerDir)

	if a.updater.handle == nil || a.updater.provider == nil || a.updater.wsl == nil {
		t.Fatalf("expected a configured WSL updater, got updater=%v provider=%v mode=%v",
			a.updater.handle, a.updater.provider, a.updater.wsl)
	}
	// Platform MUST be "wsl": left empty, Init defaults to runtime.GOOS and
	// this backend would silently target the linux desktop assets it cannot
	// install, while an empty platform AND arch would let the matcher pick the
	// SHASUMS256 sidecar as the artifact.
	if a.updater.provider.req.Platform != "wsl" {
		t.Fatalf("provider platform = %q, want wsl", a.updater.provider.req.Platform)
	}
	if a.updater.provider.req.CurrentVersion != "0.0.10" {
		t.Fatalf("provider CurrentVersion = %q, want 0.0.10", a.updater.provider.req.CurrentVersion)
	}
	if a.updater.handle.CurrentVersion() != "0.0.10" {
		t.Fatalf("updater CurrentVersion = %q, want 0.0.10", a.updater.handle.CurrentVersion())
	}
	wantStaging := filepath.Join(appData, "agent-overflow", selfupdate.StagingDirName)
	if a.updater.wsl.stagingDir != wantStaging {
		t.Fatalf("stagingDir = %q, want %q", a.updater.wsl.stagingDir, wantStaging)
	}
	if a.updater.wsl.markerDir != markerDir {
		t.Fatalf("markerDir = %q, want %q", a.updater.wsl.markerDir, markerDir)
	}
	if a.updater.wsl.ackTimeout != wslInstallACKTimeout {
		t.Fatalf("ackTimeout = %v, want %v", a.updater.wsl.ackTimeout, wslInstallACKTimeout)
	}
}

// --- full flow --------------------------------------------------------------

// TestWSLUpdateFlowStagesAndHandsOff walks the whole WSL path: check, download
// + verify, copy across the boundary, hand off to the launcher, and the
// launcher's acknowledgement.
func TestWSLUpdateFlowStagesAndHandsOff(t *testing.T) {
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)

	avail, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !avail.Available || avail.LatestVersion != "0.0.8" {
		t.Fatalf("availability = %+v, want available 0.0.8", avail)
	}

	if err := a.DownloadUpdate(""); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	ready := rec.await(t, "updater:ready", 20*time.Second)

	// updater:ready carries the same *updater.Release the desktop bridge
	// forwards for EventUpdateReady — the frontend keys only off the channel
	// today, but the shapes must not diverge.
	rel, ok := ready.(*updater.Release)
	if !ok {
		t.Fatalf("updater:ready payload = %T, want *updater.Release", ready)
	}
	if rel.Version != "0.0.8" || rel.Artifact.Filename != wslAssetName {
		t.Fatalf("ready release = %+v, want 0.0.8 / %s", rel, wslAssetName)
	}

	// The staged file must exist on the Windows side with the verified bytes.
	stagedPath := filepath.Join(mode.stagingDir, wslAssetName)
	got, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged artifact: %v", err)
	}
	if digest := sha256.Sum256(got); hex.EncodeToString(digest[:]) != wslAssetDigestHex {
		t.Fatalf("staged bytes digest = %x, want %s", digest, wslAssetDigestHex)
	}
	if a.busySnapshot() {
		t.Fatal("the download fence must be released once the artifact is staged")
	}

	// Hand off.
	if err := a.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	directive, ok := rec.await(t, selfupdate.ChannelInstall, 5*time.Second).(selfupdate.InstallDirective)
	if !ok {
		t.Fatal("install directive has the wrong payload type")
	}
	if err := directive.Validate(); err != nil {
		t.Fatalf("emitted directive does not validate: %v", err)
	}
	if directive.Filename != wslAssetName || directive.Version != "0.0.8" || directive.SHA256 != wslAssetDigestHex {
		t.Fatalf("directive = %+v, want %s / 0.0.8 / %s", directive, wslAssetName, wslAssetDigestHex)
	}
	if !a.busySnapshot() {
		t.Fatal("the fence must stay held while the launcher has the directive")
	}
	marker := readMarker(t, mode.markerDir)
	if marker == nil || marker.ExpectedVersion != "0.0.8" || marker.PriorVersion != "0.0.1" {
		t.Fatalf("marker = %+v, want expected 0.0.8 / prior 0.0.1", marker)
	}

	// The launcher acknowledges: the ACK timer is cancelled, the marker stays
	// (the next boot needs it), and the fence stays held because the process is
	// about to be killed.
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(proceeding): %v", err)
	}
	if readMarker(t, mode.markerDir) == nil {
		t.Fatal("a proceeding install must leave the marker in place for the next boot")
	}
	if !a.busySnapshot() {
		t.Fatal("a proceeding install must keep the fence held")
	}
	rec.refute(t, "updater:error", 100*time.Millisecond)

	// The acknowledgement does not END the install, it moves it into its second
	// phase: still in flight, now acknowledged, under the silence backstop
	// instead of the ACK deadline. The launcher's promise to kill this process
	// is not a fact until it happens.
	a.updater.mu.Lock()
	inflight, acked, timer := a.updater.install, a.updater.installAcked, a.updater.installTimer
	a.updater.mu.Unlock()
	if inflight == nil || !acked || timer == nil {
		t.Fatalf("acknowledged install must stay in flight under the backstop: install=%v acked=%v timer=%v",
			inflight, acked, timer)
	}
}

func TestWSLReadyEventLandsAfterTheDownloadFenceDrops(t *testing.T) {
	// A client that acts on updater:ready the instant it arrives calls
	// RestartToUpdate, which refuses while the download fence is held. The
	// terminal event must therefore be emitted AFTER the fence drops — a UI
	// that is merely fast must never be told "an update is already being
	// installed" by the goroutine that just finished installing it.
	//
	// Restarting from inside the emit hook is the sharpest form of that
	// assertion and is safe here precisely because of the property under test:
	// the ready event is raised from the download goroutine's deferred block,
	// which holds no App lock.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, _ := newWSLTestApp(t, srv, "0.0.1", noDeadlines)

	record := a.testEmitHook
	restarted := make(chan error, 1)
	a.testEmitHook = func(name string, data any) {
		record(name, data)
		if name != "updater:ready" {
			return
		}
		select {
		case restarted <- a.RestartToUpdate():
		default:
		}
	}

	if _, err := a.CheckForUpdate(); err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if err := a.DownloadUpdate(""); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	select {
	case err := <-restarted:
		if err != nil {
			t.Fatalf("restarting the instant updater:ready landed = %v, want success", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("updater:ready never arrived; saw %v", rec.channels())
	}
	// And the directive really went out, so this is proving a working restart
	// rather than a silently swallowed one.
	rec.await(t, selfupdate.ChannelInstall, 5*time.Second)
}

func TestWSLInstallFailedReportUnwinds(t *testing.T) {
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	stageForTest(t, a, rec)

	if err := a.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	rec.await(t, selfupdate.ChannelInstall, 5*time.Second)

	if err := a.ReportUpdateInstallStatus(selfupdate.StatusFailed, "0.0.8", "swap denied by antivirus"); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(failed): %v", err)
	}

	info, ok := rec.await(t, "updater:error", 5*time.Second).(updater.ErrorInfo)
	if !ok {
		t.Fatal("updater:error has the wrong payload type")
	}
	if info.Stage != updater.StageInstall || info.Message != "swap denied by antivirus" {
		t.Fatalf("error info = %+v, want install stage carrying the launcher's message", info)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("a failed install must clear the marker, got %+v", m)
	}
	if a.busySnapshot() {
		t.Fatal("a failed install must release the fence so the user can retry")
	}
	// The artifact is genuinely still staged on the Windows side, so the App's
	// record of it stays — a retry has something to hand over, and the next
	// download sweeps it first anyway.
	a.updater.mu.Lock()
	staged := a.updater.staged
	a.updater.mu.Unlock()
	if staged == nil {
		t.Fatal("a failed handoff must not forget the still-staged artifact")
	}
}

func TestWSLInstallACKTimeoutUnwinds(t *testing.T) {
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", wslTestDeadlines{ack: 20 * time.Millisecond, backstop: time.Hour})
	stageForTest(t, a, rec)

	if err := a.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	info, ok := rec.await(t, "updater:error", 5*time.Second).(updater.ErrorInfo)
	if !ok {
		t.Fatal("updater:error has the wrong payload type")
	}
	if !strings.Contains(info.Message, "did not respond") {
		t.Fatalf("timeout message = %q, want it to name the unresponsive launcher", info.Message)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("a timed-out install must clear the marker, got %+v", m)
	}
	if a.busySnapshot() {
		t.Fatal("a timed-out install must release the fence")
	}

	// A report that arrives after the deadline is refused, and changes nothing.
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); !errors.Is(err, ErrNoInstallInFlight) {
		t.Fatalf("late report = %v, want ErrNoInstallInFlight", err)
	}
	if a.busySnapshot() {
		t.Fatal("a late report must not re-claim the fence")
	}
}

// --- post-acknowledgement backstop ------------------------------------------

// handOffForTest drives the flow to "the launcher has the directive", which is
// where every acknowledgement-phase test starts.
func handOffForTest(t *testing.T, a *App, rec *eventRecorder) {
	t.Helper()
	stageForTest(t, a, rec)
	from := rec.mark()
	if err := a.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	rec.awaitAfter(t, selfupdate.ChannelInstall, from, 5*time.Second)
}

func TestWSLInstallBackstopUnwindsAfterSilentLauncher(t *testing.T) {
	// The gap this closes: the launcher acknowledges, then hits an install
	// error AND its "failed" report never lands (the bridge died in exactly
	// that window). It stays alive on the old version; without the backstop
	// this backend would hold a.updater.busy and the marker forever — no retry
	// short of restarting the app, plus a spurious "didn't apply" next boot.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", wslTestDeadlines{ack: time.Hour, backstop: 20 * time.Millisecond})
	handOffForTest(t, a, rec)

	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(proceeding): %v", err)
	}
	// ... and then silence.

	info, ok := rec.await(t, "updater:error", 5*time.Second).(updater.ErrorInfo)
	if !ok {
		t.Fatal("updater:error has the wrong payload type")
	}
	if !strings.Contains(info.Message, "went silent") || !strings.Contains(info.Message, "0.0.1") {
		t.Fatalf("backstop message = %q, want it to name the silence and the version still running", info.Message)
	}
	if info.Stage != updater.StageInstall {
		t.Fatalf("error stage = %q, want %q", info.Stage, updater.StageInstall)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("marker = %+v, want cleared — nothing is going to swap", m)
	}
	if a.busySnapshot() {
		t.Fatal("the backstop must release the fence so the user can retry")
	}
	a.updater.mu.Lock()
	inflight, acked, timer := a.updater.install, a.updater.installAcked, a.updater.installTimer
	a.updater.mu.Unlock()
	if inflight != nil || acked || timer != nil {
		t.Fatalf("backstop must return the install state to rest: install=%v acked=%v timer=%v", inflight, acked, timer)
	}
}

func TestWSLInstallFailedReportAfterAckCancelsBackstop(t *testing.T) {
	// The ordinary way the acknowledged phase ends without a swap: the launcher
	// accepted the directive, then failed, and said so. That report must unwind
	// immediately AND disarm the backstop, or the backstop would later fire
	// against a settled install.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", wslTestDeadlines{ack: time.Hour, backstop: 50 * time.Millisecond})
	handOffForTest(t, a, rec)

	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(proceeding): %v", err)
	}
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusFailed, "0.0.8", "swap helper refused"); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(failed) after ack: %v", err)
	}

	info := rec.await(t, "updater:error", 5*time.Second).(updater.ErrorInfo)
	if info.Message != "swap helper refused" {
		t.Fatalf("error message = %q, want the launcher's own reason", info.Message)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("marker = %+v, want cleared", m)
	}
	if a.busySnapshot() {
		t.Fatal("a failure after the acknowledgement must release the fence")
	}
	// Well past the backstop: it was disarmed, so no second unwind and no
	// second event.
	time.Sleep(150 * time.Millisecond)
	if n := countEvents(rec, "updater:error"); n != 1 {
		t.Fatalf("updater:error count = %d, want exactly 1 (the backstop must have been disarmed)", n)
	}
}

func TestWSLInstallLateFailedAfterBackstopIsRefused(t *testing.T) {
	// The launcher's report finally gets through after the backstop already
	// gave up. Nothing is in flight, so it is refused idempotently: no second
	// error event, and no state to corrupt.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", wslTestDeadlines{ack: time.Hour, backstop: 20 * time.Millisecond})
	handOffForTest(t, a, rec)
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(proceeding): %v", err)
	}
	rec.await(t, "updater:error", 5*time.Second)

	if err := a.ReportUpdateInstallStatus(selfupdate.StatusFailed, "0.0.8", "late news"); !errors.Is(err, ErrNoInstallInFlight) {
		t.Fatalf("late failed report = %v, want ErrNoInstallInFlight", err)
	}
	if n := countEvents(rec, "updater:error"); n != 1 {
		t.Fatalf("updater:error count = %d, want exactly 1", n)
	}
	if a.busySnapshot() {
		t.Fatal("a late report must not re-claim the fence")
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("marker = %+v, want still cleared", m)
	}
}

func TestWSLInstallDuplicateProceedingDoesNotExtendTheBackstop(t *testing.T) {
	// A looping or chatty launcher must not be able to push the deadline out
	// forever — that would restore the very deadlock the backstop exists to
	// prevent. The duplicate is accepted (idempotent) but re-arms nothing.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, _ := newWSLTestApp(t, srv, "0.0.1", wslTestDeadlines{ack: time.Hour, backstop: 150 * time.Millisecond})
	handOffForTest(t, a, rec)

	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("first proceeding: %v", err)
	}
	a.updater.mu.Lock()
	first := a.updater.installTimer
	a.updater.mu.Unlock()

	for i := 0; i < 3; i++ {
		time.Sleep(30 * time.Millisecond)
		if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
			t.Fatalf("duplicate proceeding %d: %v", i, err)
		}
	}
	a.updater.mu.Lock()
	still := a.updater.installTimer
	a.updater.mu.Unlock()
	if still != first {
		t.Fatal("a duplicate acknowledgement re-armed the deadline; a chatty launcher could hold the fence forever")
	}

	// The original deadline still lands on schedule.
	rec.await(t, "updater:error", 5*time.Second)
	if a.busySnapshot() {
		t.Fatal("the backstop must still have released the fence")
	}
}

func TestWSLInstallResequencesAfterBackstop(t *testing.T) {
	// End to end for the recovery the backstop exists to enable: hand off,
	// acknowledge, go silent, get unwound — then download and hand off again,
	// with no interference from the dead generation's state or timer.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", wslTestDeadlines{ack: time.Hour, backstop: 20 * time.Millisecond})
	handOffForTest(t, a, rec)
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(proceeding): %v", err)
	}
	rec.await(t, "updater:error", 5*time.Second)

	a.updater.mu.Lock()
	deadGen := a.updater.installGen
	a.updater.mu.Unlock()

	// A fresh cycle: re-check, re-download (which sweeps and re-stages), and
	// hand off again. Give this one an unreachable backstop so the assertions
	// below describe the new sequence, not another expiry.
	mode.backstopTimeout = time.Hour
	stageForTest(t, a, rec)
	from := rec.mark()
	if err := a.RestartToUpdate(); err != nil {
		t.Fatalf("second RestartToUpdate: %v", err)
	}
	rec.awaitAfter(t, selfupdate.ChannelInstall, from, 5*time.Second)

	a.updater.mu.Lock()
	gen, inflight, acked := a.updater.installGen, a.updater.install, a.updater.installAcked
	a.updater.mu.Unlock()
	if gen == deadGen {
		t.Fatalf("install generation = %d, want a fresh one past the abandoned %d", gen, deadGen)
	}
	if inflight == nil || acked {
		t.Fatalf("a fresh handoff must start unacknowledged and in flight: install=%v acked=%v", inflight, acked)
	}
	if !a.busySnapshot() {
		t.Fatal("the second handoff must hold the fence again")
	}
	if m := readMarker(t, mode.markerDir); m == nil || m.ExpectedVersion != "0.0.8" {
		t.Fatalf("marker = %+v, want a freshly written 0.0.8", m)
	}

	// And the new sequence settles on its own terms: exactly one error event
	// from the whole test (the abandoned first attempt), none from the second.
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("second proceeding: %v", err)
	}
	if n := countEvents(rec, "updater:error"); n != 1 {
		t.Fatalf("updater:error count = %d, want exactly 1 (only the abandoned first attempt)", n)
	}
	if readMarker(t, mode.markerDir) == nil {
		t.Fatal("the second acknowledgement must leave its marker for the next boot")
	}
}

func TestWSLInstallStaleAckDeadlineCannotUnwindAnAcknowledgedInstall(t *testing.T) {
	// The race: the ACK deadline fires just as the launcher's "proceeding"
	// report wins a.updater.mu. Stop can no longer help — the fired callback is
	// past stopping, merely parked on the lock — so the only thing keeping it
	// from unwinding the acknowledged install is its generation now being
	// stale (armWSLInstallDeadlineLocked bumps on every arm). Without that, it
	// would clear the marker and emit an error while the launcher continues
	// the swap it was just told to proceed with. Simulate the parked callback
	// by driving the deadline path with the pre-acknowledgement generation.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	handOffForTest(t, a, rec)

	a.updater.mu.Lock()
	ackGen := a.updater.installGen
	a.updater.mu.Unlock()

	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(proceeding): %v", err)
	}

	// The stale ACK callback finally gets the lock — it must stand down.
	a.failWSLInstall(ackGen, "the Windows launcher did not respond")

	rec.refute(t, "updater:error", 100*time.Millisecond)
	if m := readMarker(t, mode.markerDir); m == nil || m.ExpectedVersion != "0.0.8" {
		t.Fatalf("marker = %+v, want intact — the launcher is mid-swap and the next boot needs it", m)
	}
	if !a.busySnapshot() {
		t.Fatal("the fence must stay held for the acknowledged install")
	}
	a.updater.mu.Lock()
	inflight, acked, timer := a.updater.install, a.updater.installAcked, a.updater.installTimer
	a.updater.mu.Unlock()
	if inflight == nil || !acked || timer == nil {
		t.Fatalf("acknowledged install must stay in flight under its backstop: install=%v acked=%v timer=%v",
			inflight, acked, timer)
	}
}

func TestWSLInstallUnwindDropsTheMarkerBeforeTheFenceLifts(t *testing.T) {
	// The defect shape this pins against: an unwind that releases a.updater.busy
	// under the lock but clears the marker after the unlock leaves a window
	// where a waiting RestartToUpdate claims the fence and writes a FRESH
	// marker — which the old unwind's deferred cleanup then deletes, making a
	// silent failure of the new swap undetectable on the next boot. So the
	// invariant is positional: by the time abandonWSLInstallLocked returns,
	// with a.updater.mu still held and the fence just lifted, the marker must
	// already be gone.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	handOffForTest(t, a, rec)

	a.updater.mu.Lock()
	acted := a.abandonWSLInstallLocked(a.updater.installGen)
	marker, err := selfupdate.LoadMarker(mode.markerDir)
	a.updater.mu.Unlock()
	if !acted {
		t.Fatal("abandonWSLInstallLocked must act on the install in flight")
	}
	if err != nil {
		t.Fatalf("load marker: %v", err)
	}
	if marker != nil {
		t.Fatalf("marker = %+v, want already cleared inside the locked unwind", marker)
	}
}

func TestWSLInstallDeadlineStandsDownDuringShutdown(t *testing.T) {
	// The backstop's subject is a process that is supposed to be dying, so it
	// will routinely still be armed while the launcher's quit unwinds. Firing
	// there would delete the marker the next boot needs to judge the swap and
	// emit a terminal error into a bus being torn down.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", wslTestDeadlines{ack: time.Hour, backstop: 20 * time.Millisecond})
	handOffForTest(t, a, rec)
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); err != nil {
		t.Fatalf("ReportUpdateInstallStatus(proceeding): %v", err)
	}
	a.shuttingDown.Store(true)

	rec.refute(t, "updater:error", 200*time.Millisecond)
	if m := readMarker(t, mode.markerDir); m == nil || m.ExpectedVersion != "0.0.8" {
		t.Fatalf("marker = %+v, want it left intact for the next boot", m)
	}
}

// stageForTest drives check + download so a test can start from "an artifact is
// staged and waiting for the restart".
func stageForTest(t *testing.T, a *App, rec *eventRecorder) {
	t.Helper()
	// Cursor first: a test running a second cycle must not be satisfied by the
	// first cycle's readiness and charge ahead while this download still holds
	// the fence.
	from := rec.mark()
	if _, err := a.CheckForUpdate(); err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if err := a.DownloadUpdate(""); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	rec.awaitAfter(t, "updater:ready", from, 20*time.Second)
}

// --- ReportUpdateInstallStatus edge orders ----------------------------------

func TestReportUpdateInstallStatusRejectsUnknownStage(t *testing.T) {
	a := &App{updater: appUpdaterState{wsl: &wslUpdateMode{markerDir: t.TempDir(), stagingDir: t.TempDir()}}}
	if err := a.ReportUpdateInstallStatus("installed", "0.0.8", ""); !errors.Is(err, ErrInvalidInstallStatus) {
		t.Fatalf("unknown stage = %v, want ErrInvalidInstallStatus", err)
	}
}

func TestReportUpdateInstallStatusUnsupportedOffWSL(t *testing.T) {
	a := &App{} // desktop / test build: no WSL mode
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusProceeding, "0.0.8", ""); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("off-WSL report = %v, want ErrUpdatesUnsupported", err)
	}
}

func TestReportUpdateInstallStatusBeforeAnyRestart(t *testing.T) {
	// A launcher that reports without ever having received a directive (a
	// replayed frame, a confused build) must not be able to invent state.
	a := &App{updater: appUpdaterState{wsl: &wslUpdateMode{markerDir: t.TempDir(), stagingDir: t.TempDir()}}}
	for _, stage := range []string{selfupdate.StatusProceeding, selfupdate.StatusFailed} {
		if err := a.ReportUpdateInstallStatus(stage, "0.0.8", "x"); !errors.Is(err, ErrNoInstallInFlight) {
			t.Fatalf("%s before any restart = %v, want ErrNoInstallInFlight", stage, err)
		}
	}
	if a.busySnapshot() {
		t.Fatal("a report with no install in flight must not touch the fence")
	}
}

func TestReportUpdateInstallStatusRejectsStaleVersion(t *testing.T) {
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	stageForTest(t, a, rec)
	if err := a.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	rec.await(t, selfupdate.ChannelInstall, 5*time.Second)

	err := a.ReportUpdateInstallStatus(selfupdate.StatusFailed, "0.0.7", "stale")
	if !errors.Is(err, ErrInstallVersionMismatch) {
		t.Fatalf("stale-version report = %v, want ErrInstallVersionMismatch", err)
	}
	// Nothing moved: the real install is still in flight.
	if !a.busySnapshot() {
		t.Fatal("a stale report must not release the fence")
	}
	if readMarker(t, mode.markerDir) == nil {
		t.Fatal("a stale report must not clear the marker")
	}
	rec.refute(t, "updater:error", 50*time.Millisecond)
}

func TestReportUpdateInstallStatusDuplicateFailedIsIdempotent(t *testing.T) {
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	stageForTest(t, a, rec)
	if err := a.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	rec.await(t, selfupdate.ChannelInstall, 5*time.Second)

	if err := a.ReportUpdateInstallStatus(selfupdate.StatusFailed, "0.0.8", "first"); err != nil {
		t.Fatalf("first failed report: %v", err)
	}
	rec.await(t, "updater:error", 5*time.Second)

	// The second one finds nothing in flight: refused, no second event, no
	// state change (in particular it must not re-clear a marker a NEW install
	// may have written).
	if err := a.ReportUpdateInstallStatus(selfupdate.StatusFailed, "0.0.8", "second"); !errors.Is(err, ErrNoInstallInFlight) {
		t.Fatalf("duplicate failed report = %v, want ErrNoInstallInFlight", err)
	}
	if n := countEvents(rec, "updater:error"); n != 1 {
		t.Fatalf("updater:error count = %d, want exactly 1", n)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("marker = %+v, want cleared once", m)
	}
}

func countEvents(rec *eventRecorder, channel string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, ev := range rec.events {
		if ev.channel == channel {
			n++
		}
	}
	return n
}

// --- restart guards ---------------------------------------------------------

func TestRestartToUpdateWSLRequiresStagedArtifact(t *testing.T) {
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, _, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	if err := a.RestartToUpdate(); !errors.Is(err, ErrUpdateNotReady) {
		t.Fatalf("RestartToUpdate with nothing staged = %v, want ErrUpdateNotReady", err)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("a refused restart must write no marker, got %+v", m)
	}
}

func TestRestartToUpdateWSLRejectedWhileBusy(t *testing.T) {
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, _ := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	stageForTest(t, a, rec)

	a.updater.mu.Lock()
	a.updater.busy = true
	a.updater.mu.Unlock()
	if err := a.RestartToUpdate(); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("RestartToUpdate while busy = %v, want ErrUpdateBusy", err)
	}
}

// --- release-identity stash sequences ---------------------------------------

func TestUpdaterPendingStashSequences(t *testing.T) {
	srv := newMockGitHub(t, []relSpec{
		{tag: "v0.0.8", name: "Latest", withPlatform: true, withWSL: true, withChecksum: true},
		{tag: "v0.0.6", name: "Older", withPlatform: true, withWSL: true, withChecksum: true},
	}, sumsForWSL)

	t.Run("check stashes the resolved identity", func(t *testing.T) {
		a, _, _ := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
		if _, err := a.CheckForUpdate(); err != nil {
			t.Fatalf("CheckForUpdate: %v", err)
		}
		if a.updater.pending == nil || a.updater.pending.Version != "0.0.8" {
			t.Fatalf("stash = %+v, want 0.0.8", a.updater.pending)
		}
		if a.updater.pending.Artifact.Filename != wslAssetName {
			t.Fatalf("stashed artifact = %q, want the wsl asset", a.updater.pending.Artifact.Filename)
		}
	})

	t.Run("up-to-date clears the stash", func(t *testing.T) {
		// Running the newest release: Check returns nil, so there is nothing a
		// download could install and the stash must not describe one.
		a, _, _ := newWSLTestApp(t, srv, "0.0.8", noDeadlines)
		if _, err := a.CheckForUpdate(); err != nil {
			t.Fatalf("CheckForUpdate: %v", err)
		}
		if a.updater.pending != nil {
			t.Fatalf("stash = %+v, want nil when up to date", a.updater.pending)
		}
	})

	t.Run("by-tag download restashes the resolved tag", func(t *testing.T) {
		a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
		if err := a.DownloadUpdate("v0.0.6"); err != nil {
			t.Fatalf("DownloadUpdate(v0.0.6): %v", err)
		}
		rec.await(t, "updater:ready", 20*time.Second)
		a.updater.mu.Lock()
		pending, staged := a.updater.pending, a.updater.staged
		a.updater.mu.Unlock()
		if pending == nil || pending.Version != "0.0.6" {
			t.Fatalf("stash after by-tag download = %+v, want 0.0.6", pending)
		}
		if staged == nil || staged.Version != "0.0.6" {
			t.Fatalf("staged = %+v, want the rolled-back 0.0.6", staged)
		}
		if _, err := os.Stat(filepath.Join(mode.stagingDir, wslAssetName)); err != nil {
			t.Fatalf("stat staged rollback artifact: %v", err)
		}
	})

	t.Run("re-check after ready leaves the staged identity alone", func(t *testing.T) {
		// A second --connect client can check while an update sits staged. That
		// re-check retargets the provider and rewrites the pending stash — it
		// must not touch what was already copied to the Windows side, or the
		// restart would hand the launcher a directive for a different release
		// than the file on disk.
		a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
		if err := a.DownloadUpdate("v0.0.6"); err != nil {
			t.Fatalf("DownloadUpdate(v0.0.6): %v", err)
		}
		rec.await(t, "updater:ready", 20*time.Second)

		avail, err := a.CheckForUpdate()
		if err != nil {
			t.Fatalf("CheckForUpdate after ready: %v", err)
		}
		if avail.LatestVersion != "0.0.8" {
			t.Fatalf("re-check LatestVersion = %q, want 0.0.8", avail.LatestVersion)
		}
		a.updater.mu.Lock()
		pending, staged := a.updater.pending, a.updater.staged
		a.updater.mu.Unlock()
		if pending == nil || pending.Version != "0.0.8" {
			t.Fatalf("stash after re-check = %+v, want the newly resolved 0.0.8", pending)
		}
		if staged == nil || staged.Version != "0.0.6" {
			t.Fatalf("staged = %+v, want the untouched 0.0.6", staged)
		}

		if err := a.RestartToUpdate(); err != nil {
			t.Fatalf("RestartToUpdate: %v", err)
		}
		directive := rec.await(t, selfupdate.ChannelInstall, 5*time.Second).(selfupdate.InstallDirective)
		if directive.Version != "0.0.6" {
			t.Fatalf("directive version = %q, want the staged 0.0.6", directive.Version)
		}
		if m := readMarker(t, mode.markerDir); m == nil || m.ExpectedVersion != "0.0.6" {
			t.Fatalf("marker = %+v, want expected 0.0.6", m)
		}
	})
}

// --- staging failures -------------------------------------------------------

func TestStageWSLUpdateDigestMismatchEmitsError(t *testing.T) {
	// StageCopy re-verifies what it writes to the Windows side. Feed it an
	// identity whose digest does not describe the downloaded bytes and the
	// staging must fail closed: no file at the destination, no staged state,
	// and a terminal event so the UI does not hang at "installing".
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	stageForTest(t, a, rec)
	if err := selfupdate.SweepStagingDir(mode.stagingDir); err != nil {
		t.Fatalf("clear staging dir: %v", err)
	}

	wrong, err := hex.DecodeString(testValidDigestHex)
	if err != nil {
		t.Fatalf("decode fixture digest: %v", err)
	}
	// stageWSLUpdate hands its terminal event back rather than emitting it, so
	// the download goroutine can drop the busy fence first; drive that here.
	a.stageWSLUpdate(&updater.Release{
		Version:      "0.0.8",
		Artifact:     updater.Artifact{Filename: wslAssetName},
		Verification: &updater.Verification{DigestAlgo: "sha256", Digest: wrong},
	})()

	info := rec.await(t, "updater:error", 5*time.Second).(updater.ErrorInfo)
	if info.Stage != updater.StageInstall {
		t.Fatalf("error stage = %q, want %q", info.Stage, updater.StageInstall)
	}
	if _, err := os.Stat(filepath.Join(mode.stagingDir, wslAssetName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a digest mismatch must leave nothing at the destination (stat err = %v)", err)
	}
	a.updater.mu.Lock()
	staged := a.updater.staged
	a.updater.mu.Unlock()
	if staged != nil {
		t.Fatalf("staged = %+v, want nil after a failed copy", staged)
	}
	// The restart must now refuse rather than hand the launcher a directive for
	// a file that is not there.
	if err := a.RestartToUpdate(); !errors.Is(err, ErrUpdateNotReady) {
		t.Fatalf("RestartToUpdate after a failed stage = %v, want ErrUpdateNotReady", err)
	}
}

func TestStageWSLUpdateWithoutIdentityFailsClosed(t *testing.T) {
	// The download succeeded but no release identity came with it (a stash that
	// went stale, or a caller that skipped the check). Nothing may be staged:
	// the digest is the only integrity gate on the /mnt/c hop.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, rec, mode := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	a.stageWSLUpdate(nil)()

	info := rec.await(t, "updater:error", 5*time.Second).(updater.ErrorInfo)
	if info.Stage != updater.StageInstall {
		t.Fatalf("error stage = %q, want %q", info.Stage, updater.StageInstall)
	}
	if entries, err := os.ReadDir(mode.stagingDir); err == nil && len(entries) > 0 {
		t.Fatalf("staging dir holds %d entries, want none", len(entries))
	}
}

// --- boot reconciliation ----------------------------------------------------

func newReconcileFixture(t *testing.T) (*App, *wslUpdateMode) {
	t.Helper()
	mode := &wslUpdateMode{markerDir: t.TempDir(), stagingDir: filepath.Join(t.TempDir(), selfupdate.StagingDirName)}
	return &App{updater: appUpdaterState{wsl: mode}}, mode
}

// seedStagedArtifact drops a file in the staging dir so the sweep half of the
// boot check has something to prove it cleared.
func seedStagedArtifact(t *testing.T, mode *wslUpdateMode) string {
	t.Helper()
	if err := os.MkdirAll(mode.stagingDir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	path := filepath.Join(mode.stagingDir, wslAssetName)
	if err := os.WriteFile(path, wslAssetBytes, 0o755); err != nil {
		t.Fatalf("seed staged artifact: %v", err)
	}
	return path
}

func TestReconcileWSLUpdateMarkerAbsent(t *testing.T) {
	a, mode := newReconcileFixture(t)
	staged := seedStagedArtifact(t, mode)

	reconcileWSLUpdateMarker(a, "0.0.10", mode)

	if a.updater.applyFailure != "" {
		t.Fatalf("notice = %q, want empty on an ordinary boot", a.updater.applyFailure)
	}
	// No marker means no install was ever handed over, so the boot check has no
	// business deleting anything a download in this session might be mid-stage.
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("an ordinary boot must not sweep the staging dir: %v", err)
	}
}

func TestReconcileWSLUpdateMarkerMatchClearsAndSweeps(t *testing.T) {
	a, mode := newReconcileFixture(t)
	staged := seedStagedArtifact(t, mode)
	if err := selfupdate.SaveMarker(mode.markerDir, selfupdate.Marker{
		ExpectedVersion: "0.0.10", PriorVersion: "0.0.9", StagedAt: time.Now(),
	}); err != nil {
		t.Fatalf("save marker: %v", err)
	}

	reconcileWSLUpdateMarker(a, "0.0.10", mode)

	if a.updater.applyFailure != "" {
		t.Fatalf("notice = %q, want empty when the swap worked", a.updater.applyFailure)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("marker = %+v, want cleared once its question is answered", m)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an applied update must sweep the staged artifact (stat err = %v)", err)
	}
}

func TestReconcileWSLUpdateMarkerMismatchRecordsNotice(t *testing.T) {
	a, mode := newReconcileFixture(t)
	staged := seedStagedArtifact(t, mode)
	if err := selfupdate.SaveMarker(mode.markerDir, selfupdate.Marker{
		ExpectedVersion: "0.0.11", PriorVersion: "0.0.10", StagedAt: time.Now(),
	}); err != nil {
		t.Fatalf("save marker: %v", err)
	}

	reconcileWSLUpdateMarker(a, "0.0.10", mode)

	want := "Update to 0.0.11 didn't apply — still running 0.0.10."
	if a.updater.applyFailure != want {
		t.Fatalf("notice = %q, want %q", a.updater.applyFailure, want)
	}
	if m := readMarker(t, mode.markerDir); m != nil {
		t.Fatalf("marker = %+v, want cleared so the next boot does not re-accuse", m)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed apply must still sweep the dead artifact (stat err = %v)", err)
	}
}

func TestReconcileWSLUpdateMarkerCorruptIsLoudAndSelfHealing(t *testing.T) {
	// An undecodable marker means an install WAS attempted and we cannot tell
	// which. Report the uncertainty rather than swallow it, and clear the file
	// so it does not repeat forever.
	a, mode := newReconcileFixture(t)
	staged := seedStagedArtifact(t, mode)
	if err := os.WriteFile(selfupdate.MarkerPath(mode.markerDir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}

	reconcileWSLUpdateMarker(a, "0.0.10", mode)

	if !strings.Contains(a.updater.applyFailure, "unreadable") {
		t.Fatalf("notice = %q, want it to name the unreadable record", a.updater.applyFailure)
	}
	if _, err := os.Stat(selfupdate.MarkerPath(mode.markerDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a corrupt marker must be cleared (stat err = %v)", err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a corrupt marker must still sweep the staging dir (stat err = %v)", err)
	}
}

func TestInitWSLUpdaterSurfacesApplyFailureThroughCheck(t *testing.T) {
	// End to end for the notice: a marker the running version does not match
	// must reach the frontend through CheckForUpdate, and survive re-checks —
	// re-checking is not evidence that the install worked.
	appData := t.TempDir()
	t.Setenv(wsldistro.AppDataEnv, appData)
	markerDir := t.TempDir()
	if err := selfupdate.SaveMarker(markerDir, selfupdate.Marker{
		ExpectedVersion: "0.0.11", PriorVersion: "0.0.10", StagedAt: time.Now(),
	}); err != nil {
		t.Fatalf("save marker: %v", err)
	}

	a := &App{}
	initWSLUpdaterIn(a, "0.0.10", markerDir)
	if a.updater.handle == nil {
		t.Fatal("expected a configured updater")
	}

	// Take the busy fence so CheckForUpdate answers from state alone. This test
	// is about the notice reaching the wire shape, and the busy path is the one
	// return that touches no network — the production updater here points at
	// the real api.github.com.
	a.updater.mu.Lock()
	a.updater.busy = true
	a.updater.mu.Unlock()

	avail, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if avail.LastApplyFailure != "Update to 0.0.11 didn't apply — still running 0.0.10." {
		t.Fatalf("LastApplyFailure = %q, want the boot notice", avail.LastApplyFailure)
	}

	// A second check reports it again: the notice clears on the next boot whose
	// marker matches, never merely because the user pressed Check.
	again, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("second CheckForUpdate: %v", err)
	}
	if again.LastApplyFailure != avail.LastApplyFailure {
		t.Fatalf("LastApplyFailure = %q on re-check, want it unchanged", again.LastApplyFailure)
	}
}

func TestCheckForUpdateSurfacesCheckFailureAsState(t *testing.T) {
	// An offline check must not eat the boot-detected apply-failure notice.
	// The failed release check comes back as CheckError on the result, not as
	// an RPC error, so the fields the backend knows without the network —
	// Supported, CurrentVersion, LastApplyFailure — still reach the panel.
	srv := newMockGitHub(t, wslReleases(), sumsForWSL)
	a, _, _ := newWSLTestApp(t, srv, "0.0.1", noDeadlines)
	notice := "Update to 0.0.9 didn't apply — still running 0.0.1."
	a.setUpdateApplyFailure(notice)
	srv.Close() // every request now fails: the offline check

	avail, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate = %v, want the failure carried as result state", err)
	}
	if !avail.Supported || avail.CurrentVersion != "0.0.1" {
		t.Fatalf("availability = %+v, want supported with the current version", avail)
	}
	if avail.CheckError == "" {
		t.Fatal("CheckError is empty, want the failed check reported")
	}
	if avail.Available || avail.LatestVersion != "" {
		t.Fatalf("availability = %+v, want no release fields from a failed check", avail)
	}
	if avail.LastApplyFailure != notice {
		t.Fatalf("LastApplyFailure = %q, want %q preserved through the failed check", avail.LastApplyFailure, notice)
	}
}

// --- host adapter -----------------------------------------------------------

func TestWSLUpdaterHostSuppressesFrameworkUpdateReady(t *testing.T) {
	// The framework fires EventUpdateReady when the artifact is staged inside
	// the DISTRO. On this path that is not ready — the launcher cannot see it
	// yet — so the host must swallow it and let stageWSLUpdate emit the real
	// one after the copy lands.
	a := &App{}
	rec := newEventRecorder(a)
	host := wslUpdaterHost{app: a}

	host.Emit(updater.EventUpdateReady, &updater.Release{Version: "0.0.8"})
	if n := countEvents(rec, "updater:ready"); n != 0 {
		t.Fatalf("updater:ready count = %d, want 0 (the framework's is premature here)", n)
	}

	host.Emit(updater.EventDownloadProgress, updater.Progress{Written: 5, Total: 10})
	got := rec.await(t, "updater:progress", time.Second)
	if p, ok := got.(updater.Progress); !ok || p.Written != 5 {
		t.Fatalf("progress payload = %#v, want it forwarded verbatim", got)
	}

	// Unbridged framework events are logged, not forwarded.
	host.Emit(updater.EventNoUpdate)
	if n := len(rec.channels()); n != 1 {
		t.Fatalf("emitted %d events (%v), want only the bridged progress one", n, rec.channels())
	}
}

func TestWSLUpdaterHostOnEventReturnsUsableRemover(t *testing.T) {
	// OnEvent is unreachable with Window=WindowNone and CheckInterval=0 (only
	// openSession calls it, only CheckAndInstall and the periodic loop call
	// that). It must still hand back a callable remover rather than nil so a
	// framework change cannot turn an unused path into a nil dereference.
	remove := wslUpdaterHost{app: &App{}}.OnEvent("anything", func(any) {})
	if remove == nil {
		t.Fatal("OnEvent returned a nil remover")
	}
	remove()
}

func TestWSLUpdaterHostOpenWindowIsNilSafe(t *testing.T) {
	if h := (wslUpdaterHost{app: &App{}}).OpenWindow(updater.WindowOptions{}); h != nil {
		t.Fatalf("OpenWindow = %v, want nil — there is no display server in the distro", h)
	}
}

// --- directive wire shape ---------------------------------------------------

func TestInstallDirectiveMarshalsForTheLauncher(t *testing.T) {
	// The launcher decodes this off the transport event ring, so the JSON keys
	// are contract. Guard the shape here rather than only in selfupdate's own
	// tests, since this is the side that produces it.
	raw, err := json.Marshal(selfupdate.InstallDirective{
		Filename: wslAssetName, SHA256: wslAssetDigestHex, Version: "0.0.8",
	})
	if err != nil {
		t.Fatalf("marshal directive: %v", err)
	}
	var back selfupdate.InstallDirective
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal directive: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped directive does not validate: %v", err)
	}
	for _, key := range []string{`"filename"`, `"sha256"`, `"version"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("directive JSON %s is missing %s", raw, key)
		}
	}
}
