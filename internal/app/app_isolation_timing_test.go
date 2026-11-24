package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIsolationTimingReachesThreadPollAndTransferJobs: the intervals a
// harness boot shortens through ConfigureIsolation are the ones the request
// poller schedules with and the transfer scheduler retries with. A parked
// call never polls slower than the shortened cadence.
func TestIsolationTimingReachesThreadPollAndTransferJobs(t *testing.T) {
	t.Parallel()
	const poll = 300 * time.Millisecond
	const retry = 40 * time.Millisecond
	f := newRequestFixture(t)
	const prRetry = 25 * time.Millisecond
	const ciLive, ciFollow = 70 * time.Millisecond, 35 * time.Millisecond
	ConfigureIsolation(f.app, IsolationConfig{
		ThreadRequestPoll: poll, TransferPendingRetry: retry,
		PRUpdateRetryBase: prRetry, PRCILiveInterval: ciLive, PRCIFollowInterval: ciFollow,
	})
	if got := f.app.prUpdateRetryBase(); got != prRetry {
		t.Fatalf("pr update retry base = %s, want %s", got, prRetry)
	}
	if got := f.app.prCILiveInterval(); got != ciLive {
		t.Fatalf("pr ci live interval = %s, want %s", got, ciLive)
	}
	if got := f.app.prCIFollowInterval(); got != ciFollow {
		t.Fatalf("pr ci follow interval = %s, want %s", got, ciFollow)
	}
	computer := uuid.NewString()

	open := f.seedRemoteRequest(t, computer)
	if delay := f.scheduleDelay(t, open); !delay.is(poll) {
		t.Fatalf("open request rescheduled in %s, want %s", delay, poll)
	}
	_, _, end := f.app.beginWait(context.Background(), threadRequestWait{}, []string{open})
	if delay := f.app.threadPollDelay(open); delay != poll {
		t.Fatalf("parked request cadence = %s, want the shortened %s", delay, poll)
	}
	end()

	settled := f.seedRemoteRequest(t, computer)
	now := time.Now()
	f.settleRemote(t, settled, now, now.Add(threadLateReplyWindow))
	if delay := f.scheduleDelay(t, settled); !delay.is(2 * poll) {
		t.Fatalf("settled request rescheduled in %s, want %s", delay, 2*poll)
	}

	if err := f.app.startThreadTransfers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.app.transfers.close)
	if got := f.app.transfers.live.Load().jobs.PendingRetry(); got != retry {
		t.Fatalf("transfer pending retry = %s, want %s", got, retry)
	}
}

// TestIsolationReachesTheOlderOwnerOpsPeer: a harness boot's older-peer
// switch is what the owner operation door reads.
func TestIsolationReachesTheOlderOwnerOpsPeer(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	if f.app.ownerOpsOmitted {
		t.Fatal("an unconfigured app has owner ops omitted")
	}
	ConfigureIsolation(f.app, IsolationConfig{OmitOwnerOps: true})
	if !f.app.ownerOpsOmitted {
		t.Fatal("the older owner operations peer was not configured")
	}
}

func TestThreadWakeRetryDelayScalesWithItsBase(t *testing.T) {
	ladder := func(a *App) []time.Duration {
		var out []time.Duration
		for attempts := int64(1); attempts <= threadWakeRetryAttempts; attempts++ {
			out = append(out, a.threadWakeRetryDelay(attempts))
		}
		return out
	}
	product := ladder(&App{})
	if product[0] != 30*time.Second || product[len(product)-1] != 10*time.Minute {
		t.Fatalf("product wake retry ladder = %v; want 30s up to a 10m ceiling", product)
	}
	short := ladder(&App{wakeRetryBaseOverride: 2 * time.Second})
	for i := range product {
		if short[i]*15 != product[i] {
			t.Fatalf("2s wake retry ladder = %v; want the product ladder %v scaled by 2/30", short, product)
		}
	}
}

func TestAgentSettledWindowSpan(t *testing.T) {
	if got := (&App{}).agentSettledWindowSpan(); got != 30*time.Second {
		t.Fatalf("product settled window = %v; want 30s", got)
	}
	if got := (&App{agentSettledWindowOverride: time.Second}).agentSettledWindowSpan(); got != time.Second {
		t.Fatalf("settled window with a 1s override = %v", got)
	}
}

// isolationConsumers reads each IsolationConfig interval where the code
// that runs on it reads it. Keyed by field name and written apart from
// ConfigureIsolation, so a field applied to the wrong consumer, or not
// applied, reads the wrong value here.
var isolationConsumers = map[string]func(a *App) time.Duration{
	"ThreadRequestPoll":       (*App).threadPollNormal,
	"TransferPendingRetry":    func(a *App) time.Duration { return a.transferPendingRetry },
	"PRUpdateInterval":        (*App).prUpdatePollInterval,
	"PRUpdateRetryBase":       (*App).prUpdateRetryBase,
	"PRCILiveInterval":        (*App).prCILiveInterval,
	"PRCIFollowInterval":      (*App).prCIFollowInterval,
	"PRCILogWaitInterval":     (*App).prCILogWaitInterval,
	"MergedPRPollInterval":    func(a *App) time.Duration { return a.mergedPRs.pollInterval() },
	"PRLookupRetryBase":       func(a *App) time.Duration { return a.prLookupRetryBase },
	"IdleReapThreshold":       (*App).idleReapThreshold,
	"IdleReapInterval":        (*App).idleReapInterval,
	"WakeupReapGrace":         (*App).wakeupReapGrace,
	"AgentWakeFloor":          (*App).agentWakeFloor,
	"AgentWatchFailureWindow": (*App).agentWatchFailureWindow,
	"RemoteWatchPoll":         (*App).remoteWatchPollInterval,
	"ProviderEventDrainStall": func(a *App) time.Duration { return a.providerEvents.drainStallBound() },
	"UsageProbeGap":           func(a *App) time.Duration { return a.usageProbeGap },
	"UsagePollInterval":       func(a *App) time.Duration { return a.usagePollInterval },
	"ThreadWakeRetryBase":     func(a *App) time.Duration { return a.threadWakeRetryDelay(1) },
	"AgentSettledWindow":      (*App).agentSettledWindowSpan,
}

// isolationDerived names the consumers that follow another interval by
// design: an unset log wait paces at the PR update cadence.
var isolationDerived = map[string]string{"PRCILogWaitInterval": "PRUpdateInterval"}

// TestEveryIsolationIntervalReachesItsConsumer: an interval set through
// ConfigureIsolation is the one its consumer runs on, it moves no other
// consumer, and unset it leaves every consumer on the product value.
func TestEveryIsolationIntervalReachesItsConsumer(t *testing.T) {
	const sentinel = 7 * time.Millisecond
	read := func(config IsolationConfig) map[string]time.Duration {
		a := &App{}
		ConfigureIsolation(a, config)
		got := map[string]time.Duration{}
		for field, consumer := range isolationConsumers {
			got[field] = consumer(a)
		}
		return got
	}
	product := read(IsolationConfig{})
	unconfigured := &App{}
	for field, consumer := range isolationConsumers {
		if want := consumer(unconfigured); product[field] != want {
			t.Errorf("%s: an isolated boot with no interval set runs on %v; the product runs on %v", field, product[field], want)
		}
	}
	// A give-up limit reads the test process's patience on both sides of
	// that comparison, so its product value is read with none.
	unset := &App{}
	ConfigureIsolation(unset, IsolationConfig{})
	if got := giveUpBoundWith(unset.providerEvents.drainStall, 0, providerEventDrainStall); got != 5*time.Second {
		t.Errorf("ProviderEventDrainStall: an isolated boot with none set gives up after %v in production; want the product 5s", got)
	}
	configType := reflect.TypeFor[IsolationConfig]()
	for i := range configType.NumField() {
		field := configType.Field(i)
		if field.Type != reflect.TypeFor[time.Duration]() {
			continue
		}
		if _, ok := isolationConsumers[field.Name]; !ok {
			t.Errorf("IsolationConfig.%s has no consumer in isolationConsumers", field.Name)
			continue
		}
		var config IsolationConfig
		reflect.ValueOf(&config).Elem().Field(i).Set(reflect.ValueOf(sentinel))
		for consumer, got := range read(config) {
			switch {
			case consumer == field.Name && got != sentinel:
				t.Errorf("IsolationConfig.%s = %v: its consumer runs on %v", field.Name, sentinel, got)
			case consumer != field.Name && isolationDerived[consumer] == field.Name && got != sentinel:
				t.Errorf("IsolationConfig.%s = %v: %s, which follows it, runs on %v", field.Name, sentinel, consumer, got)
			case consumer != field.Name && isolationDerived[consumer] != field.Name && got != product[consumer]:
				t.Errorf("IsolationConfig.%s = %v moved %s to %v", field.Name, sentinel, consumer, got)
			}
		}
	}
}
