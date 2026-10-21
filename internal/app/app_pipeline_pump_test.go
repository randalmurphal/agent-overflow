package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// pipelineFixture is prCIFixture's stubbed forge behind a pipeline pump:
// the pipeline read answers the published pipeline, the log reads the
// fixture's logs, and ci:pipeline_updated frames land in events.
type pipelineFixture struct {
	*prCIFixture
	events chan PipelineUpdatedEvent
	reads  chan string
}

func newPipelineFixture(t *testing.T) *pipelineFixture {
	t.Helper()
	f := &pipelineFixture{prCIFixture: newPRCIFixture(t, true), events: make(chan PipelineUpdatedEvent, 64), reads: make(chan string, 256)}
	// Registered first, so it runs after every subscription's release.
	t.Cleanup(f.app.prUpdates.wg.Wait)
	f.app.prUpdates.pipelineFetchFn = func(ctx context.Context, repo gitops.RepoReference, id string, _ *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ciInteractive = append(f.ciInteractive, forgeapi.IsInteractive(ctx))
		f.stepsFor = append(f.stepsFor, slices.Sorted(slices.Values(stepsFor)))
		f.ciFetches.Add(1)
		select {
		case f.reads <- repo.Key() + "/" + id:
		default:
		}
		return f.pipeline, f.pipelineErr
	}
	logs := f.app.testEmitHook
	f.app.testEmitHook = func(name string, data any) {
		if name == "ci:pipeline_updated" {
			f.events <- data.(PipelineUpdatedEvent)
			return
		}
		logs(name, data)
	}
	return f
}

func (f *pipelineFixture) subscribe(t *testing.T, id string) PipelineSubscriptionResult {
	t.Helper()
	sub, err := f.app.SubscribePipeline(context.Background(), testPR.Repository(), id)
	if err != nil {
		t.Fatalf("SubscribePipeline: %v", err)
	}
	t.Cleanup(func() {
		if err := f.app.UnsubscribePipeline(context.Background(), sub.ID); err != nil {
			t.Errorf("UnsubscribePipeline: %v", err)
		}
	})
	return sub
}

func (f *pipelineFixture) pumpCount() int {
	f.app.prUpdates.mu.Lock()
	defer f.app.prUpdates.mu.Unlock()
	return len(f.app.prUpdates.pipelines)
}

func awaitPipelineEvent(t *testing.T, events <-chan PipelineUpdatedEvent, why string) PipelineUpdatedEvent {
	t.Helper()
	select {
	case evt := <-events:
		return evt
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", why)
		return PipelineUpdatedEvent{}
	}
}

// The first subscriber reads the pipeline as the user's action and creates
// the pump; a joiner shares its state without a read; the last release
// stops it.
func TestPipelinePumpIsSharedPerPipelineAndReadsOnSubscribe(t *testing.T) {
	t.Parallel()
	f := newPipelineFixture(t)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess}), nil)

	if _, err := f.app.SubscribePipeline(context.Background(), testPR.Repository(), "../1"); err == nil {
		t.Fatal("a pipeline id that is no id was accepted")
	}
	a := f.subscribe(t, "41")
	if a.Key != pipelineKey(testPR.Repository(), "41") || a.Pipeline == nil || a.Pipeline.Status != gitops.CIStatusSuccess || a.Seq == 0 || a.Error != "" {
		t.Fatalf("first subscription = %+v", a)
	}
	if got := f.ciFetches.Load(); got != 1 || !f.ciInteractive[0] {
		t.Fatalf("subscribe reads = %d interactive %v, want one interactive read", got, f.ciInteractive)
	}
	b := f.subscribe(t, "41")
	if b.ID == a.ID || b.Key != a.Key || b.Seq != a.Seq || f.ciFetches.Load() != 1 {
		t.Fatalf("joiner = %+v after %d reads", b, f.ciFetches.Load())
	}
	other := f.subscribe(t, "42")
	if other.Key == a.Key || f.pumpCount() != 2 {
		t.Fatalf("another pipeline shares a pump: %s, %d pumps", other.Key, f.pumpCount())
	}

	for _, id := range []string{a.ID, b.ID} {
		if err := f.app.UnsubscribePipeline(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if f.pumpCount() != 1 {
		t.Fatalf("pumps after releasing pipeline 41 = %d, want 1", f.pumpCount())
	}
}

// A live pipeline is polled at the live cadence and each change is a
// frame; hidden, it is not polled; shown again it catches up.
func TestPipelinePumpPollsWhileLiveAndPausesWhileHidden(t *testing.T) {
	t.Parallel()
	f := newPipelineFixture(t)
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning}), nil)
	sub := f.subscribe(t, "41")

	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess}), nil)
	evt := awaitPipelineEvent(t, f.events, "the finished pipeline")
	if evt.Key != sub.Key || evt.Pipeline == nil || evt.Pipeline.Status != gitops.CIStatusSuccess || evt.Seq <= sub.Seq {
		t.Fatalf("frame = %+v", evt)
	}
	// Terminal: the live cadence stops.
	expectCountHolds(t, &f.ciFetches, "a terminal pipeline kept polling")

	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning}), nil)
	if err := f.app.RefreshPipeline(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if evt := awaitPipelineEvent(t, f.events, "the retried pipeline"); evt.Pipeline.Status != gitops.CIStatusRunning {
		t.Fatalf("frame after refresh = %+v", evt)
	}
	if err := f.app.SetPipelineActive(context.Background(), sub.ID, false); err != nil {
		t.Fatal(err)
	}
	expectCountHolds(t, &f.ciFetches, "a hidden pipeline kept polling")

	f.publish(pipelineWith(gitops.CIStatusFailed, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusFailed}), nil)
	if err := f.app.SetPipelineActive(context.Background(), sub.ID, true); err != nil {
		t.Fatal(err)
	}
	if evt := awaitPipelineEvent(t, f.events, "the catch-up read"); evt.Pipeline.Status != gitops.CIStatusFailed {
		t.Fatalf("frame after resume = %+v", evt)
	}
}

// A queued pipeline that lists no job yet is live: it is polled at the
// live cadence until it runs and finishes, then the cadence stops.
func TestPipelinePumpPollsAQueuedPipelineWithoutJobs(t *testing.T) {
	t.Parallel()
	f := newPipelineFixture(t)
	f.publish(gitops.CIPipeline{Status: gitops.CIStatusPending, Stages: []gitops.CIStage{}}, nil)
	f.subscribe(t, "41")

	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning}), nil)
	if evt := awaitPipelineEvent(t, f.events, "the queued pipeline running"); evt.Pipeline == nil || evt.Pipeline.Status != gitops.CIStatusRunning {
		t.Fatalf("frame = %+v", evt)
	}
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess}), nil)
	if evt := awaitPipelineEvent(t, f.events, "the finished pipeline"); evt.Pipeline == nil || evt.Pipeline.Status != gitops.CIStatusSuccess {
		t.Fatalf("frame = %+v", evt)
	}
	expectCountHolds(t, &f.ciFetches, "a terminal pipeline kept polling")
}

// A failed first read still creates the pump, carrying the failure,
// which retries until the pipeline reads.
func TestPipelinePumpFirstReadFailureIsCarriedAndRetried(t *testing.T) {
	t.Parallel()
	f := newPipelineFixture(t)
	f.app.prUpdates.retryBase = 5 * time.Millisecond
	f.publish(gitops.CIPipeline{}, &forgeapi.TransientError{Err: errors.New("dial refused")})
	sub := f.subscribe(t, "41")
	if sub.Pipeline != nil || sub.ErrorKind != forgeFailureTransient || sub.Error == "" {
		t.Fatalf("failed subscription = %+v", sub)
	}
	f.publish(pipelineWith(gitops.CIStatusSuccess), nil)
	evt := awaitPipelineEvent(t, f.events, "the recovered read")
	if evt.Pipeline == nil || evt.Error != "" {
		t.Fatalf("recovered frame = %+v", evt)
	}
}

// A pipeline subscription follows job logs as a PR's does; its frames
// carry the pipeline's key, not a PR's.
func TestPipelineFollowsFetchLogsUnderThePipelineKey(t *testing.T) {
	t.Parallel()
	f := newPipelineFixture(t)
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true}), nil)
	f.setLog("7", "step 1\nstep 2", nil)
	sub := f.subscribe(t, "41")

	if _, err := f.app.SetPipelineFollows(context.Background(), sub.ID, []string{"7 8"}); err == nil {
		t.Fatal("a job id that is no id was accepted")
	}
	tooMany := make([]string, maxPRCILogFollowsPerHandle+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint(100 + i)
	}
	if _, err := f.app.SetPipelineFollows(context.Background(), sub.ID, tooMany); !errors.Is(err, ErrTooManyPRCILogFollows) {
		t.Fatalf("over the follow cap: %v", err)
	}
	result, err := f.app.SetPipelineFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatal(err)
	}
	if state := result.Logs["7"]; state.TotalLines != 2 || !state.Available {
		t.Fatalf("follow result = %+v", state)
	}
	frame := awaitLogEvent(t, f.logEvents, "the log frame")
	if frame.PRKey != "" || frame.PipelineKey != sub.Key || frame.RepoKey != testPR.Repository().Key() || frame.JobID != "7" {
		t.Fatalf("log frame = %+v", frame)
	}
	if got := f.lines(t, "7"); !slices.Equal(got, []string{"step 1", "step 2"}) {
		t.Fatalf("lines = %q", got)
	}
	if _, err := f.app.SetPipelineFollows(context.Background(), "nope", []string{"7"}); !errors.Is(err, errPRUpdatesUnknownSubscription) {
		t.Fatalf("unknown subscription: %v", err)
	}
}

func TestUnsubscribePipelineUnbindsItsConnectionTie(t *testing.T) {
	t.Parallel()
	f := newPipelineFixture(t)
	f.publish(pipelineWith(gitops.CIStatusSuccess), nil)
	checkByIDTie(t, byIDTie{
		subscribe: func(ctx context.Context) (string, error) {
			sub, err := f.app.SubscribePipeline(ctx, testPR.Repository(), "41")
			return sub.ID, err
		},
		unsubscribe: f.app.UnsubscribePipeline,
		key:         pipelineCleanupKey,
		live: func(id string) bool {
			f.app.prUpdates.mu.Lock()
			defer f.app.prUpdates.mu.Unlock()
			_, ok := f.app.prUpdates.pipelineHandles[id]
			return ok
		},
	})
	f.app.prUpdates.wg.Wait()
}

// The stage summaries: bounded, names and statuses only, and one failed
// read is that row's error beside the others.
func TestPipelineStagesAreBoundedAndReportPerPipelineFailures(t *testing.T) {
	t.Parallel()
	app := NewApp()
	app.prUpdates.pipelineFetchFn = func(_ context.Context, _ gitops.RepoReference, id string, _ *gitops.CIPipeline, _ []string) (gitops.CIPipeline, error) {
		if id == "2" {
			return gitops.CIPipeline{}, errors.New("HTTP 404")
		}
		return pipelineWith(gitops.CIStatusFailed, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusFailed}), nil
	}
	ids := make([]string, maxPipelineStageReads+1)
	for i := range ids {
		ids[i] = fmt.Sprint(i + 1)
	}
	if _, err := app.ReadPipelineStages(context.Background(), testPR.Repository(), ids); err == nil {
		t.Fatal("a read over the bound was accepted")
	}
	got, err := app.ReadPipelineStages(context.Background(), testPR.Repository(), []string{"1", "2"})
	if err != nil {
		t.Fatal(err)
	}
	one := got["1"]
	if one.Error != "" || len(one.Stages) != 1 || one.Stages[0].Name != "test" || one.Stages[0].Status != gitops.CIStatusFailed || len(one.Stages[0].Jobs) != 0 {
		t.Fatalf("pipeline 1 = %+v", one)
	}
	if two := got["2"]; two.Error == "" || len(two.Stages) != 0 {
		t.Fatalf("pipeline 2 = %+v", two)
	}
}
