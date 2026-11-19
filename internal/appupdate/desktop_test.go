package appupdate

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// desktopTestHost is the Wails application's half of the desktop updater:
// its events reach the service through ForwardFrameworkEvent, as
// bridgeUpdaterEvents delivers them.
type desktopTestHost struct {
	service *Service
	quits   atomic.Int32
}

func (h *desktopTestHost) Emit(name string, data ...any) bool {
	var payload any
	if len(data) > 0 {
		payload = data[0]
	}
	h.service.ForwardFrameworkEvent(name, payload)
	return true
}

func (h *desktopTestHost) OnEvent(string, func(any)) func() { return func() {} }

func (h *desktopTestHost) OpenWindow(updater.WindowOptions) updater.WindowHandle { return nil }

func (h *desktopTestHost) Quit() { h.quits.Add(1) }

// fakeDesktopTrial answers Check and HandOff as told and records the calls.
type fakeDesktopTrial struct {
	mu           sync.Mutex
	takesTrial   bool
	checkErr     error
	handOffErr   error
	checks       []string
	handoffs     []string
	restartingTo string
	// restartingReads counts RestartingTo, which reads the durable record.
	restartingReads int
}

func (f *fakeDesktopTrial) Check(_ context.Context, path, version string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, path+"|"+version)
	return f.takesTrial, f.checkErr
}

func (f *fakeDesktopTrial) HandOff(_ context.Context, path, version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handoffs = append(f.handoffs, path+"|"+version)
	if f.handOffErr == nil {
		f.restartingTo = version
	}
	return f.handOffErr
}

func (f *fakeDesktopTrial) RestartingTo() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restartingReads++
	return f.restartingTo
}

func (f *fakeDesktopTrial) calls() (checks, handoffs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.checks...), append([]string(nil), f.handoffs...)
}

type desktopTestApp struct {
	*Service
	rec    *eventRecorder
	host   *desktopTestHost
	trial  *fakeDesktopTrial
	quits  atomic.Int32
	exited chan int
}

// newDesktopTestApp is a desktop updater over the real provider chain and
// the mock release server, with the trial configured. The release it
// installs is a plain binary, so the download needs no archive.
func newDesktopTestApp(t *testing.T) *desktopTestApp {
	t.Helper()
	return newDesktopTestAppWith(t, []relSpec{
		{tag: "v0.8.1", name: "Next", withHeadless: true, withChecksum: true},
	})
}

// newDesktopTestAppWith is newDesktopTestApp over releases, newest first.
func newDesktopTestAppWith(t *testing.T, releases []relSpec) *desktopTestApp {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	srv := newMockGitHub(t, releases, sumsForHeadless)
	d := &desktopTestApp{trial: &fakeDesktopTrial{}, exited: make(chan int, 1)}
	d.Service = New("0.8.0", Deps{
		RestartWatchdogDelay: 50 * time.Millisecond,
		Exit: func(code int) {
			select {
			case d.exited <- code:
			default:
			}
		},
	})
	d.rec = newEventRecorder(d.Service)
	d.host = &desktopTestHost{service: d.Service}
	if err := d.Configure(updater.New(d.host), Config{
		CurrentVersion: "0.8.0", Platform: headlessPlatform, Arch: headlessArch,
		Repository: testRepo, ChecksumAsset: "SHASUMS256", BaseURL: srv.URL, HTTPClient: srv.Client(),
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := d.ConfigureDesktopTrial(d.trial, func() { d.quits.Add(1) }); err != nil {
		t.Fatalf("ConfigureDesktopTrial: %v", err)
	}
	// The real swap re-executes the test binary as its helper.
	frameworkRestart = func(*updater.Updater, context.Context) error {
		t.Error("the framework's swap ran")
		return errors.New("the framework's swap ran")
	}
	t.Cleanup(func() { frameworkRestart = (*updater.Updater).Restart })
	return d
}

func (d *desktopTestApp) download(t *testing.T) *updater.Release {
	t.Helper()
	if _, err := d.CheckForUpdate(); err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	from := d.rec.mark()
	if err := d.DownloadUpdate(""); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	rel, ok := d.rec.awaitAfter(t, "updater:ready", from, 20*time.Second).(*updater.Release)
	if !ok || rel.Version != "0.8.1" {
		t.Fatalf("updater:ready = %+v, want release 0.8.1", rel)
	}
	return rel
}

func (d *desktopTestApp) refuteExit(t *testing.T) {
	t.Helper()
	select {
	case <-d.exited:
		t.Fatal("the restart watchdog fired for a restart that did not hand off")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDesktopTrialHandsTheDownloadedTargetToItsHelper(t *testing.T) {
	d := newDesktopTestApp(t)
	d.trial.takesTrial = true
	if err := d.RestartReady(); !errors.Is(err, ErrUpdateNotReady) {
		t.Fatalf("RestartReady before a download = %v, want ErrUpdateNotReady", err)
	}
	d.download(t)
	if err := d.RestartReady(); err != nil {
		t.Fatalf("RestartReady after the download = %v", err)
	}
	path := d.updater.handle.DownloadedPath()
	// Until a helper has the update there is nothing restarting, and the
	// record is not read for it.
	if got := d.Availability().RestartingTo; got != "" {
		t.Fatalf("RestartingTo before the handoff = %q", got)
	}
	d.trial.mu.Lock()
	reads := d.trial.restartingReads
	d.trial.mu.Unlock()
	if reads != 0 {
		t.Fatalf("the record was read %d times before the handoff", reads)
	}

	if err := d.RestartToUpdate(nil); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	checks, handoffs := d.trial.calls()
	if want := path + "|0.8.1"; len(checks) != 1 || checks[0] != want || len(handoffs) != 1 || handoffs[0] != want {
		t.Fatalf("checks %q, handoffs %q; want one of each for %q", checks, handoffs, want)
	}
	if d.quits.Load() != 1 || d.host.quits.Load() != 0 {
		t.Fatalf("quit %d times and the framework's %d; want the host's ordinary quit once", d.quits.Load(), d.host.quits.Load())
	}
	if got := d.Availability().RestartingTo; got != "0.8.1" {
		t.Fatalf("RestartingTo = %q, want the handed-off 0.8.1", got)
	}
	if err := d.RestartReady(); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("RestartReady after the handoff = %v, want ErrUpdateBusy", err)
	}
	if err := d.DownloadUpdate(""); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("DownloadUpdate after the handoff = %v, want ErrUpdateBusy", err)
	}
	select {
	case <-d.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the restart watchdog was not armed after the handoff")
	}
}

// TestDesktopTrialRestartsIntoTheDownloadedRelease: a check after the
// download finds a newer release, and the restart still installs what was
// downloaded, by its own version.
func TestDesktopTrialRestartsIntoTheDownloadedRelease(t *testing.T) {
	d := newDesktopTestAppWith(t, []relSpec{
		{tag: "v0.8.2", name: "Newer", withHeadless: true, withChecksum: true},
		{tag: "v0.8.1", name: "Next", withHeadless: true, withChecksum: true},
	})
	d.trial.takesTrial = true
	from := d.rec.mark()
	if err := d.DownloadUpdate("v0.8.1"); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	if rel, ok := d.rec.awaitAfter(t, "updater:ready", from, 20*time.Second).(*updater.Release); !ok || rel.Version != "0.8.1" {
		t.Fatalf("updater:ready = %+v, want release 0.8.1", rel)
	}
	path := d.updater.handle.DownloadedPath()
	if got, err := d.CheckForUpdate(); err != nil || got.LatestVersion != "0.8.2" {
		t.Fatalf("CheckForUpdate = %+v, %v; want the newer 0.8.2 pending", got, err)
	}
	if err := d.RestartToUpdate(nil); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	if _, handoffs := d.trial.calls(); len(handoffs) != 1 || handoffs[0] != path+"|0.8.1" {
		t.Fatalf("handoffs %q; want the downloaded 0.8.1 at %s", handoffs, path)
	}
}

func TestDesktopTrialReadyLandsAfterTheReleaseIsRecorded(t *testing.T) {
	d := newDesktopTestApp(t)
	d.trial.takesTrial = true
	record := d.deps.Emit
	restarted := make(chan error, 1)
	d.deps.Emit = func(name eventchan.Channel, data any) {
		record(name, data)
		if name == "updater:ready" {
			select {
			case restarted <- d.RestartToUpdate(nil):
			default:
			}
		}
	}
	if _, err := d.CheckForUpdate(); err != nil {
		t.Fatal(err)
	}
	if err := d.DownloadUpdate(""); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-restarted:
		if err != nil {
			t.Fatalf("restarting the instant updater:ready landed = %v, want success", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("updater:ready never arrived; saw %v", d.rec.channels())
	}
	if n := countEvents(d.rec, "updater:ready"); n != 1 {
		t.Fatalf("updater:ready arrived %d times, want once", n)
	}
}

func TestDesktopTrialFailuresKeepTheAppRunning(t *testing.T) {
	d := newDesktopTestApp(t)
	d.download(t)

	d.trial.checkErr = errors.New("not enough free space for the database backup")
	if err := d.RestartToUpdate(nil); err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("RestartToUpdate with a refused check = %v", err)
	}
	if _, handoffs := d.trial.calls(); len(handoffs) != 0 || d.busySnapshot() || d.quits.Load() != 0 {
		t.Fatalf("a refused check handed off %d, left busy %t, quit %d", len(handoffs), d.busySnapshot(), d.quits.Load())
	}

	d.trial.checkErr = nil
	d.trial.takesTrial = true
	d.trial.handOffErr = errors.New("start the update: exec format error")
	if err := d.RestartToUpdate(nil); err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("RestartToUpdate with a failed handoff = %v", err)
	}
	if d.busySnapshot() || d.quits.Load() != 0 || d.Availability().RestartingTo != "" {
		t.Fatal("a failed handoff left the app busy, quitting or restarting")
	}
	d.refuteExit(t)
	// The update is still ready for another try.
	if err := d.RestartReady(); err != nil {
		t.Fatalf("RestartReady after a failed handoff = %v", err)
	}
}

func TestDesktopTrialLeavesATargetWithoutItToTheFrameworkSwap(t *testing.T) {
	d := newDesktopTestApp(t)
	d.download(t)
	swapped := make(chan struct{}, 1)
	frameworkRestart = func(*updater.Updater, context.Context) error {
		swapped <- struct{}{}
		return nil
	}
	if err := d.RestartToUpdate(nil); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	select {
	case <-swapped:
	default:
		t.Fatal("a target without the trial did not take the framework's swap")
	}
	if _, handoffs := d.trial.calls(); len(handoffs) != 0 || d.quits.Load() != 0 {
		t.Fatalf("the swap also handed off (%d) or quit the host (%d)", len(handoffs), d.quits.Load())
	}

	// A swap that cannot start releases the fence with its error.
	d2 := newDesktopTestApp(t)
	d2.download(t)
	frameworkRestart = func(*updater.Updater, context.Context) error { return errors.New("spawn helper: boom") }
	if err := d2.RestartToUpdate(nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("RestartToUpdate with a failed swap = %v", err)
	}
	if d2.busySnapshot() {
		t.Fatal("a failed swap left the updater busy")
	}
	d2.refuteExit(t)
}

// swapLines captures the standard logger's lines about a framework swap.
// Serial: it replaces the standard logger's output.
func swapLines(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previous)
		log.SetFlags(flags)
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		var lines []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.HasPrefix(line, "updater: swap") {
				lines = append(lines, line)
			}
		}
		return lines
	}
}

// wantSwapLines fails unless got times each phase of want once, ends with
// one summary, and that summary lists want in order: a name, or
// name:skipped for a phase that did not run.
func wantSwapLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want)+1 {
		t.Fatalf("logged %d lines, want %d phases and a summary:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	var listed []string
	for i, name := range want {
		phase, skipped := strings.CutSuffix(name, ":skipped")
		prefix := "updater: swap to 0.8.1: phase=" + phase + " duration="
		listed = append(listed, phase+":")
		if skipped {
			prefix = "updater: swap to 0.8.1: phase=" + phase + " skipped: "
			listed[i] = name
		}
		if !strings.HasPrefix(got[i], prefix) {
			t.Fatalf("line %d = %q, want %q:\n%s", i, got[i], prefix, strings.Join(got, "\n"))
		}
	}
	summary := got[len(want)]
	_, phases, ok := strings.Cut(summary, " phases=")
	if !strings.HasPrefix(summary, "updater: swap to 0.8.1: summary total=") || !ok {
		t.Fatalf("last line = %q, want the summary:\n%s", summary, strings.Join(got, "\n"))
	}
	entries := strings.Split(strings.Fields(phases)[0], ",")
	if len(entries) != len(listed) {
		t.Fatalf("summary lists %q, want %q", entries, listed)
	}
	for i, entry := range entries {
		if !strings.HasPrefix(entry, listed[i]) {
			t.Fatalf("summary lists %q, want %q", entries, listed)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A target without the trial takes the framework's swap, and the update
// logs that branch as skipped, never failed, then the swap's start, this
// process's shutdown once the host has shut down, and one summary. A swap
// whose shutdown the watchdog cuts short says so, and one that cannot
// start ends with its error and no shutdown.
func TestDesktopSwapWithoutTheTrialLogsItsPhases(t *testing.T) {
	t.Run("swapped", func(t *testing.T) {
		d := newDesktopTestApp(t)
		// The watchdog's own case is below; here the host shuts down first.
		d.deps.RestartWatchdogDelay = time.Hour
		d.download(t)
		lines := swapLines(t)
		frameworkRestart = func(*updater.Updater, context.Context) error { return nil }
		if err := d.RestartToUpdate(nil); err != nil {
			t.Fatalf("RestartToUpdate: %v", err)
		}
		d.EndRestart(nil)
		d.EndRestart(errors.New("a second end"))
		want := []string{
			"updater: swap to 0.8.1: phase=handoff skipped: 0.8.1 does not apply updates with a trial",
			"updater: swap to 0.8.1: phase=swap duration=",
			"updater: swap to 0.8.1: phase=shutdown duration=",
			"updater: swap to 0.8.1: summary total=",
		}
		got := lines()
		if len(got) != len(want) {
			t.Fatalf("logged %q\nwant %q", got, want)
		}
		for i, prefix := range want {
			if !strings.HasPrefix(got[i], prefix) || strings.Contains(got[i], " failed: ") {
				t.Fatalf("line %d = %q, want %q and no failure", i, got[i], prefix)
			}
		}
		if !strings.Contains(got[3], "phases=handoff:skipped,swap:") || !strings.Contains(got[3], ",shutdown:") ||
			!strings.HasSuffix(got[3], " outcome=swapped") {
			t.Fatalf("summary = %q", got[3])
		}
	})
	t.Run("the watchdog ends the shutdown", func(t *testing.T) {
		d := newDesktopTestApp(t)
		d.download(t)
		lines := swapLines(t)
		frameworkRestart = func(*updater.Updater, context.Context) error { return nil }
		if err := d.RestartToUpdate(nil); err != nil {
			t.Fatalf("RestartToUpdate: %v", err)
		}
		select {
		case <-d.exited:
		case <-time.After(5 * time.Second):
			t.Fatal("the restart watchdog did not fire")
		}
		d.EndRestart(nil)
		got := strings.Join(lines(), "\n")
		if strings.Count(got, "phase=shutdown duration=") != 1 ||
			!strings.Contains(got, "phase=shutdown duration=") || !strings.Contains(got, "failed: the shutdown did not finish within 50ms") ||
			strings.Count(got, "summary total=") != 1 || !strings.Contains(got, "outcome=error") {
			t.Fatalf("logged:\n%s\nwant the shutdown failed once and one summary", got)
		}
	})
	// The framework's Restart quits the host before it returns, so the app
	// loop can end the swap first; the summary still comes last and lists
	// the swap.
	t.Run("the app loop ends before Restart returns", func(t *testing.T) {
		d := newDesktopTestApp(t)
		d.deps.RestartWatchdogDelay = time.Hour
		d.download(t)
		lines := swapLines(t)
		frameworkRestart = func(*updater.Updater, context.Context) error {
			d.EndRestart(nil)
			return nil
		}
		if err := d.RestartToUpdate(nil); err != nil {
			t.Fatalf("RestartToUpdate: %v", err)
		}
		d.EndRestart(nil)
		wantSwapLines(t, lines(), "handoff:skipped", "swap")
	})
	// The host's shutdown callback begins the shutdown inside Restart, and
	// the app loop ends it there or later, while the code after Restart
	// runs: the shutdown is timed from the callback, once.
	for _, tc := range []struct {
		name string
		// during is what the host does before Restart returns.
		during func(d *desktopTestApp, ended *sync.WaitGroup)
	}{
		{"the app loop ends after Restart returns", func(d *desktopTestApp, _ *sync.WaitGroup) {
			d.RestartQuitting()
		}},
		{"the app loop ends before Restart returns", func(d *desktopTestApp, _ *sync.WaitGroup) {
			d.RestartQuitting()
			d.EndRestart(nil)
		}},
		{"the app loop ends while Restart returns", func(d *desktopTestApp, ended *sync.WaitGroup) {
			d.RestartQuitting()
			ended.Add(1)
			go func() {
				defer ended.Done()
				d.EndRestart(nil)
			}()
		}},
	} {
		t.Run("the shutdown callback runs inside Restart; "+tc.name, func(t *testing.T) {
			d := newDesktopTestApp(t)
			d.deps.RestartWatchdogDelay = time.Hour
			d.download(t)
			lines := swapLines(t)
			var ended sync.WaitGroup
			frameworkRestart = func(*updater.Updater, context.Context) error {
				tc.during(d, &ended)
				return nil
			}
			if err := d.RestartToUpdate(nil); err != nil {
				t.Fatalf("RestartToUpdate: %v", err)
			}
			ended.Wait()
			d.EndRestart(nil)
			d.RestartQuitting()
			wantSwapLines(t, lines(), "handoff:skipped", "swap", "shutdown")
		})
	}
	t.Run("the swap cannot start", func(t *testing.T) {
		d := newDesktopTestApp(t)
		d.download(t)
		lines := swapLines(t)
		frameworkRestart = func(*updater.Updater, context.Context) error { return errors.New("spawn helper: boom") }
		if err := d.RestartToUpdate(nil); err == nil {
			t.Fatal("RestartToUpdate succeeded without its helper")
		}
		d.EndRestart(nil)
		got := strings.Join(lines(), "\n")
		if !strings.Contains(got, "phase=swap duration=") || !strings.Contains(got, "failed: spawn helper: boom") ||
			strings.Contains(got, "phase=shutdown") || strings.Count(got, "summary total=") != 1 ||
			!strings.Contains(got, "outcome=error") {
			t.Fatalf("logged:\n%s\nwant the swap failed, no shutdown and one summary", got)
		}
	})
}

func TestReportUnsuccessfulUpdateIsTheBootNotice(t *testing.T) {
	d := newDesktopTestApp(t)
	d.ReportUnsuccessfulUpdate("0.8.1", "the trial exited with exit status 3.")
	want := "Update to 0.8.1 didn't apply: the trial exited with exit status 3. Still running 0.8.0."
	if got := d.ApplyFailure(); got != want {
		t.Fatalf("ApplyFailure = %q, want %q", got, want)
	}
	if got := d.Availability().LastApplyFailure; got != want {
		t.Fatalf("LastApplyFailure = %q", got)
	}
}

func TestConfigureDesktopTrialNeedsAConfiguredDesktopUpdater(t *testing.T) {
	if err := New("0.8.0", Deps{}).ConfigureDesktopTrial(&fakeDesktopTrial{}, func() {}); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("an unconfigured updater = %v, want ErrUpdatesUnsupported", err)
	}
	d := newDesktopTestApp(t)
	if err := d.ConfigureDesktopTrial(nil, func() {}); err == nil {
		t.Fatal("a trial without a handoff was accepted")
	}
}
