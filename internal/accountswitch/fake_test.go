package accountswitch

import (
	"context"
	"errors"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

type notice struct{ threadID, text string }

// fakeApp is an in-memory app behind Deps. Switch moves the selection.
type fakeApp struct {
	mu         sync.Mutex
	now        time.Time
	cfg        Config
	pools      map[string]*Pool
	readings   map[accountKey]provider.RateLimitsSnapshot
	fresh      map[accountKey]provider.RateLimitsSnapshot // installed by RefreshReading
	refreshErr error
	inflight   map[string][]ThreadModel
	reaches    map[string]bool
	rejects    map[string]bool
	needsLogin map[string]bool
	switchErr  map[string]error
	// models is each thread's model; a thread not listed runs opusModel.
	models      map[string]string
	continueErr error
	// lockHook, when set, runs in Continue before admit, without the fake's
	// lock: what changed while the continue waited for the thread lock.
	lockHook func()
	// continueHook, when set, runs after admit accepted and before the
	// continue is recorded, without the fake's lock; its error is returned.
	continueHook func(ctx context.Context, threadID string) error
	// requestHook runs inside RequestReading without the fake's lock.
	requestHook func()
	// switched runs after a successful Switch without the fake's lock, as
	// the app publishes the listing and the selection move there.
	switched func(prov, from, to string)
	// beforeSwitch runs at the start of Switch without the fake's lock.
	beforeSwitch func(id string)

	switches  []string
	reports   []Report
	notices   []notice
	requests  int
	refreshes []string
	continued []string

	needsLoginAttempts map[string]int
}

func newFakeApp(prov, selected string, ids ...string) *fakeApp {
	f := &fakeApp{
		now:        testNow,
		cfg:        Config{Strategy: StrategyAccountOrder, ThresholdPercent: 95, AutoContinue: true, Revision: 1},
		pools:      map[string]*Pool{},
		readings:   map[accountKey]provider.RateLimitsSnapshot{},
		fresh:      map[accountKey]provider.RateLimitsSnapshot{},
		inflight:   map[string][]ThreadModel{},
		reaches:    map[string]bool{"claude": true},
		rejects:    map[string]bool{},
		models:     map[string]string{},
		needsLogin: map[string]bool{},
		switchErr:  map[string]error{},

		needsLoginAttempts: map[string]int{},
	}
	p := &Pool{SelectedID: selected}
	for _, id := range ids {
		p.Accounts = append(p.Accounts, Account{ID: id, Label: id + "@x.com", InPool: true, Usable: true})
	}
	f.pools[prov] = p
	return f
}

func (f *fakeApp) setReading(prov, id string, updated time.Time, limits ...provider.RateLimitEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readings[accountKey{prov, id}] = provider.RateLimitsSnapshot{Provider: prov, AccountID: id, Limits: limits, UpdatedAt: updated.UnixMilli()}
}

func (f *fakeApp) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeApp) deps() Deps {
	return Deps{
		Now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
		Settings: func() Config {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.cfg
		},
		Pool: func(prov string) Pool {
			f.mu.Lock()
			defer f.mu.Unlock()
			p := f.pools[prov]
			if p == nil {
				return Pool{}
			}
			out := *p
			out.Accounts = append([]Account(nil), p.Accounts...)
			return out
		},
		Reading: func(prov, id string) (provider.RateLimitsSnapshot, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			s, ok := f.readings[accountKey{prov, id}]
			return s, ok
		},
		RefreshReading: func(_ context.Context, prov, id string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.refreshes = append(f.refreshes, id)
			if f.refreshErr != nil {
				return f.refreshErr
			}
			if s, ok := f.fresh[accountKey{prov, id}]; ok {
				f.readings[accountKey{prov, id}] = s
			}
			return nil
		},
		RequestReading: func(string) {
			f.mu.Lock()
			f.requests++
			hook := f.requestHook
			f.mu.Unlock()
			if hook != nil {
				hook()
			}
		},
		InFlight: func(prov string) []ThreadModel {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]ThreadModel(nil), f.inflight[prov]...)
		},
		Switch: func(prov, id string) (bool, error) {
			if f.beforeSwitch != nil {
				f.beforeSwitch(id)
			}
			f.mu.Lock()
			if f.needsLogin[id] {
				f.needsLoginAttempts[id]++
				f.mu.Unlock()
				return true, errors.New("sign in required")
			}
			if err := f.switchErr[id]; err != nil {
				f.mu.Unlock()
				return false, err
			}
			from := f.pools[prov].SelectedID
			f.switches = append(f.switches, id)
			f.pools[prov].SelectedID = id
			f.pools[prov].Pinned = false
			switched := f.switched
			f.mu.Unlock()
			if switched != nil {
				switched(prov, from, id)
			}
			return false, nil
		},
		ReachesRunningTurns: func(prov string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.reaches[prov]
		},
		ContinueModel: func(threadID string) (string, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			model := f.models[threadID]
			if model == "" {
				model = opusModel
			}
			return model, !f.rejects[threadID]
		},
		Continue: func(ctx context.Context, threadID string, admit func() bool) error {
			f.mu.Lock()
			locked := f.lockHook
			f.mu.Unlock()
			if locked != nil {
				locked()
			}
			if !admit() {
				return nil
			}
			f.mu.Lock()
			hook := f.continueHook
			f.mu.Unlock()
			if hook != nil {
				if err := hook(ctx, threadID); err != nil {
					return err
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.continueErr != nil {
				return f.continueErr
			}
			f.continued = append(f.continued, threadID)
			return nil
		},
		Report: func(r Report) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reports = append(f.reports, r)
		},
		ThreadNotice: func(threadID, text string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.notices = append(f.notices, notice{threadID, text})
		},
	}
}

func (f *fakeApp) snapshot() (switches []string, reports []Report, notices []notice, continued []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.switches...), append([]Report(nil), f.reports...),
		append([]notice(nil), f.notices...), append([]string(nil), f.continued...)
}

// settle runs the queued triggers and waits for the continues they started.
func settle(c *Coordinator) {
	c.runPending(context.Background())
	c.deliveries.Wait()
}

// check runs the periodic check and waits for the continues it started.
func check(c *Coordinator) {
	c.checkWaiting(context.Background())
	c.deliveries.Wait()
}

// write records a settings write: it changes the settings and returns the
// write's revision.
func (f *fakeApp) write(change func(*Config)) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.cfg)
	f.cfg.Revision++
	return f.cfg.Revision
}

func (c *Coordinator) isWaiting(threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiting[threadID] != nil
}

func (c *Coordinator) waitingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiting)
}
