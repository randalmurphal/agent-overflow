package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/cilog"
	gitops "agent-overflow/internal/git"
)

var errNotFound = fmt.Errorf("%w: forge GET repos/o/r/actions/jobs/7/logs: HTTP 404 Not Found", gitops.ErrCIJobLogNotFound)

// A follow downloads the log now and replies with its index; each change
// of the log is a frame with the new version and how many of the lines
// before it still hold, until the job is terminal and its final log is in.
func TestSetPRCILogFollowsFetchesNowAndEmitsIndexesUntilTerminal(t *testing.T) {
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
	if state.Version != 1 || state.TotalLines != 2 || !state.Available || state.Downloading || state.Error != "" || state.Seq == 0 {
		t.Fatalf("follow result = %+v", state)
	}
	if got := f.lines(t, "7"); !slices.Equal(got, []string{"step 1", "progress 10%"}) {
		t.Fatalf("lines = %q", got)
	}
	// The follow's first fetch was also emitted as a frame; the frontend
	// dedups it by seq against the result.
	first := awaitLogEvent(t, f.logEvents, "first log frame")
	if first.Seq != state.Seq || first.JobID != "7" || first.PRKey != sub.PRKey || first.Version != 1 || first.TotalLines != 2 {
		t.Fatalf("first frame = %+v", first)
	}

	// A rewritten progress line: the new version keeps the first line.
	f.setLog("7", "step 1\nprogress 60%\nstep 2\n", nil)
	next := awaitLogEvent(t, f.logEvents, "changed log frame")
	if next.Version != 2 || next.TotalLines != 3 || next.StableLines != 1 || !next.Available || next.Seq <= first.Seq {
		t.Fatalf("changed frame = %+v", next)
	}
	// Unchanged log: quiet.
	expectNoLogEvent(t, f.logEvents, "unchanged log")

	// The job completes: one final fetch, then the follow goes idle.
	f.setLog("7", "step 1\nprogress 60%\nstep 2\ndone\n", nil)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	// A tick between the two writes above fetches the finished log while
	// the pipeline still says running; the job's terminal status then
	// arrives as a frame of its own. Either way the last frame shows the
	// finished job over the final log.
	final := awaitLogEvent(t, f.logEvents, "final log frame")
	for {
		if final.Version != 3 || final.TotalLines != 4 || final.StableLines != 3 || len(final.Sections) != 1 {
			t.Fatalf("final = %+v", final)
		}
		if final.Sections[0].Status == gitops.CIStatusSuccess {
			break
		}
		final = awaitLogEvent(t, f.logEvents, "terminal job frame")
	}
	expectCountHolds(t, &f.logFetches, "terminal job kept being fetched")
	expectCountHolds(t, &f.ciFetches, "terminal pipeline kept polling")

	// A re-sent follow (the Refresh button) fetches again, quietly when
	// nothing changed.
	before := f.logFetches.Load()
	again, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil || again.Logs["7"].Version != 3 || again.Logs["7"].TotalLines != 4 {
		t.Fatalf("re-follow = %+v, %v", again, err)
	}
	if f.logFetches.Load() != before+1 {
		t.Fatalf("re-follow did not fetch: %d -> %d", before, f.logFetches.Load())
	}
	expectNoLogEvent(t, f.logEvents, "re-follow with an unchanged log")
}

// GitHub answers 404 for a running job's log until it completed. The
// follow keeps asking while the job runs and shows the wait, not an
// error, with the job's steps as rows; the completed job's lagging log is
// a wait past the quick tries until it lands.
func TestPRCILogFollowOnANonStreamingForgeWaitsForCompletion(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, false)
	f.app.prUpdates.ciLogWaitInterval = 5 * time.Millisecond
	running := gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true, Steps: []gitops.CIStep{
		{Number: 1, Name: "Set up job", Status: gitops.CIStatusSuccess, StartedAt: "2026-10-11T00:00:00Z", CompletedAt: "2026-10-11T00:00:01Z"},
		{Number: 2, Name: "Run make test", Status: gitops.CIStatusRunning, StartedAt: "2026-10-11T00:00:01Z"},
	}}
	f.publish(pipelineWith(gitops.CIStatusRunning, running), nil)
	f.setLog("7", "", errNotFound)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")

	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	state := result.Logs["7"]
	if state.Available || state.Error != "" || state.TotalLines != 0 {
		t.Fatalf("running GitHub job state = %+v, want unavailable and no error", state)
	}
	if len(state.Sections) != 2 || state.Sections[1].Name != "Run make test" || state.Sections[1].Status != gitops.CIStatusRunning || state.Sections[1].Lines != 0 {
		t.Fatalf("rows before the log exists = %+v, want the steps without lines", state.Sections)
	}
	if _, err := f.app.GetPRCILogLines(testPR.Repository(), "7", 0, 10); !errors.Is(err, errCILogNotDownloaded) {
		t.Fatalf("lines of an unpublished log = %v", err)
	}
	if frame := awaitLogEvent(t, f.logEvents, "unavailable frame"); frame.Available || frame.Error != "" {
		t.Fatalf("frame = %+v", frame)
	}
	before := f.logFetches.Load()
	deadline := time.Now().Add(2 * time.Second)
	for f.logFetches.Load() < before+3 {
		if time.Now().After(deadline) {
			t.Fatalf("a running job's log stopped being asked for after %d fetches", f.logFetches.Load())
		}
		time.Sleep(time.Millisecond)
	}
	expectNoLogEvent(t, f.logEvents, "steady unavailable state")

	// Completion: the forge lags before the log exists, past the quick
	// tries; those misses keep the waiting state, whose rows moved with
	// the steps, then the log arrives.
	done := running
	done.Status = gitops.CIStatusFailed
	done.Steps = slices.Clone(running.Steps)
	done.Steps[1].Status, done.Steps[1].CompletedAt = gitops.CIStatusFailed, "2026-10-11T00:00:09Z"
	f.publish(pipelineWith(gitops.CIStatusFailed, done), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	if moved := awaitLogEvent(t, f.logEvents, "rows of the completed steps"); moved.Available || moved.Sections[1].Status != gitops.CIStatusFailed {
		t.Fatalf("frame = %+v", moved)
	}
	f.logFetches.Store(0)
	deadline = time.Now().Add(2 * time.Second)
	for f.logFetches.Load() < prCILogFinalAttempts+2 {
		if time.Now().After(deadline) {
			t.Fatalf("final log was not retried: %d fetches", f.logFetches.Load())
		}
		time.Sleep(time.Millisecond)
	}
	expectNoLogEvent(t, f.logEvents, "a lagging final log")
	f.setLog("7", "2026-10-11T00:00:00.1000000Z boom\n", nil)
	final := awaitLogEvent(t, f.logEvents, "final log frame")
	if !final.Available || final.Version != 1 || final.TotalLines != 1 {
		t.Fatalf("final = %+v", final)
	}
	expectCountHolds(t, &f.logFetches, "fetched log kept being fetched")
}

// A forge that serves running logs can still answer 404 for a running
// job. The follow shows it as not available, never as an error, keeps
// asking at the follow cadence without a frame per miss, and follows the
// log live once it is served, revalidating it with its ETag.
func TestPRCILogLiveUnpublishedLogIsAWaitThenFollowsGrowth(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true}), nil)
	f.setLog("7", "", errNotFound)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")

	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	if state := result.Logs["7"]; state.Available || state.Error != "" || state.TotalLines != 0 {
		t.Fatalf("unpublished running log = %+v, want unavailable and no error", state)
	}
	if frame := awaitLogEvent(t, f.logEvents, "waiting frame"); frame.Available || frame.Error != "" {
		t.Fatalf("frame = %+v", frame)
	}
	// Past the quick tries a completed job gets: a live job's misses are
	// not final attempts, so they never turn into a shown failure.
	deadline := time.Now().Add(2 * time.Second)
	for f.logFetches.Load() < prCILogFinalAttempts+3 {
		if time.Now().After(deadline) {
			t.Fatalf("a live unpublished log stopped being asked for after %d fetches", f.logFetches.Load())
		}
		time.Sleep(time.Millisecond)
	}
	expectNoLogEvent(t, f.logEvents, "steady wait on a live job")

	f.setLog("7", "step 1\n", nil)
	if first := awaitLogEvent(t, f.logEvents, "first served log"); !first.Available || first.Error != "" || first.Version != 1 || first.TotalLines != 1 {
		t.Fatalf("first log frame = %+v", first)
	}
	f.setLog("7", "step 1\nstep 2\n", nil)
	if grown := awaitLogEvent(t, f.logEvents, "grown log"); grown.Version != 2 || grown.TotalLines != 2 || grown.StableLines != 1 {
		t.Fatalf("growth frame = %+v", grown)
	}
	if got := f.lines(t, "7"); !slices.Equal(got, []string{"step 1", "step 2"}) {
		t.Fatalf("lines = %q", got)
	}
	requests := f.requests()
	if last := requests[len(requests)-1]; last.ETag != strconv.Quote("step 1\nstep 2\n") && last.ETag != strconv.Quote("step 1\n") {
		t.Fatalf("a live log was re-fetched with ETag %q", last.ETag)
	}

	f.setLog("7", "step 1\nstep 2\ndone\n", nil)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	if final := awaitLogEvent(t, f.logEvents, "final log"); final.TotalLines != 3 || final.StableLines != 2 {
		t.Fatalf("final frame = %+v", final)
	}
	expectCountHolds(t, &f.logFetches, "a completed job's fetched log kept being fetched")
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
	// The wire error names a correlation id, a hex string that can contain
	// "500"; the forge's own text is what must not reach the wire.
	if frame.Error == "" || strings.Contains(frame.Error, "glab: 500") || frame.Version != 1 || frame.TotalLines != 1 || !frame.Available {
		t.Fatalf("error frame = %+v, want the failure over the held version", frame)
	}
	if n := f.logFetches.Load(); n != 1+prCILogFinalAttempts {
		t.Fatalf("final fetch attempted %d times, want %d", n-1, prCILogFinalAttempts)
	}
	if got := f.lines(t, "7"); !slices.Equal(got, []string{"partial"}) {
		t.Fatalf("the held version is not readable under the failure: %q", got)
	}
	expectCountHolds(t, &f.logFetches, "a follow that showed its failure kept fetching")
}

// A completed job's log the forge answers 404 for shows as not available
// yet, never as an error, over the version the cache holds, and is asked
// for past the quick tries until it lands.
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

	f.setLog("7", "", errNotFound)
	f.publish(pipelineWith(gitops.CIStatusFailed, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusFailed, LogsAvailable: true}), nil)
	awaitCIEvent(t, f.ciEvents, "terminal pipeline frame")
	waiting := awaitLogEvent(t, f.logEvents, "waiting frame")
	if waiting.Available || waiting.Error != "" || waiting.Version != 1 || waiting.TotalLines != 1 {
		t.Fatalf("waiting frame = %+v, want unavailable over the held version and no error", waiting)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.logFetches.Load() < 1+prCILogFinalAttempts+3 {
		if time.Now().After(deadline) {
			t.Fatalf("the wait stopped asking after %d fetches", f.logFetches.Load())
		}
		time.Sleep(time.Millisecond)
	}
	expectNoLogEvent(t, f.logEvents, "a steady wait")

	// The Refresh button lands in the same state, not an error.
	again, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("re-follow: %v", err)
	}
	if state := again.Logs["7"]; state.Available || state.Error != "" || state.TotalLines != 1 {
		t.Fatalf("re-follow state = %+v", state)
	}

	f.setLog("7", "final\n", nil)
	final := awaitLogEvent(t, f.logEvents, "final log frame")
	if !final.Available || final.Error != "" || final.Version != 2 || final.StableLines != 0 {
		t.Fatalf("final = %+v", final)
	}
	if got := f.lines(t, "7"); !slices.Equal(got, []string{"final"}) {
		t.Fatalf("lines = %q", got)
	}
	expectCountHolds(t, &f.logFetches, "a fetched final log kept being fetched")
}

// A download the forge cuts off after it made progress shows as
// downloading and resumes from where it stopped on the follow cadence,
// even for a completed job past its quick tries.
func TestPRCILogCutDownloadResumes(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.app.prUpdates.ciLogWaitInterval = time.Hour
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	body := strings.Repeat("0123456789abcdef\n", 2000)
	f.setLog("7", body, nil)
	f.cutLog("7", 10000)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	if state := result.Logs["7"]; !state.Downloading || state.Available || state.Error != "" || state.TotalLines != 0 {
		t.Fatalf("cut download state = %+v, want downloading", state)
	}
	awaitLogEvent(t, f.logEvents, "downloading frame")
	done := awaitLogEvent(t, f.logEvents, "resumed log frame")
	if done.Downloading || !done.Available || done.TotalLines != 2000 {
		t.Fatalf("resumed frame = %+v", done)
	}
	if requests := f.requests(); len(requests) != 2 || requests[1].From != 10000 {
		t.Fatalf("requests %+v, want the resume from 10000", requests)
	}
	expectCountHolds(t, &f.logFetches, "a resumed log kept being fetched")
}

// A cache directory deleted under a followed log (a user clearing it) is
// noticed by the next read of the index, which downloads the log again
// in the same poll instead of revalidating a file that no longer exists.
func TestPRCILogDeletedCacheIsDownloadedAgain(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	f.setLog("7", "a\nb\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.app.configDir, "ci-logs", "cache")); err != nil {
		t.Fatal(err)
	}
	again, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("re-follow: %v", err)
	}
	if state := again.Logs["7"]; state.Error != "" || state.TotalLines != 2 || state.Version != 2 {
		t.Fatalf("state after the cache was deleted = %+v, want a fresh version", state)
	}
	if got := f.lines(t, "7"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("lines = %q", got)
	}
	if requests := f.requests(); len(requests) != 3 || requests[2].ETag != "" {
		t.Fatalf("requests %+v, want the revalidation then a whole download", requests)
	}
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

// A followed log is pinned in the cache past its bound; once nobody
// follows it, by unfollowing, unsubscribing or a pump that died, the bound
// applies to it again.
func TestPRCILogFollowPinsItsLogInTheCache(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	// A bound of one block: any second log is over it.
	store := cilog.Open(filepath.Join(f.app.configDir, "ci-logs", "cache"), 4<<10, t.Logf)
	f.app.ciLogOnce.Do(func() { f.app.ciLogStore = store })
	f.publish(pipelineWith(gitops.CIStatusSuccess,
		gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true},
		gitops.CIJob{ID: "8", Name: "lint", Status: gitops.CIStatusSuccess, LogsAvailable: true},
		gitops.CIJob{ID: "9", Name: "e2e", Status: gitops.CIStatusSuccess, LogsAvailable: true},
	), nil)
	big := strings.Repeat("x", 5000) + "\n"
	for _, job := range []string{"7", "8", "9"} {
		f.setLog(job, big, nil)
	}
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("follow 7: %v", err)
	}
	// Following 8 too puts the cache over its bound; 7 stays pinned.
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7", "8"}); err != nil {
		t.Fatalf("follow 8: %v", err)
	}
	if !store.Has(ciLogKey(testPR.Repository(), "7")) || !store.Has(ciLogKey(testPR.Repository(), "8")) {
		t.Fatal("a followed log was evicted")
	}
	// Unfollowing 7 lets the bound take it.
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"8"}); err != nil {
		t.Fatalf("unfollow 7: %v", err)
	}
	if store.Has(ciLogKey(testPR.Repository(), "7")) || !store.Has(ciLogKey(testPR.Repository(), "8")) {
		t.Fatalf("after unfollowing 7: has 7 %v, has 8 %v; want 7 evicted", store.Has(ciLogKey(testPR.Repository(), "7")), store.Has(ciLogKey(testPR.Repository(), "8")))
	}

	// A pump that died on its own releases its handles' follows.
	other, err := f.app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	if _, err := f.app.SetPRCILogFollows(context.Background(), other.ID, []string{"9"}); err != nil {
		t.Fatalf("follow 9: %v", err)
	}
	if !store.Has(ciLogKey(testPR.Repository(), "8")) || !store.Has(ciLogKey(testPR.Repository(), "9")) {
		t.Fatal("a followed log was evicted")
	}
	f.app.prUpdates.mu.Lock()
	pump := f.app.prUpdates.pumps[sub.PRKey]
	f.app.prUpdates.mu.Unlock()
	f.app.dropPRUpdatePump(pump)
	if store.Has(ciLogKey(testPR.Repository(), "8")) || store.Has(ciLogKey(testPR.Repository(), "9")) {
		t.Fatal("a dropped pump's follows stayed pinned over the bound")
	}
}

// GetPRCILogLines validates its window inside the method and bounds each
// reply by lines and by bytes.
func TestGetPRCILogLinesIsBounded(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	// 3000 lines of 1 KB: past both bounds.
	line := strings.Repeat("y", 1023)
	f.setLog("7", strings.Repeat(line+"\n", 3000), nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	window, err := f.app.GetPRCILogLines(testPR.Repository(), "7", 100, maxCILogLinesPerRead)
	if err != nil {
		t.Fatal(err)
	}
	bytes := 0
	for _, l := range window.Lines {
		bytes += len(l)
	}
	if window.From != 100 || window.TotalLines != 3000 || len(window.Lines) == 0 || bytes > maxCILogBytesPerRead || len(window.Lines) != maxCILogBytesPerRead/len(line) {
		t.Fatalf("window from %d: %d lines, %d bytes of %d lines", window.From, len(window.Lines), bytes, window.TotalLines)
	}
	if window, err := f.app.GetPRCILogLines(testPR.Repository(), "7", 2990, 50); err != nil || len(window.Lines) != 10 {
		t.Fatalf("window at the end = %d lines, %v", len(window.Lines), err)
	}
	if window, err := f.app.GetPRCILogLines(testPR.Repository(), "7", 3000, 5); err != nil || len(window.Lines) != 0 || window.Lines == nil {
		t.Fatalf("window past the end = %+v, %v", window.Lines, err)
	}
	// A line past the line bound is shown clipped, and listed.
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "8", Name: "build", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	f.setLog("8", "before\n"+strings.Repeat("z", 3*maxCILogLineBytes)+"\nafter\n", nil)
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"8"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	long, err := f.app.GetPRCILogLines(testPR.Repository(), "8", 0, 3)
	if err != nil || len(long.Lines) != 3 || len(long.Lines[1]) != maxCILogLineBytes || !slices.Equal(long.Clipped, []int{1}) || long.Lines[2] != "after" {
		t.Fatalf("long line read = %d lines, clipped %v, %v", len(long.Lines), long.Clipped, err)
	}
	for _, bad := range []struct {
		pr          gitops.PRReference
		job         string
		from, count int
	}{
		{testPR, "7", -1, 1},
		{testPR, "7", 0, 0},
		{testPR, "7", 0, maxCILogLinesPerRead + 1},
		{testPR, "7; rm", 0, 1},
		{gitops.PRReference{Forge: "github"}, "7", 0, 1},
	} {
		if _, err := f.app.GetPRCILogLines(bad.pr.Repository(), bad.job, bad.from, bad.count); err == nil {
			t.Fatalf("GetPRCILogLines(%+v, %q, %d, %d) was accepted", bad.pr, bad.job, bad.from, bad.count)
		}
	}
}

// A resume the forge refuses (416: the download was cut off after its last
// byte) is not asked again: the pump's next poll starts the download over
// and the log arrives whole, with no failure shown.
func TestPRCILogRefusedResumeStartsOver(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.app.prUpdates.ciLogWaitInterval = time.Hour
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	body := strings.Repeat("0123456789abcdef\n", 2000)
	f.setLog("7", body, nil)
	f.cutLog("7", len(body))
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	if state := result.Logs["7"]; !state.Downloading || state.Error != "" {
		t.Fatalf("cut download state = %+v, want downloading", state)
	}
	awaitLogEvent(t, f.logEvents, "downloading frame")
	done := awaitLogEvent(t, f.logEvents, "restarted log frame")
	if done.Downloading || !done.Available || done.Error != "" || done.TotalLines != 2000 {
		t.Fatalf("restarted frame = %+v", done)
	}
	requests := f.requests()
	if len(requests) != 3 || requests[1].From != int64(len(body)) || requests[2].From != 0 || requests[2].ETag != "" {
		t.Fatalf("requests %+v, want the refused resume and then a whole download", requests)
	}
	expectCountHolds(t, &f.logFetches, "a restarted log kept being fetched")
}
