package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// prCIFixture is a bare App whose pump halves are all stubbed: the snapshot
// fetch answers a fixed detail, the CI fetch answers whatever pipeline the
// test currently publishes, the log fetch answers the test's current text
// per job. Counters tell a test what the pump actually asked for.
type prCIFixture struct {
	app         *App
	mu          sync.Mutex
	pipeline    gitops.CIPipeline
	pipelineErr error
	stepsFor    [][]string
	logs        map[string]string
	logErrs     map[string]error
	// ciInteractive and logInteractive record, per fetch, whether its
	// context carried the interactive mark.
	ciInteractive  []bool
	logInteractive []bool
	// logETags is the If-None-Match each log fetch carried.
	logETags     []string
	ciFetches    atomic.Int32
	logFetches   atomic.Int32
	snapshotHead atomic.Pointer[string]
	ciEvents     chan PRCIUpdatedEvent
	logEvents    chan PRCILogEvent
}

func newPRCIFixture(t *testing.T, whileRunning bool) *prCIFixture {
	t.Helper()
	f := &prCIFixture{
		app:       NewApp(),
		logs:      map[string]string{},
		logErrs:   map[string]error{},
		ciEvents:  make(chan PRCIUpdatedEvent, 64),
		logEvents: make(chan PRCILogEvent, 64),
	}
	head := "head-a"
	f.snapshotHead.Store(&head)
	f.app.prUpdates.interval = time.Hour
	f.app.prUpdates.ciLiveInterval = 5 * time.Millisecond
	f.app.prUpdates.ciFollowInterval = 5 * time.Millisecond
	f.app.prUpdates.ciLogWhileRunning = &whileRunning
	f.app.prUpdates.fetchFn = func(_ context.Context, got gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{Detail: gitops.PRDetail{Number: got.Number, HeadSHA: *f.snapshotHead.Load()}}, nil
	}
	f.app.prUpdates.ciFetchFn = func(ctx context.Context, _ gitops.PRReference, _ *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ciInteractive = append(f.ciInteractive, forgeapi.IsInteractive(ctx))
		f.stepsFor = append(f.stepsFor, slices.Sorted(slices.Values(stepsFor)))
		f.ciFetches.Add(1)
		return f.pipeline, f.pipelineErr
	}
	f.app.prUpdates.ciLogFetchFn = func(ctx context.Context, _ gitops.PRReference, req gitops.CIJobLogRequest) (gitops.CIJobLog, error) {
		f.logFetches.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logInteractive = append(f.logInteractive, forgeapi.IsInteractive(ctx))
		f.logETags = append(f.logETags, req.ETag)
		if err := f.logErrs[req.JobID]; err != nil {
			return gitops.CIJobLog{}, err
		}
		// The log's ETag is its text: a fetch that names the text it holds
		// is answered 304.
		etag := strconv.Quote(f.logs[req.JobID])
		if req.ETag == etag {
			return gitops.CIJobLog{ETag: etag, NotModified: true}, nil
		}
		return gitops.CIJobLog{Text: f.logs[req.JobID], ETag: etag}, nil
	}
	f.app.testEmitHook = func(name string, data any) {
		switch name {
		case "pr:ci_updated":
			f.ciEvents <- data.(PRCIUpdatedEvent)
		case "pr:ci_log":
			f.logEvents <- data.(PRCILogEvent)
		}
	}
	return f
}

func (f *prCIFixture) publish(pipeline gitops.CIPipeline, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pipeline = pipeline
	f.pipelineErr = err
}

func (f *prCIFixture) setLog(jobID, text string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[jobID] = text
	f.logErrs[jobID] = err
}

// lastStepsFor is the job set the latest pipeline fetch asked steps for.
func (f *prCIFixture) lastStepsFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stepsFor) == 0 {
		return nil
	}
	return f.stepsFor[len(f.stepsFor)-1]
}

func (f *prCIFixture) subscribe(t *testing.T) PRUpdateSubscriptionResult {
	t.Helper()
	sub, err := f.app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	t.Cleanup(func() {
		if err := f.app.UnsubscribePRUpdates(context.Background(), sub.ID); err != nil {
			t.Fatalf("UnsubscribePRUpdates: %v", err)
		}
		f.app.prUpdates.wg.Wait()
	})
	return sub
}

func awaitCIEvent(t *testing.T, events <-chan PRCIUpdatedEvent, why string) PRCIUpdatedEvent {
	t.Helper()
	select {
	case evt := <-events:
		return evt
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", why)
		return PRCIUpdatedEvent{}
	}
}

func awaitLogEvent(t *testing.T, events <-chan PRCILogEvent, why string) PRCILogEvent {
	t.Helper()
	select {
	case evt := <-events:
		return evt
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", why)
		return PRCILogEvent{}
	}
}

func expectNoLogEvent(t *testing.T, events <-chan PRCILogEvent, why string) {
	t.Helper()
	select {
	case evt := <-events:
		t.Fatalf("unexpected pr:ci_log (%s): %+v", why, evt)
	case <-time.After(60 * time.Millisecond):
	}
}

// expectCountHolds fails when the counter moves across several cadences:
// the pump must be idle.
func expectCountHolds(t *testing.T, counter *atomic.Int32, why string) {
	t.Helper()
	time.Sleep(15 * time.Millisecond)
	before := counter.Load()
	time.Sleep(60 * time.Millisecond)
	if after := counter.Load(); after != before {
		t.Fatalf("%s: count %d -> %d", why, before, after)
	}
}

func pipelineWith(status string, jobs ...gitops.CIJob) gitops.CIPipeline {
	return gitops.CIPipeline{Status: status, Stages: []gitops.CIStage{{Name: "test", Status: status, Jobs: jobs}}}
}

func TestPRCIPollsOnStartEmitsOnChangeAndServesJoiners(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "1", Name: "unit", Status: gitops.CIStatusSuccess}), nil)

	sub := f.subscribe(t)
	if sub.CI != nil || sub.CIError != "" {
		t.Fatalf("subscribe result CI = %+v, %q; want nil until the first poll", sub.CI, sub.CIError)
	}
	evt := awaitCIEvent(t, f.ciEvents, "first pipeline frame")
	if evt.PRKey != sub.PRKey || evt.Pipeline == nil || evt.Pipeline.Status != gitops.CIStatusSuccess || evt.Seq <= sub.Seq {
		t.Fatalf("frame = %+v", evt)
	}
	// Terminal pipeline: no further CI polls.
	expectCountHolds(t, &f.ciFetches, "terminal pipeline kept polling")

	joiner, err := f.app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer func() { _ = f.app.UnsubscribePRUpdates(context.Background(), joiner.ID) }()
	if joiner.CI == nil || joiner.CI.Status != gitops.CIStatusSuccess {
		t.Fatalf("joiner CI = %+v, want the pump's pipeline", joiner.CI)
	}
	if n := f.ciFetches.Load(); n != 1 {
		t.Fatalf("joining fetched CI: %d fetches", n)
	}
}

func TestPRCIPollsWhileLiveAndStopsWhenTerminal(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "1", Name: "unit", Status: gitops.CIStatusRunning}), nil)
	f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")

	deadline := time.Now().Add(2 * time.Second)
	for f.ciFetches.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("live pipeline polled %d times, want the live cadence", f.ciFetches.Load())
		}
		time.Sleep(time.Millisecond)
	}
	// Identical observations stay quiet.
	select {
	case evt := <-f.ciEvents:
		t.Fatalf("unchanged pipeline re-emitted: %+v", evt)
	default:
	}

	f.publish(pipelineWith(gitops.CIStatusFailed, gitops.CIJob{ID: "1", Name: "unit", Status: gitops.CIStatusFailed}), nil)
	if evt := awaitCIEvent(t, f.ciEvents, "terminal frame"); evt.Pipeline == nil || evt.Pipeline.Status != gitops.CIStatusFailed {
		t.Fatalf("frame = %+v", evt)
	}
	expectCountHolds(t, &f.ciFetches, "terminal pipeline kept polling")
}

func TestPRCIRepollsWhenTheSnapshotHeadMoves(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.app.prUpdates.interval = 5 * time.Millisecond
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "1", Name: "unit", Status: gitops.CIStatusSuccess}), nil)
	f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "first pipeline frame")
	expectCountHolds(t, &f.ciFetches, "terminal pipeline kept polling")

	f.publish(pipelineWith(gitops.CIStatusPending, gitops.CIJob{ID: "2", Name: "unit", Status: gitops.CIStatusPending}), nil)
	head := "head-b"
	f.snapshotHead.Store(&head)
	if evt := awaitCIEvent(t, f.ciEvents, "pipeline frame after the head moved"); evt.Pipeline == nil || evt.Pipeline.Stages[0].Jobs[0].ID != "2" {
		t.Fatalf("frame = %+v", evt)
	}
}

func TestPRCIFetchFailureIsEmittedOnceAndCleared(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(gitops.CIPipeline{}, errors.New("gh: boom https://token@host"))
	sub := f.subscribe(t)
	evt := awaitCIEvent(t, f.ciEvents, "error frame")
	if evt.Error == "" || strings.Contains(evt.Error, "boom") || evt.Pipeline != nil {
		t.Fatalf("error frame = %+v, want a caller-safe summary", evt)
	}
	// The failure retries on the live cadence (doubling), and the same
	// failure is not re-emitted.
	time.Sleep(30 * time.Millisecond)
	select {
	case again := <-f.ciEvents:
		t.Fatalf("duplicate failure re-emitted: %+v", again)
	default:
	}
	if f.ciFetches.Load() < 2 {
		t.Fatalf("failing CI poll was not retried: %d fetches", f.ciFetches.Load())
	}
	joiner, err := f.app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer func() { _ = f.app.UnsubscribePRUpdates(context.Background(), joiner.ID) }()
	if joiner.CIError != evt.Error {
		t.Fatalf("joiner CIError = %q, want the active failure %q", joiner.CIError, evt.Error)
	}

	// RefreshPRCI reports the failure to the caller who asked.
	if err := f.app.RefreshPRCI(context.Background(), sub.ID); err == nil || err.Error() != evt.Error {
		t.Fatalf("RefreshPRCI error = %v, want %q", err, evt.Error)
	}

	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "1", Name: "unit", Status: gitops.CIStatusSuccess}), nil)
	if err := f.app.RefreshPRCI(context.Background(), sub.ID); err != nil {
		t.Fatalf("RefreshPRCI after recovery: %v", err)
	}
	if recovered := awaitCIEvent(t, f.ciEvents, "recovery frame"); recovered.Pipeline == nil || recovered.Error != "" {
		t.Fatalf("recovery frame = %+v", recovered)
	}
}

func TestSetPRCILogFollowsFetchesNowAndStreamsDeltasUntilTerminal(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true}), nil)
	f.setLog("7", "step 1\nprogress 10%", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")

	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	state := result.Logs["7"]
	if state.Text != "step 1\nprogress 10%" || !state.Available || state.Error != "" || state.Seq == 0 {
		t.Fatalf("follow result = %+v", state)
	}
	// The follow's first fetch was also emitted as a frame; the frontend
	// dedups it by seq against the result.
	first := awaitLogEvent(t, f.logEvents, "first log frame")
	if first.Seq != state.Seq || first.PrevLen != 0 || first.Base != 0 || first.Append != state.Text {
		t.Fatalf("first frame = %+v", first)
	}

	// A rewritten progress line: the delta rewinds to the common prefix.
	f.setLog("7", "step 1\nprogress 60%\nstep 2\n", nil)
	delta := awaitLogEvent(t, f.logEvents, "delta frame")
	if delta.PrevLen != len("step 1\nprogress 10%") || delta.Base != len("step 1\nprogress ") || delta.Append != "60%\nstep 2\n" || !delta.Available || delta.Seq <= first.Seq {
		t.Fatalf("delta = %+v", delta)
	}
	// Unchanged text: quiet.
	expectNoLogEvent(t, f.logEvents, "unchanged trace")

	// The job completes: one final fetch, then the follow goes idle.
	f.setLog("7", "step 1\nprogress 60%\nstep 2\ndone\n", nil)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	final := awaitLogEvent(t, f.logEvents, "final log frame")
	if final.Append != "done\n" || final.Base != len("step 1\nprogress 60%\nstep 2\n") {
		t.Fatalf("final = %+v", final)
	}
	expectCountHolds(t, &f.logFetches, "terminal job kept being fetched")
	expectCountHolds(t, &f.ciFetches, "terminal pipeline kept polling")

	// A re-sent follow (the Refresh button) fetches again, quietly when
	// nothing changed.
	before := f.logFetches.Load()
	again, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil || again.Logs["7"].Text != "step 1\nprogress 60%\nstep 2\ndone\n" {
		t.Fatalf("re-follow = %+v, %v", again, err)
	}
	if f.logFetches.Load() != before+1 {
		t.Fatalf("re-follow did not fetch: %d -> %d", before, f.logFetches.Load())
	}
	expectNoLogEvent(t, f.logEvents, "re-follow with unchanged text")
}

func TestPRCILogFollowWaitsForCompletionWhereLogsNeedIt(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, false)
	f.app.prUpdates.ciLogWaitInterval = 5 * time.Millisecond
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true}), nil)
	f.setLog("7", "", fmt.Errorf("%w: forge GET repos/o/r/actions/jobs/7/logs: HTTP 404 Not Found", gitops.ErrCIJobLogNotFound))
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")

	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	if state := result.Logs["7"]; state.Available || state.Error != "" || state.Text != "" {
		t.Fatalf("running GitHub job state = %+v, want unavailable and no error", state)
	}
	if f.logFetches.Load() != 0 {
		t.Fatalf("a running job's log was fetched where the forge cannot serve it")
	}
	if frame := awaitLogEvent(t, f.logEvents, "unavailable frame"); frame.Available || frame.Error != "" {
		t.Fatalf("frame = %+v", frame)
	}
	expectNoLogEvent(t, f.logEvents, "steady unavailable state")

	// Completion: the forge lags before the log exists, past the quick
	// tries; those misses keep the waiting state quietly, then the text
	// arrives.
	f.publish(pipelineWith(gitops.CIStatusFailed, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusFailed, LogsAvailable: true}), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	deadline := time.Now().Add(2 * time.Second)
	for f.logFetches.Load() < prCILogFinalAttempts+2 {
		if time.Now().After(deadline) {
			t.Fatalf("final log was not retried: %d fetches", f.logFetches.Load())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case frame := <-f.logEvents:
		t.Fatalf("a lagging final log surfaced as an error: %+v", frame)
	default:
	}
	f.setLog("7", "boom\n", nil)
	final := awaitLogEvent(t, f.logEvents, "final log frame")
	if !final.Available || final.Append != "boom\n" || final.PrevLen != 0 {
		t.Fatalf("final = %+v", final)
	}
	expectCountHolds(t, &f.logFetches, "fetched log kept being fetched")
}

func TestPRCILogFinalFetchFailureSurfacesAfterTheQuickTries(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.app.prUpdates.ciLogWaitInterval = 5 * time.Millisecond
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true}), nil)
	f.setLog("7", "partial\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	awaitLogEvent(t, f.logEvents, "first log frame")

	f.setLog("7", "", errors.New("glab: 500"))
	f.publish(pipelineWith(gitops.CIStatusFailed, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusFailed, LogsAvailable: true}), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	frame := awaitLogEvent(t, f.logEvents, "error frame after the quick tries ran out")
	if frame.Error == "" || frame.PrevLen != len("partial\n") || frame.Base != frame.PrevLen || frame.Append != "" || !frame.Available {
		t.Fatalf("error frame = %+v", frame)
	}
	if n := f.logFetches.Load(); n != 1+prCILogFinalAttempts {
		t.Fatalf("final fetch attempted %d times, want %d", n-1, prCILogFinalAttempts)
	}
	expectCountHolds(t, &f.logFetches, "a follow that showed its failure kept fetching")
}

// TestPRCILogUnpublishedFinalLogIsAWaitNotAFailure: a completed job's log
// the forge answers 404 for shows as not available yet, never as an
// error, and is asked for past the quick tries until it lands.
func TestPRCILogUnpublishedFinalLogIsAWaitNotAFailure(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.app.prUpdates.ciLogWaitInterval = 5 * time.Millisecond
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true}), nil)
	f.setLog("7", "partial\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	awaitLogEvent(t, f.logEvents, "first log frame")

	f.setLog("7", "", fmt.Errorf("%w: forge GET repos/o/r/actions/jobs/7/logs: HTTP 404 Not Found", gitops.ErrCIJobLogNotFound))
	f.publish(pipelineWith(gitops.CIStatusFailed, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusFailed, LogsAvailable: true}), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	waiting := awaitLogEvent(t, f.logEvents, "waiting frame")
	if waiting.Available || waiting.Error != "" || waiting.PrevLen != len("partial\n") || waiting.Base != waiting.PrevLen || waiting.Append != "" {
		t.Fatalf("waiting frame = %+v, want unavailable with the text kept and no error", waiting)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.logFetches.Load() < 1+prCILogFinalAttempts+3 {
		if time.Now().After(deadline) {
			t.Fatalf("the wait stopped asking after %d fetches", f.logFetches.Load())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case frame := <-f.logEvents:
		t.Fatalf("a steady wait emitted %+v", frame)
	default:
	}

	// The Refresh button lands in the same state, not an error.
	again, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("re-follow: %v", err)
	}
	if state := again.Logs["7"]; state.Available || state.Error != "" || state.Text != "partial\n" {
		t.Fatalf("re-follow state = %+v", state)
	}

	f.setLog("7", "final\n", nil)
	final := awaitLogEvent(t, f.logEvents, "final log frame")
	if !final.Available || final.Error != "" || final.PrevLen != len("partial\n") || final.Base != 0 || final.Append != "final\n" {
		t.Fatalf("final = %+v", final)
	}
	expectCountHolds(t, &f.logFetches, "a fetched final log kept being fetched")
}

// TestPRCIIntervalPacesEachFollow: a completed job's log is asked for at
// the follow cadence for the quick tries, then at the wait cadence; a live
// followed job asks for the follow cadence only where the forge serves
// running logs, and the pipeline's live cadence otherwise.
func TestPRCIIntervalPacesEachFollow(t *testing.T) {
	t.Parallel()
	const (
		follow = 3 * time.Millisecond
		live   = 7 * time.Millisecond
		wait   = 11 * time.Millisecond
		poll   = 13 * time.Millisecond
	)
	app := NewApp()
	app.prUpdates.interval = poll
	app.prUpdates.ciFollowInterval = follow
	app.prUpdates.ciLiveInterval = live
	running := pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning})
	interval := func(whileRunning bool, ci *gitops.CIPipeline, state prCILogFollow) time.Duration {
		t.Helper()
		app.prUpdates.ciLogWhileRunning = &whileRunning
		pump := &prUpdatePump{pr: testPR, follows: map[string]*prCILogFollow{"7": &state}}
		if ci != nil {
			pump.ci, pump.ciKnown = *ci, true
		}
		var retry time.Duration
		return app.prCIInterval(pump, &retry)
	}

	if got := interval(false, nil, prCILogFollow{pendingFinal: true, finalAttempts: prCILogFinalAttempts - 1}); got != follow {
		t.Fatalf("quick tries interval = %v, want %v", got, follow)
	}
	if got := interval(false, nil, prCILogFollow{pendingFinal: true, finalAttempts: prCILogFinalAttempts}); got != poll {
		t.Fatalf("wait interval = %v, want the snapshot cadence %v", got, poll)
	}
	app.prUpdates.ciLogWaitInterval = wait
	if got := interval(false, nil, prCILogFollow{pendingFinal: true, finalAttempts: prCILogFinalAttempts + 50}); got != wait {
		t.Fatalf("wait interval = %v, want %v", got, wait)
	}
	if got := interval(false, &running, prCILogFollow{pendingFinal: true, finalAttempts: prCILogFinalAttempts}); got != live {
		t.Fatalf("waiting follow on a live pipeline = %v, want the live cadence %v", got, live)
	}
	if got := interval(false, &running, prCILogFollow{wasLive: true}); got != live {
		t.Fatalf("live follow without running logs = %v, want the live cadence %v", got, live)
	}
	if got := interval(true, &running, prCILogFollow{wasLive: true}); got != follow {
		t.Fatalf("live follow with running logs = %v, want %v", got, follow)
	}
	settled := pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess})
	if got := interval(true, &settled, prCILogFollow{fetched: true, available: true}); got != 0 {
		t.Fatalf("settled follow on a terminal pipeline = %v, want no poll", got)
	}
}

// TestPRCINewFollowPollsThePipelineForItsSteps: steps ride the pipeline,
// filled for followed jobs only, so a new follow polls it once with the job
// in, even when nothing else would; an unchanged follow set does not.
func TestPRCINewFollowPollsThePipelineForItsSteps(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, false)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	f.setLog("7", "ok\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	expectCountHolds(t, &f.ciFetches, "terminal pipeline kept polling")
	if got := f.lastStepsFor(); len(got) != 0 {
		t.Fatalf("unfollowed pipeline asked steps for %v", got)
	}

	before := f.ciFetches.Load()
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	if n := f.ciFetches.Load(); n != before+1 {
		t.Fatalf("a new follow polled the pipeline %d times, want once", n-before)
	}
	if got := f.lastStepsFor(); !slices.Equal(got, []string{"7"}) {
		t.Fatalf("steps asked for %v, want [7]", got)
	}
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("re-follow: %v", err)
	}
	if n := f.ciFetches.Load(); n != before+1 {
		t.Fatalf("an unchanged follow set polled the pipeline again")
	}
	expectCountHolds(t, &f.ciFetches, "settled follow kept polling")
}

func TestPRCILogFollowsAreRefcountedAndReleasedWithTheHandle(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	f.setLog("7", "ok\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	other, err := f.app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}

	for _, id := range []string{sub.ID, other.ID} {
		if _, err := f.app.SetPRCILogFollows(context.Background(), id, []string{"7"}); err != nil {
			t.Fatalf("SetPRCILogFollows(%s): %v", id, err)
		}
	}
	follows := func() int {
		f.app.prUpdates.mu.Lock()
		defer f.app.prUpdates.mu.Unlock()
		pump := f.app.prUpdates.pumps[sub.PRKey]
		if pump.follows["7"] == nil {
			return 0
		}
		return pump.follows["7"].refs
	}
	if follows() != 2 {
		t.Fatalf("refs = %d, want 2", follows())
	}
	if err := f.app.UnsubscribePRUpdates(context.Background(), other.ID); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if follows() != 1 {
		t.Fatalf("refs after one unsubscribe = %d, want 1", follows())
	}
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, nil); err != nil {
		t.Fatalf("clear follows: %v", err)
	}
	if follows() != 0 {
		t.Fatalf("refs after clearing = %d, want 0", follows())
	}
	if _, err := f.app.SetPRCILogFollows(context.Background(), "nope", []string{"7"}); !errors.Is(err, errPRUpdatesUnknownSubscription) {
		t.Fatalf("unknown subscription error = %v", err)
	}
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7; rm"}); err == nil {
		t.Fatalf("malformed job id accepted")
	}
	// The cap refuses before touching the handle: the follow set stays as
	// it was and nothing was fetched for the oversized list.
	tooMany := make([]string, maxPRCILogFollowsPerHandle+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(100 + i)
	}
	fetchesBefore := f.logFetches.Load()
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, tooMany); !errors.Is(err, ErrTooManyPRCILogFollows) {
		t.Fatalf("oversized follow list error = %v, want ErrTooManyPRCILogFollows", err)
	}
	if follows() != 0 || f.logFetches.Load() != fetchesBefore {
		t.Fatalf("refused call changed state: refs %d, fetches %d -> %d", follows(), fetchesBefore, f.logFetches.Load())
	}
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, tooMany[:maxPRCILogFollowsPerHandle]); err != nil {
		t.Fatalf("a list at the cap was refused: %v", err)
	}
}

func TestPRCIRequestsRunWhileThePumpIsPaused(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	f.setLog("7", "ok\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	if err := f.app.SetPRUpdatesActive(sub.ID, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	before := f.ciFetches.Load()
	if err := f.app.RefreshPRCI(context.Background(), sub.ID); err != nil {
		t.Fatalf("RefreshPRCI while paused: %v", err)
	}
	if f.ciFetches.Load() != before+1 {
		t.Fatalf("RefreshPRCI did not poll while paused")
	}
	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil || result.Logs["7"].Text != "ok\n" {
		t.Fatalf("follow while paused = %+v, %v", result, err)
	}
}

func TestPRCILiveTicksPauseWithThePump(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning}), nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")
	if err := f.app.SetPRUpdatesActive(sub.ID, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	expectCountHolds(t, &f.ciFetches, "paused pump kept polling CI")
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess}), nil)
	if err := f.app.SetPRUpdatesActive(sub.ID, true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if evt := awaitCIEvent(t, f.ciEvents, "catch-up CI poll after resume"); evt.Pipeline == nil || evt.Pipeline.Status != gitops.CIStatusSuccess {
		t.Fatalf("frame = %+v", evt)
	}
}

func TestCILogDelta(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		old, new      string
		prevLen, base int
		appended      string
	}{
		{"append", "a\nb\n", "a\nb\nc\n", 4, 4, "c\n"},
		{"rewritten tail", "a\nprogress 10%", "a\nprogress 90%\n", 14, 11, "90%\n"},
		{"shrink", "abc", "ab", 3, 2, ""},
		{"identical", "abc", "abc", 3, 3, ""},
		{"from empty", "", "x", 0, 0, "x"},
		{"multibyte boundary", "héllo wörld", "héllo wørld", 11, 7, "ørld"},
		{"astral counts two units", "😀a", "😀b", 3, 2, "b"},
		{"invalid byte counts one unit", "\xffab", "\xffac", 3, 2, "c"},
		{"split inside a rune backs off", "é", "è", 1, 0, "è"},
	}
	for _, tc := range cases {
		prevLen, base, appended := ciLogDelta(tc.old, tc.new)
		if prevLen != tc.prevLen || base != tc.base || appended != tc.appended {
			t.Errorf("%s: ciLogDelta = (%d, %d, %q), want (%d, %d, %q)", tc.name, prevLen, base, appended, tc.prevLen, tc.base, tc.appended)
		}
	}
}

// TestPRPumpMarksOnlyRequestedPollsInteractive: the pump's own polls are
// background work, and the polls a person's Refresh or follow asked for
// carry the interactive mark (forgeapi.WithInteractive).
func TestPRPumpMarksOnlyRequestedPollsInteractive(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	f.setLog("7", "ok\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	if err := f.app.RefreshPRCI(t.Context(), sub.ID); err != nil {
		t.Fatalf("RefreshPRCI: %v", err)
	}
	if _, err := f.app.SetPRCILogFollows(t.Context(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ciInteractive) < 2 || f.ciInteractive[0] || !f.ciInteractive[1] {
		t.Fatalf("pipeline fetch marks = %v, want the start poll unmarked and the Refresh poll marked", f.ciInteractive)
	}
	if len(f.logInteractive) != 1 || !f.logInteractive[0] {
		t.Fatalf("log fetch marks = %v, want the follow's forced fetch marked", f.logInteractive)
	}
}

// TestPRPumpCancelsItsForgeCallWhenTheLastSubscriberLeaves: a pump that
// dies cancels the forge request it has in flight instead of leaving it
// to run out its timeout.
func TestPRPumpCancelsItsForgeCallWhenTheLastSubscriberLeaves(t *testing.T) {
	t.Parallel()
	app := NewApp()
	app.prUpdates.interval = time.Hour
	app.prUpdates.fetchFn = func(context.Context, gitops.PRReference) (prUpdateSnapshot, error) {
		return prUpdateSnapshot{}, nil
	}
	entered := make(chan struct{})
	released := make(chan error, 1)
	app.prUpdates.ciFetchFn = func(ctx context.Context, _ gitops.PRReference, _ *gitops.CIPipeline, _ []string) (gitops.CIPipeline, error) {
		close(entered)
		select {
		case <-ctx.Done():
			released <- ctx.Err()
		case <-time.After(5 * time.Second):
			released <- errors.New("the pump's context outlived its last subscriber")
		}
		return gitops.CIPipeline{}, ctx.Err()
	}
	app.prUpdates.ciLogFetchFn = func(context.Context, gitops.PRReference, gitops.CIJobLogRequest) (gitops.CIJobLog, error) {
		return gitops.CIJobLog{}, nil
	}
	whileRunning := true
	app.prUpdates.ciLogWhileRunning = &whileRunning

	sub, err := app.SubscribePRUpdates(t.Context(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the pump never polled CI")
	}
	if err := app.UnsubscribePRUpdates(t.Context(), sub.ID); err != nil {
		t.Fatalf("UnsubscribePRUpdates: %v", err)
	}
	if err := <-released; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight fetch ended with %v, want context.Canceled", err)
	}
	app.prUpdates.wg.Wait()
}

// TestSubscribePRUpdatesAbandonedCallCreatesNoPump: the first fetch runs
// under the subscribe call's context, and a call that was cancelled
// mid-fetch must not leave a pump carrying its cancellation as the PR's
// failure.
func TestSubscribePRUpdatesAbandonedCallCreatesNoPump(t *testing.T) {
	t.Parallel()
	app := NewApp()
	stubPRCIFetch(app)
	ctx, cancel := context.WithCancel(t.Context())
	app.prUpdates.fetchFn = func(fetchCtx context.Context, _ gitops.PRReference) (prUpdateSnapshot, error) {
		cancel()
		<-fetchCtx.Done()
		return prUpdateSnapshot{}, fetchCtx.Err()
	}
	if _, err := app.SubscribePRUpdates(ctx, testPR); !errors.Is(err, context.Canceled) {
		t.Fatalf("SubscribePRUpdates error = %v, want context.Canceled", err)
	}
	app.prUpdates.mu.Lock()
	pumps, handles := len(app.prUpdates.pumps), len(app.prUpdates.handles)
	app.prUpdates.mu.Unlock()
	if pumps != 0 || handles != 0 {
		t.Fatalf("abandoned subscribe left %d pumps and %d handles", pumps, handles)
	}
	app.prUpdates.wg.Wait()
}

// TestSubscribePRUpdatesMarksOnlyTheFirstFetchInteractive: opening the
// pane is the user's action, so the subscribe call's fetch is marked; the
// pump's own detail polls after it are not.
func TestSubscribePRUpdatesMarksOnlyTheFirstFetchInteractive(t *testing.T) {
	t.Parallel()
	app := NewApp()
	app.prUpdates.interval = time.Millisecond
	marks := make(chan bool, 64)
	app.prUpdates.fetchFn = func(ctx context.Context, _ gitops.PRReference) (prUpdateSnapshot, error) {
		select {
		case marks <- forgeapi.IsInteractive(ctx):
		default:
		}
		return prUpdateSnapshot{}, nil
	}
	app.prUpdates.ciFetchFn = func(context.Context, gitops.PRReference, *gitops.CIPipeline, []string) (gitops.CIPipeline, error) {
		return gitops.CIPipeline{}, nil
	}
	app.prUpdates.ciLogFetchFn = func(context.Context, gitops.PRReference, gitops.CIJobLogRequest) (gitops.CIJobLog, error) {
		return gitops.CIJobLog{}, nil
	}

	sub, err := app.SubscribePRUpdates(t.Context(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	for i := range 2 {
		select {
		case marked := <-marks:
			if want := i == 0; marked != want {
				t.Fatalf("detail fetch %d interactive = %v, want %v", i, marked, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("detail fetch %d never ran", i)
		}
	}
	if err := app.UnsubscribePRUpdates(t.Context(), sub.ID); err != nil {
		t.Fatalf("UnsubscribePRUpdates: %v", err)
	}
	app.prUpdates.wg.Wait()
}
