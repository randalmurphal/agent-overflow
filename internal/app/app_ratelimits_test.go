package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerlifecycleapp"
	"agent-overflow/internal/settings"
)

func TestMergeRateLimitsSnapshotKeepsAccountAndDynamicBuckets(t *testing.T) {
	incoming := provider.RateLimitsSnapshot{
		Provider:  string(provider.Codex),
		AccountID: "account-one",
		UpdatedAt: 10,
		Limits: []provider.RateLimitEntry{
			{LimitID: "codex", LimitName: "Codex", WindowMins: 300, UsedPercent: 100, ResetsAt: 1000},
			{LimitID: "spark", LimitName: "Spark", WindowMins: 300, UsedPercent: 46, ResetsAt: 1000},
		},
	}
	got, changed := providerlifecycleapp.MergeSnapshot(provider.RateLimitsSnapshot{}, incoming)
	if !changed {
		t.Fatal("merge reported unchanged")
	}
	if got.AccountID != "account-one" {
		t.Fatalf("AccountID = %q, want account-one", got.AccountID)
	}
	if len(got.Limits) != 2 {
		t.Fatalf("Limits len = %d, want 2", len(got.Limits))
	}
}

func TestMergeRateLimitsSnapshotCollapsesClaudeLegacyAliases(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		Limits: []provider.RateLimitEntry{
			{LimitID: "seven_day", LimitName: "seven_day", WindowMins: 10080, UsedPercent: 50, ResetsAt: 1000},
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 49, ResetsAt: 1000},
			{LimitID: "weekly_scoped:fable", LimitName: "Fable", WindowMins: 10080, UsedPercent: 99, ResetsAt: 1000},
		},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		Limits: []provider.RateLimitEntry{
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 50, ResetsAt: 1000},
		},
	}

	got, changed := providerlifecycleapp.MergeSnapshot(current, incoming)
	if !changed {
		t.Fatal("merge reported unchanged")
	}
	if len(got.Limits) != 2 {
		t.Fatalf("Limits len = %d, want canonical weekly plus Fable: %+v", len(got.Limits), got.Limits)
	}
	if got.Limits[0].LimitID != "weekly_all" || got.Limits[0].LimitName != "All models" {
		t.Fatalf("canonical weekly limit = %+v", got.Limits[0])
	}
	if got.Limits[0].UsedPercent != 50 {
		t.Fatalf("canonical weekly utilization = %v, want 50", got.Limits[0].UsedPercent)
	}
	if got.Limits[1].LimitID != "weekly_scoped:fable" {
		t.Fatalf("scoped limit was changed: %+v", got.Limits[1])
	}
}

func TestMergeRateLimitsSnapshotAcceptsHigherUsageAcrossResetTimestampJitter(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		UpdatedAt: 10,
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 0, ResetsAt: 1784841601,
		}},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		UpdatedAt: 20,
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 8, ResetsAt: 1784841599,
		}},
	}

	got, changed := providerlifecycleapp.MergeSnapshot(current, incoming)
	if !changed {
		t.Fatal("merge rejected a higher reading from the same jittered window")
	}
	if got.Limits[0].UsedPercent != 8 {
		t.Fatalf("UsedPercent = %v, want 8", got.Limits[0].UsedPercent)
	}
	if got.Limits[0].ResetsAt != 1784841601 {
		t.Fatalf("ResetsAt = %d, want stable boundary 1784841601", got.Limits[0].ResetsAt)
	}
}

// Reset-boundary jitter within tolerance never moves the boundary. An equal
// reading is a no-op; a lower reading wins (same-window usage may
// legitimately drop when the limit grows mid-window — see MergeSnapshot)
// but still keeps the established boundary.
func TestMergeRateLimitsSnapshotKeepsBoundaryUnderJitter(t *testing.T) {
	for _, test := range []struct {
		name            string
		incomingPercent float64
		wantChanged     bool
	}{
		{name: "equal usage", incomingPercent: 8, wantChanged: false},
		{name: "lower usage", incomingPercent: 7, wantChanged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := provider.RateLimitsSnapshot{
				Provider: string(provider.Claude),
				Limits: []provider.RateLimitEntry{{
					LimitID: "session", LimitName: "Current session",
					WindowMins: 300, UsedPercent: 8, ResetsAt: 1784841601,
				}},
			}
			incoming := provider.RateLimitsSnapshot{
				Provider: string(provider.Claude),
				Limits: []provider.RateLimitEntry{{
					LimitID: "session", LimitName: "Current session",
					WindowMins: 300, UsedPercent: test.incomingPercent, ResetsAt: 1784841599,
				}},
			}

			got, changed := providerlifecycleapp.MergeSnapshot(current, incoming)
			if changed != test.wantChanged {
				t.Fatalf("merge changed = %v, want %v: %+v", changed, test.wantChanged, got.Limits)
			}
			if !changed {
				return
			}
			if got.Limits[0].UsedPercent != test.incomingPercent {
				t.Fatalf("newest same-window reading did not win: %+v", got.Limits)
			}
			if got.Limits[0].ResetsAt != 1784841601 {
				t.Fatalf("jittered boundary replaced the established one: %+v", got.Limits)
			}
		})
	}
}

func TestMergeRateLimitsSnapshotAcceptsLowerUsageInNewQuotaWindow(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 95, ResetsAt: 1784823600,
		}},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 5, ResetsAt: 1784841600,
		}},
	}

	got, changed := providerlifecycleapp.MergeSnapshot(current, incoming)
	if !changed {
		t.Fatal("merge rejected a lower reading from a new quota window")
	}
	if got.Limits[0].UsedPercent != 5 || got.Limits[0].ResetsAt != 1784841600 {
		t.Fatalf("new-window limit = %+v, want 5%% at 1784841600", got.Limits[0])
	}
}

func TestMergeRateLimitsSnapshotStillRejectsOlderQuotaWindow(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session", WindowMins: 300,
			UsedPercent: 5, ResetsAt: 1784841600,
		}},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session", WindowMins: 300,
			UsedPercent: 95, ResetsAt: 1784823600,
		}},
	}

	got, changed := providerlifecycleapp.MergeSnapshot(current, incoming)
	if changed {
		t.Fatalf("merge accepted an older quota window: %+v", got.Limits)
	}
}

// TestStartRateLimitProbeLoop_ExitsOnAppCtxCancel pins Step 1b's wiring
// into the probe loop: a regression that swapped a.lifeCtx() back to
// context.Background() (or dropped the <-ctx.Done() select arm) would
// keep the goroutine alive after Shutdown returned. Drive the loop
// with a probe stub that signals on each call, cancel appCtx after the
// startup probe lands, and confirm no further probe fires.
func TestStartRateLimitProbeLoop_ExitsOnAppCtxCancel(t *testing.T) {
	app := newTestAppWithStore(t)

	probeFired := make(chan struct{}, 4)
	probe := func() {
		select {
		case probeFired <- struct{}{}:
		default:
		}
	}

	app.providerLifecycleService().StartProbeLoop(providerlifecycleapp.ProbeLoop{
		ProbeImmediately:   true,
		TurnCompletedSince: func(time.Time) bool { return true },
		Probe:              probe,
	})

	// Wait for the startup probe so we know the goroutine reached the
	// select-loop body, not still mid-spawn.
	select {
	case <-probeFired:
	case <-time.After(time.Second):
		t.Fatal("startup probe never fired")
	}

	// Cancel appCtx the same way Shutdown step 1b does.
	app.appCancel()

	// The select arm on ctx.Done() must win — no further probe calls.
	// rateLimitProbeInterval is 2 minutes in production, so any probe
	// inside this window is the spurious post-cancel call we're
	// guarding against.
	select {
	case <-probeFired:
		t.Fatal("probe fired after appCancel — loop did not honour ctx.Done()")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSessionEventHandlerTurnCompleteRecordsActivityWithoutProbing pins the
// turn-complete contract after the per-turn-probe removal: a completing turn
// records the provider's activity mark for the periodic poll and sends
// NOTHING itself. Per-turn probing multiplied across parallel sessions (and
// machines sharing one account bearer) is what earned server 429s on the
// usage endpoint; a regression that probes from the event chokepoint again
// would reintroduce it.
func TestSessionEventHandlerTurnCompleteRecordsActivityWithoutProbing(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	app := newTestAppWithStore(t)
	// Seed the canonical credential AFTER the fixture's HOME detach so a
	// probe, if one incorrectly fired, would reach the fake server rather
	// than dying on a missing credential.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	credsDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"bearer-x"}}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	app.settings = settings.NewService(t.TempDir())
	app.rateLimitProbeClientOverride = &http.Client{
		Transport: redirectRoundTripper{target: srvURL, inner: http.DefaultTransport},
	}

	mark := time.Now().Add(-time.Millisecond)
	claudeHandler := app.sessionEventHandler("thread-claude", "tok-claude", string(provider.Claude))
	claudeHandler(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "thread-claude",
		Timestamp: time.Now(),
	})

	if !app.providerLifecycleService().TurnCompletedSince(string(provider.Claude), mark) {
		t.Fatalf("Claude turn-complete did not record turn activity")
	}
	if app.providerLifecycleService().TurnCompletedSince(string(provider.Codex), mark) {
		t.Fatalf("Claude turn-complete recorded Codex activity")
	}
	// Give any incorrectly-fired probe goroutine a moment to make an HTTP
	// call before asserting silence.
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("turn-complete sent %d usage request(s), want 0 — polling belongs to the ticker", hits.Load())
	}
}

// TestStartRateLimitProbeLoop_PollsOnlyAfterTurnActivity drives the loop with
// a short interval and pins the polling economics: an idle loop never polls,
// one activity mark earns exactly one poll, and the next poll requires fresh
// activity.
func TestStartRateLimitProbeLoop_PollsOnlyAfterTurnActivity(t *testing.T) {
	app := newTestAppWithStore(t)
	const testProvider = "test-provider"

	probes := atomic.Int32{}
	probed := make(chan struct{}, 16)
	app.providerLifecycleService().StartProbeLoop(providerlifecycleapp.ProbeLoop{
		Interval: 5 * time.Millisecond,
		TurnCompletedSince: func(mark time.Time) bool {
			return app.providerLifecycleService().TurnCompletedSince(testProvider, mark)
		},
		Probe: func() {
			probes.Add(1)
			select {
			case probed <- struct{}{}:
			default:
			}
		},
	})

	// Idle boot: ticks pass, nothing polls.
	time.Sleep(40 * time.Millisecond)
	if got := probes.Load(); got != 0 {
		t.Fatalf("probes before any turn activity = %d, want 0", got)
	}

	// One completed turn earns exactly one poll.
	app.providerLifecycleService().NoteTurnActivity(testProvider)
	select {
	case <-probed:
	case <-time.After(time.Second):
		t.Fatal("no poll after turn activity")
	}
	time.Sleep(40 * time.Millisecond)
	if got := probes.Load(); got != 1 {
		t.Fatalf("probes after one turn = %d, want 1 — the mark must advance past consumed activity", got)
	}

	// Fresh activity earns the next poll.
	app.providerLifecycleService().NoteTurnActivity(testProvider)
	select {
	case <-probed:
	case <-time.After(time.Second):
		t.Fatal("no poll after fresh turn activity")
	}
}
