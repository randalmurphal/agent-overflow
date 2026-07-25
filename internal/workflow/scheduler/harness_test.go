package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// contextWithDeadline bounds a Stop in tests so a wedged loop fails the test
// instead of hanging the suite.
func contextWithDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// fakeClock is the scheduler's clock under test: time only moves when a test
// says so, and every timer the loop arms is visible here.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	deadline time.Time
	channel  chan time.Time
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{deadline: c.now.Add(d), channel: make(chan time.Time, 1)}
	if d <= 0 {
		timer.channel <- c.now
		return timer.channel, func() {}
	}
	c.timers = append(c.timers, timer)
	return timer.channel, func() { c.release(timer) }
}

func (c *fakeClock) release(target *fakeTimer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index, timer := range c.timers {
		if timer == target {
			c.timers = append(c.timers[:index], c.timers[index+1:]...)
			return
		}
	}
}

// Advance moves the clock and delivers every timer that has come due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	due := make([]*fakeTimer, 0, len(c.timers))
	kept := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.deadline.After(c.now) {
			due = append(due, timer)
			continue
		}
		kept = append(kept, timer)
	}
	c.timers = kept
	now := c.now
	c.mu.Unlock()
	for _, timer := range due {
		select {
		case timer.channel <- now:
		default:
		}
	}
}

type fireRecord struct {
	automationID string
	at           int64
	itemID       string
}

type skipRecord struct {
	automationID string
	at           int64
	reason       string
}

type startRecord struct {
	automationID string
	goal         string
	seeds        map[string]any
}

// fakeStore is the scheduler's persistence under test. It is deliberately not a
// real store: these tests are about the decision, not the SQL.
type fakeStore struct {
	mu          sync.Mutex
	automations []store.Automation
	active      map[string]store.AutomationRun
	listErr     error
	getErr      error
	activeErr   error
	fires       []fireRecord
	skips       []skipRecord
}

func newFakeStore(automations ...store.Automation) *fakeStore {
	return &fakeStore{automations: automations, active: make(map[string]store.AutomationRun)}
}

func (s *fakeStore) ListEnabledAutomations() ([]store.Automation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	enabled := make([]store.Automation, 0, len(s.automations))
	for _, automation := range s.automations {
		if automation.Enabled {
			enabled = append(enabled, automation)
		}
	}
	return enabled, nil
}

// GetAutomation mirrors the real store's missing-row shape (a wrapped
// sql.ErrNoRows), because the fire path distinguishes "deleted" from "the
// database is unhappy" by exactly that sentinel.
func (s *fakeStore) GetAutomation(id string) (store.Automation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return store.Automation{}, s.getErr
	}
	for _, automation := range s.automations {
		if automation.ID == id {
			return automation, nil
		}
	}
	return store.Automation{}, fmt.Errorf("no automation %s: %w", id, sql.ErrNoRows)
}

func (s *fakeStore) ActiveAutomationRun(automationID string) (store.AutomationRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeErr != nil {
		return store.AutomationRun{}, false, s.activeErr
	}
	run, ok := s.active[automationID]
	return run, ok, nil
}

func (s *fakeStore) RecordAutomationFire(id string, firedAt int64, itemID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fires = append(s.fires, fireRecord{automationID: id, at: firedAt, itemID: itemID})
	return nil
}

func (s *fakeStore) RecordAutomationSkip(id string, at int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skips = append(s.skips, skipRecord{automationID: id, at: at, reason: reason})
	return nil
}

func (s *fakeStore) put(automation store.Automation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, existing := range s.automations {
		if existing.ID == automation.ID {
			s.automations[index] = automation
			return
		}
	}
	s.automations = append(s.automations, automation)
}

func (s *fakeStore) remove(automationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, existing := range s.automations {
		if existing.ID == automationID {
			s.automations = append(s.automations[:index], s.automations[index+1:]...)
			return
		}
	}
}

func (s *fakeStore) setActive(automationID string, run store.AutomationRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[automationID] = run
}

func (s *fakeStore) snapshotFires() []fireRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fireRecord(nil), s.fires...)
}

func (s *fakeStore) snapshotSkips() []skipRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]skipRecord(nil), s.skips...)
}

// harness wires a scheduler over the fakes and keeps every started run and
// reported error for assertions.
type harness struct {
	t         *testing.T
	clock     *fakeClock
	store     *fakeStore
	scheduler *Scheduler

	mu       sync.Mutex
	starts   []startRecord
	reports  []error
	startErr error
	nextItem int
}

func newHarness(t *testing.T, now time.Time, automations ...store.Automation) *harness {
	t.Helper()
	h := &harness{t: t, clock: newFakeClock(now), store: newFakeStore(automations...)}
	scheduler, err := New(Config{
		Store: h.store,
		Start: h.start,
		Clock: h.clock,
		Report: func(err error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.reports = append(h.reports, err)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	h.scheduler = scheduler
	scheduler.Start()
	t.Cleanup(func() {
		if err := scheduler.Stop(contextWithDeadline(t)); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	// The first sync guarantees the loop has armed once, so a later Advance
	// always meets a registered timer.
	h.sync()
	return h
}

func (h *harness) start(automation store.Automation, goal string, seeds json.RawMessage) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.startErr != nil {
		return "", h.startErr
	}
	values := map[string]any{}
	if err := json.Unmarshal(seeds, &values); err != nil {
		return "", fmt.Errorf("decode seeds: %w", err)
	}
	h.nextItem++
	itemID := fmt.Sprintf("item-%d", h.nextItem)
	h.starts = append(h.starts, startRecord{automationID: automation.ID, goal: goal, seeds: values})
	return itemID, nil
}

// sync returns once the loop has finished whatever it was doing and taken a
// command, which is what makes every assertion below race-free.
func (h *harness) sync() {
	h.t.Helper()
	if err := h.scheduler.Refresh(); err != nil {
		h.t.Fatalf("Refresh() error = %v", err)
	}
}

// advance moves the clock and returns once every fire it made due has been
// handled. Two syncs, because the first can legitimately be taken before the
// loop reaches the due occurrence; the occurrence survives that (it is armed,
// not recomputed) and the loop's timer-first priority guarantees it fires before
// the second sync is accepted.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	h.clock.Advance(d)
	h.sync()
	h.sync()
}

func (h *harness) startedAutomations() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.starts))
	for _, record := range h.starts {
		ids = append(ids, record.automationID)
	}
	return ids
}

func (h *harness) snapshotStarts() []startRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]startRecord(nil), h.starts...)
}

func (h *harness) snapshotReports() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.reports...)
}

func (h *harness) failStarts(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.startErr = err
}

func cronAutomation(id, projectID, expr string) store.Automation {
	return store.Automation{
		ID: id, ProjectID: projectID, WorkflowID: "nightly", WorkflowScope: "project",
		Name: id, Enabled: true,
		Trigger: json.RawMessage(fmt.Sprintf(`{"kind":"cron","expr":%q}`, expr)),
	}
}

func eventAutomation(id, projectID string, on ItemEventKind, workflowID string) store.Automation {
	trigger := fmt.Sprintf(`{"kind":"event","on":%q}`, on)
	if workflowID != "" {
		trigger = fmt.Sprintf(`{"kind":"event","on":%q,"workflowId":%q}`, on, workflowID)
	}
	return store.Automation{
		ID: id, ProjectID: projectID, WorkflowID: "follow-up", WorkflowScope: "project",
		Name: id, Enabled: true, Trigger: json.RawMessage(trigger),
	}
}
