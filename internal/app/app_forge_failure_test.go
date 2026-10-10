package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

func TestClassifyForgeFailure(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		err     error
		kind    string
		reserve bool
		until   time.Time
	}{
		{"rate limit", &forgeapi.RateLimitedError{Host: "github.com", Pool: "graphql", Until: until}, forgeFailureRateLimited, false, until},
		{"reserve", fmt.Errorf("read: %w", &forgeapi.RateLimitedError{Host: "github.com", Pool: "core", Until: until, Reserve: true}), forgeFailureRateLimited, true, until},
		{"setup", &forgeapi.SetupError{Forge: "github", Binary: "gh", Kind: forgeapi.SetupUnauthenticated, Message: "Run gh auth login."}, forgeFailureSetup, false, time.Time{}},
		{"transient", fmt.Errorf("read: %w", &forgeapi.TransientError{Err: errors.New("dial tcp: connection refused")}), forgeFailureTransient, false, time.Time{}},
		{"deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), forgeFailureTransient, false, time.Time{}},
		{"forge", errors.New("forge GET repos/o/r/pulls/9: HTTP 500"), forgeFailureForge, false, time.Time{}},
	}
	for _, tc := range cases {
		kind, reserve, got := classifyForgeFailure(tc.err)
		if kind != tc.kind || reserve != tc.reserve || !got.Equal(tc.until) {
			t.Errorf("%s: classify = %q, %t, %v; want %q, %t, %v", tc.name, kind, reserve, got, tc.kind, tc.reserve, tc.until)
		}
	}
}

// scriptedFetch answers the pump's snapshot polls from a script: each
// poll takes the next step (the last repeats), a nil error answering a
// fixed snapshot. It records when each poll ran.
type scriptedFetch struct {
	mu    sync.Mutex
	steps []func() error
	at    []time.Time
}

func (s *scriptedFetch) fetch(_ context.Context, pr gitops.PRReference) (prUpdateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	step := s.steps[min(len(s.at), len(s.steps)-1)]
	s.at = append(s.at, time.Now())
	if err := step(); err != nil {
		return prUpdateSnapshot{}, err
	}
	return prUpdateSnapshot{Detail: gitops.PRDetail{Number: pr.Number, HeadSHA: "head-a"}}, nil
}

func (s *scriptedFetch) calls() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.at...)
}

func ok() error { return nil }

func fails(err error) func() error { return func() error { return err } }

// newScriptedPump subscribes a bare App whose snapshot polls follow
// steps, ticking every 5ms.
func newScriptedPump(t *testing.T, steps ...func() error) (*App, *scriptedFetch, chan PRUpdatedEvent, PRUpdateSubscriptionResult) {
	t.Helper()
	app := NewApp()
	stubPRCIFetch(app)
	app.prUpdates.interval = 5 * time.Millisecond
	app.prUpdates.retryBase = 5 * time.Millisecond
	app.prUpdates.staggerFn = func() time.Duration { return 0 }
	script := &scriptedFetch{steps: steps}
	app.prUpdates.fetchFn = script.fetch
	events := capturePRUpdates(t, app)
	sub, err := app.SubscribePRUpdates(context.Background(), testPR)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	t.Cleanup(func() {
		_ = app.UnsubscribePRUpdates(context.Background(), sub.ID)
		app.prUpdates.wg.Wait()
	})
	return app, script, events, sub
}

// A rate limit repeats while its kind, reserve and resume time hold, even
// when the forge's text differs (another pool of the same host); a moved
// reserve or resume time is a new frame.
func TestPRUpdateRateLimitDedupsOnKindReserveAndResumeAt(t *testing.T) {
	t.Parallel()
	resume := time.Now().Add(-time.Minute).Truncate(time.Second)
	moved := resume.Add(-time.Hour)
	_, _, events, _ := newScriptedPump(t,
		ok,
		fails(&forgeapi.RateLimitedError{Host: "github.com", Pool: "graphql", Until: resume}),
		fails(&forgeapi.RateLimitedError{Host: "github.com", Pool: "core", Until: resume}),
		fails(&forgeapi.RateLimitedError{Host: "github.com", Pool: "core", Until: resume, Reserve: true}),
		fails(&forgeapi.RateLimitedError{Host: "github.com", Pool: "core", Until: moved, Reserve: true}),
		ok,
	)
	want := resume.UTC().Format(time.RFC3339)
	limited := awaitPRUpdate(t, events, "rate limit frame")
	if limited.ErrorKind != forgeFailureRateLimited || limited.Reserve || limited.ResumeAt != want ||
		!strings.HasPrefix(limited.Error, "failed to refresh pull request (id: ") {
		t.Fatalf("rate limit frame = %+v, want rate_limited at %s", limited, want)
	}
	// The next frame is the reserve one: the other pool's identical limit
	// sent nothing.
	reserve := awaitPRUpdate(t, events, "reserve frame")
	if reserve.ErrorKind != forgeFailureRateLimited || !reserve.Reserve || reserve.ResumeAt != want || reserve.Seq <= limited.Seq {
		t.Fatalf("reserve frame = %+v", reserve)
	}
	movedFrame := awaitPRUpdate(t, events, "moved resume time frame")
	if movedFrame.ResumeAt != moved.UTC().Format(time.RFC3339) || !movedFrame.Reserve {
		t.Fatalf("moved frame = %+v", movedFrame)
	}
	recovered := awaitPRUpdate(t, events, "recovery frame")
	if recovered.Error != "" || recovered.ErrorKind != "" || recovered.ResumeAt != "" || recovered.HeadSHA != "head-a" {
		t.Fatalf("recovery frame = %+v", recovered)
	}
	expectNoPRUpdate(t, events, "healthy after recovery")
}

// A rate-limited pump polls nothing before the resume time plus its
// stagger, though its ticker fires every 5ms, and polls once it passes.
func TestPRUpdateRateLimitHoldsPollingUntilResumeAt(t *testing.T) {
	t.Parallel()
	const stagger = 100 * time.Millisecond
	var resume atomic.Pointer[time.Time]
	app, script, events, _ := newScriptedPump(t,
		ok,
		func() error {
			until := time.Now().Add(300 * time.Millisecond)
			resume.Store(&until)
			return &forgeapi.RateLimitedError{Host: "github.com", Pool: "graphql", Until: until}
		},
		ok,
	)
	app.prUpdates.staggerFn = func() time.Duration { return stagger }
	limited := awaitPRUpdate(t, events, "rate limit frame")
	until := *resume.Load()
	if limited.ErrorKind != forgeFailureRateLimited || limited.ResumeAt != until.UTC().Format(time.RFC3339) {
		t.Fatalf("rate limit frame = %+v", limited)
	}
	recovered := awaitPRUpdate(t, events, "recovery frame")
	if recovered.Error != "" {
		t.Fatalf("recovery frame = %+v", recovered)
	}
	calls := script.calls()
	if len(calls) < 3 {
		t.Fatalf("polls = %d, want the limited poll and the recovery", len(calls))
	}
	if next := calls[2]; next.Before(until.Add(stagger)) {
		t.Fatalf("polled %v before the release at resume+stagger", until.Add(stagger).Sub(next))
	}
}

// The CI phase holds a rate-limited pipeline poll until the release and
// polls right after it, while a person's refresh still reaches the forge.
// With the snapshot ticking every 5ms, each tick asks for the unknown
// pipeline; the hold refuses those too.
func TestPRCIRateLimitHoldsThePipelinePollUntilResumeAt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		interval time.Duration
	}{{"CI timer", time.Hour}, {"snapshot ticks", 5 * time.Millisecond}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPRCIFixture(t, true)
			f.app.prUpdates.interval = tc.interval
			const stagger = 50 * time.Millisecond
			f.app.prUpdates.staggerFn = func() time.Duration { return stagger }
			until := time.Now().Add(700 * time.Millisecond)
			release := until.Add(stagger)
			f.publish(gitops.CIPipeline{}, &forgeapi.RateLimitedError{Host: "github.com", Pool: "core", Until: until, Reserve: true})
			sub := f.subscribe(t)
			limited := awaitCIEvent(t, f.ciEvents, "rate limit frame")
			if limited.ErrorKind != forgeFailureRateLimited || !limited.Reserve || limited.ResumeAt != until.UTC().Format(time.RFC3339) {
				t.Fatalf("CI rate limit frame = %+v", limited)
			}
			joined, err := f.app.SubscribePRUpdates(context.Background(), testPR)
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if joined.CIErrorKind != forgeFailureRateLimited || !joined.CIReserve || joined.CIResumeAt != limited.ResumeAt {
				t.Fatalf("joiner CI failure = %q %t %q", joined.CIErrorKind, joined.CIReserve, joined.CIResumeAt)
			}
			if err := f.app.UnsubscribePRUpdates(context.Background(), joined.ID); err != nil {
				t.Fatalf("unsubscribe joiner: %v", err)
			}
			expectCountHolds(t, &f.ciFetches, "rate-limited pipeline polled before its release")
			// A person's refresh is interactive: it goes through.
			before := f.ciFetches.Load()
			if err := f.app.RefreshPRCI(context.Background(), sub.ID); err == nil {
				t.Fatal("RefreshPRCI during the limit reported no failure")
			}
			if f.ciFetches.Load() != before+1 {
				t.Fatalf("RefreshPRCI did not reach the forge: %d -> %d", before, f.ciFetches.Load())
			}
			expectCountHolds(t, &f.ciFetches, "rate-limited pipeline polled after a refresh, before its release")
			f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "1", Name: "unit", Status: gitops.CIStatusRunning}), nil)
			recovered := awaitCIEvent(t, f.ciEvents, "recovery frame")
			now := time.Now()
			if recovered.Pipeline == nil || recovered.ErrorKind != "" {
				t.Fatalf("recovery frame = %+v", recovered)
			}
			if now.Before(release) {
				t.Fatalf("the pipeline recovered %v before its release", release.Sub(now))
			}
			if late := now.Sub(release); late > 200*time.Millisecond {
				t.Fatalf("the pipeline recovered %v after its release, want the poll at the release", late)
			}
		})
	}
}

func transientErr() error {
	return &forgeapi.TransientError{Err: errors.New("dial tcp 140.82.112.6:443: connection reset")}
}

// One transient failure while a snapshot is on screen sends nothing; a
// success after it sends nothing either.
func TestPRUpdateTransientFailureIsGracedOnce(t *testing.T) {
	t.Parallel()
	_, script, events, sub := newScriptedPump(t, ok, fails(transientErr()), ok)
	if sub.Error != "" || sub.HeadSHA != "head-a" {
		t.Fatalf("subscribe = %+v", sub)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(script.calls()) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(script.calls()) < 4 {
		t.Fatalf("polls = %d", len(script.calls()))
	}
	expectNoPRUpdate(t, events, "a graced transient failure and its recovery")
}

// The retry's failure is reported: a second transient as transient, a
// different failure as its own kind.
func TestPRUpdateTransientFailureReportsTheRetrysFailure(t *testing.T) {
	t.Parallel()
	_, _, events, _ := newScriptedPump(t, ok, fails(transientErr()), fails(transientErr()), ok)
	failed := awaitPRUpdate(t, events, "second transient failure")
	if failed.ErrorKind != forgeFailureTransient || !strings.HasPrefix(failed.Error, "failed to refresh pull request (id: ") {
		t.Fatalf("transient frame = %+v", failed)
	}
	if recovered := awaitPRUpdate(t, events, "recovery"); recovered.Error != "" {
		t.Fatalf("recovery = %+v", recovered)
	}

	_, _, events, _ = newScriptedPump(t, ok, fails(transientErr()), fails(errors.New("forge GET: HTTP 502")))
	failed = awaitPRUpdate(t, events, "retry's forge failure")
	if failed.ErrorKind != forgeFailureForge {
		t.Fatalf("retry frame = %+v, want kind forge", failed)
	}
	expectNoPRUpdate(t, events, "the same forge failure again")
}

// A pump whose first fetch failed has no snapshot to hold: it reports a
// transient failure at once.
func TestPRUpdateFirstFetchTransientFailureIsReportedAtOnce(t *testing.T) {
	t.Parallel()
	_, _, _, sub := newScriptedPump(t, fails(transientErr()))
	if sub.ErrorKind != forgeFailureTransient || !strings.HasPrefix(sub.Error, "failed to refresh pull request (id: ") {
		t.Fatalf("subscribe = %+v", sub)
	}
}

// A setup failure carries its own message, which names the login to fix,
// and its kind.
func TestPRUpdateSetupFailureCarriesItsMessage(t *testing.T) {
	t.Parallel()
	setup := &forgeapi.SetupError{Forge: "github", Binary: "gh", Kind: forgeapi.SetupUnauthenticated, Message: "GitHub CLI is not logged in. Run gh auth login."}
	_, _, events, sub := newScriptedPump(t, fails(setup), ok)
	if sub.ErrorKind != forgeFailureSetup || sub.Error != setup.Message || sub.ResumeAt != "" {
		t.Fatalf("subscribe = %+v", sub)
	}
	if recovered := awaitPRUpdate(t, events, "recovery"); recovered.Error != "" || recovered.ErrorKind != "" {
		t.Fatalf("recovery = %+v", recovered)
	}
}

// A re-sent follow revalidates the log with the ETag the last fetch
// returned: the forge's 304 keeps the text and sends no frame.
func TestPRCILogFollowRevalidatesWithItsETag(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusSuccess, LogsAvailable: true}), nil)
	f.setLog("7", "done\n", nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	if _, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"}); err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	awaitLogEvent(t, f.logEvents, "first log frame")
	again, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("re-follow: %v", err)
	}
	if state := again.Logs["7"]; state.Text != "done\n" || !state.Available || state.Error != "" {
		t.Fatalf("re-follow state = %+v, want the held text", state)
	}
	f.mu.Lock()
	etags := append([]string(nil), f.logETags...)
	f.mu.Unlock()
	if len(etags) != 2 || etags[0] != "" || etags[1] != strconv.Quote("done\n") {
		t.Fatalf("log fetches carried If-None-Match %q, want none then the first answer's ETag", etags)
	}
	expectNoLogEvent(t, f.logEvents, "a 304 re-fetch")
}

// isolatedForgeCore builds the App's git core over an isolated transport
// pointed at handler.
func isolatedForgeCore(t *testing.T, app *App, handler http.Handler) {
	t.Helper()
	fake := httptest.NewServer(handler)
	t.Cleanup(fake.Close)
	app.version = "1.2.3"
	ConfigureIsolation(app, IsolationConfig{})
	SetForgeAPI(app, ForgeAPI{BaseURL: fake.URL, Token: "fake-token-1"})
	core, err := app.newGitCore()
	if err != nil {
		t.Fatalf("newGitCore: %v", err)
	}
	t.Cleanup(func() { core.ForgeAPI().Close() })
	app.git = core
}

// A GitLab snapshot tick whose reads all answer 304 sends nothing and
// asks each endpoint once: the transport answers from its ETag store.
func TestPRUpdateGitLabNotModifiedTickIsQuiet(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"/merge_requests/12":             `{"iid":12,"title":"Fix","state":"opened","source_branch":"feat","target_branch":"main","sha":"head-a","author":{"username":"dev"}}`,
		"/merge_requests/12/approvals":   `{"approved_by":[]}`,
		"/merge_requests/12/discussions": `[]`,
	}
	var mu sync.Mutex
	type seen struct{ path, inm string }
	var requests []seen
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix := r.URL.Path[strings.Index(r.URL.Path, "/merge_requests/"):]
		body, ok := bodies[suffix]
		if !ok {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		requests = append(requests, seen{suffix, r.Header.Get("If-None-Match")})
		mu.Unlock()
		etag := strconv.Quote(suffix)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	app := NewApp()
	stubPRCIFetch(app)
	isolatedForgeCore(t, app, handler)
	app.prUpdates.interval = 10 * time.Millisecond
	events := capturePRUpdates(t, app)
	sub, err := app.SubscribePRUpdates(context.Background(), testGitLabPR)
	if err != nil || sub.Error != "" || sub.Detail.Number != 12 {
		t.Fatalf("SubscribePRUpdates = %+v, %v", sub, err)
	}
	t.Cleanup(func() {
		_ = app.UnsubscribePRUpdates(context.Background(), sub.ID)
		app.prUpdates.wg.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(requests)
		mu.Unlock()
		if n >= 4*len(bodies) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	expectNoPRUpdate(t, events, "304 ticks")
	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 4*len(bodies) {
		t.Fatalf("requests = %d, want several ticks", len(requests))
	}
	perPath := map[string]int{}
	for i, req := range requests {
		perPath[req.path]++
		if i >= len(bodies) && req.inm != strconv.Quote(req.path) {
			t.Fatalf("request %d %s carried If-None-Match %q", i, req.path, req.inm)
		}
	}
	// One tick is one request per endpoint; a 304 is not followed by a
	// full re-read. The last tick may be in flight.
	ticks := perPath["/merge_requests/12"]
	for path, n := range perPath {
		if n < ticks-1 || n > ticks {
			t.Fatalf("requests per endpoint = %v (%s)", perPath, path)
		}
	}
}

// A followed GitLab trace over the read cap reaches the pane as its tail:
// the transport re-requests the tail as a Range, the follow keeps the last
// ciLogDisplayTailBytes from a line start, and its next fetch revalidates
// with the trace's ETag.
func TestPRCILogFollowOfAnOverCapTraceShowsItsTail(t *testing.T) {
	t.Parallel()
	var trace bytes.Buffer
	for i := 0; trace.Len() <= 17*1024*1024; i++ {
		fmt.Fprintf(&trace, "line %08d of the trace\n", i)
	}
	full := trace.Bytes()
	lastLine := full[bytes.LastIndexByte(full[:len(full)-1], '\n')+1:]
	const etag = `"trace-v1"`
	var plain, ranges, notModified atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/jobs/7/trace") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == "" {
			plain.Add(1)
		}
		if r.Header.Get("Range") != "" {
			ranges.Add(1)
		}
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "text/plain")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	})
	f := newPRCIFixture(t, true)
	f.app.prUpdates.ciLogFetchFn = nil
	isolatedForgeCore(t, f.app, handler)
	// ConfigureIsolation keeps the product cadences; this follow ticks
	// every 20ms.
	f.app.prUpdates.interval = time.Hour
	f.app.prUpdates.ciLiveInterval = time.Hour
	f.app.prUpdates.ciFollowInterval = 20 * time.Millisecond
	f.publish(pipelineWith(gitops.CIStatusRunning, gitops.CIJob{ID: "7", Name: "unit", Status: gitops.CIStatusRunning, LogsAvailable: true}), nil)
	pr := testGitLabPR
	sub, err := f.app.SubscribePRUpdates(context.Background(), pr)
	if err != nil {
		t.Fatalf("SubscribePRUpdates: %v", err)
	}
	t.Cleanup(func() {
		_ = f.app.UnsubscribePRUpdates(context.Background(), sub.ID)
		f.app.prUpdates.wg.Wait()
	})
	awaitCIEvent(t, f.ciEvents, "running pipeline frame")
	result, err := f.app.SetPRCILogFollows(context.Background(), sub.ID, []string{"7"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	state := result.Logs["7"]
	if state.Error != "" || !state.Truncated || len(state.Text) > ciLogDisplayTailBytes || len(state.Text) < ciLogDisplayTailBytes-1024 {
		t.Fatalf("follow state: error %q truncated %t len %d", state.Error, state.Truncated, len(state.Text))
	}
	if !strings.HasPrefix(state.Text, "line ") || !strings.HasSuffix(state.Text, string(lastLine)) {
		t.Fatalf("follow text is not the trace's tail from a line start: %q ... %q", state.Text[:40], state.Text[len(state.Text)-40:])
	}
	if ranges.Load() != 1 {
		t.Fatalf("Range requests = %d, want the one tail read", ranges.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for notModified.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if notModified.Load() == 0 {
		t.Fatal("the follow's next fetch did not revalidate with the trace's ETag")
	}
	if plain.Load() != 2 {
		t.Fatalf("trace requests without If-None-Match = %d, want the tail read's two", plain.Load())
	}
	for len(f.logEvents) > 0 {
		<-f.logEvents
	}
	expectNoLogEvent(t, f.logEvents, "304 follow ticks")
}
