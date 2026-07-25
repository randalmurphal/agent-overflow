package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// Store is the persistence the scheduler needs: the enabled set it schedules,
// one automation by id (Run now works on a disabled row too), the overlap probe,
// and the two fire-record writes.
type Store interface {
	ListEnabledAutomations() ([]store.Automation, error)
	GetAutomation(id string) (store.Automation, error)
	ActiveAutomationRun(automationID string) (store.AutomationRun, bool, error)
	RecordAutomationFire(id string, firedAt int64, itemID string) error
	RecordAutomationSkip(id string, at int64, reason string) error
}

// StartFunc starts the run a fire calls for and returns its item id. It is the
// app's one start path (§8) — the scheduler runs no executor of its own — with
// source `automation` and the automation's id as source ref.
type StartFunc func(automation store.Automation, goal string, seeds json.RawMessage) (string, error)

// Clock is the scheduler's only source of time. Timer returns a channel that
// receives once after d (immediately for a non-positive d) plus the release for
// it; the scheduler creates one timer per loop iteration and releases it before
// doing anything else.
type Clock interface {
	Now() time.Time
	Timer(d time.Duration) (<-chan time.Time, func())
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	timer := time.NewTimer(d)
	return timer.C, func() { timer.Stop() }
}

// SystemClock is the production clock.
func SystemClock() Clock { return systemClock{} }

// refreshRetry is how long the loop waits before re-reading automations after a
// failed store read. Without it a persistent read failure would spin the loop.
const refreshRetry = time.Minute

// idleWait bounds how long the loop sleeps with no cron automation armed. It
// exists so a clock jump or an external row change cannot leave the scheduler
// parked forever on a `select` nothing will signal.
const idleWait = time.Hour

// Config wires the scheduler. Every field is required except Clock and Report.
type Config struct {
	Store Store
	Start StartFunc
	Clock Clock
	// Report receives errors that have no caller to return to: a failed store
	// read, a fire record that could not be written, an automation whose stored
	// trigger no longer parses. Nothing here is swallowed.
	Report func(error)
}

// Scheduler owns one timer and one goroutine. Every decision — a cron fire, an
// internal-event match, a Run now — is handled on that goroutine, so an
// automation's overlap check can never race another fire of the same automation.
//
// It never imports the engine: internal events reach it because the app feeds
// them in (NotifyItemEvent) from the same listener that wakes bound threads.
type Scheduler struct {
	store  Store
	start  StartFunc
	clock  Clock
	report func(error)

	commands  chan command
	quit      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	// Owner-goroutine state. The automation set is re-read from SQLite on every
	// loop iteration — the table is the truth, and re-reading it is how a CRUD
	// change takes effect — but a pending occurrence is *kept* across those
	// re-reads (see armedFire), so nothing can consume a fire that is already due.
	enabled   []store.Automation
	nextFires map[string]armedFire
	// broken remembers which automation version was already reported unparseable,
	// so a permanently broken row is reported once rather than on every tick.
	broken map[string]int64
}

// armedFire is one automation's pending cron occurrence, remembered with the
// trigger text it was computed from.
//
// It survives re-reads because `Next` only ever answers strictly *after* the
// time it is given: recomputing an occurrence that has already come due would
// silently roll it forward to the next one. That is exactly what happens when a
// command (a CRUD refresh, an internal event) lands in the same instant a fire
// is due — so the occurrence is held until it is fired, and only a changed
// trigger recomputes it.
type armedFire struct {
	trigger string
	next    time.Time
}

type commandKind int

const (
	commandRefresh commandKind = iota
	commandEvent
	commandRunNow
)

type command struct {
	kind         commandKind
	event        ItemEvent
	automationID string
	reply        chan commandResult
}

type commandResult struct {
	itemID string
	err    error
}

// ErrStopped is returned to a caller whose command arrives after the scheduler
// has stopped. It is a real error, not a silent no-op: a Run now that never ran
// must not look like one that did.
var ErrStopped = fmt.Errorf("workflow scheduler is stopped")

func New(config Config) (*Scheduler, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("workflow scheduler: store is required")
	}
	if config.Start == nil {
		return nil, fmt.Errorf("workflow scheduler: start function is required")
	}
	clock := config.Clock
	if clock == nil {
		clock = SystemClock()
	}
	report := config.Report
	if report == nil {
		return nil, fmt.Errorf("workflow scheduler: report function is required")
	}
	return &Scheduler{
		store: config.Store, start: config.Start, clock: clock, report: report,
		commands:  make(chan command),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		nextFires: make(map[string]armedFire),
		broken:    make(map[string]int64),
	}, nil
}

// Start launches the loop. Next fires are computed from now: a fire whose time
// passed while the app was closed is not replayed and not recorded as a skip —
// the app was off, and a polling automation catches up through its own cursor.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// Stop asks the loop to exit and waits for it, bounded by ctx. A fire already
// in flight finishes first; a caller blocked in NotifyItemEvent or RunNow is
// released with ErrStopped.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.quit) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("workflow scheduler: stop timed out: %w", ctx.Err())
	}
}

// Refresh recomputes the schedule. Every automation create, update, delete, and
// enable toggle calls it, so "next fire" is never stale. It returns once the
// loop has acknowledged the request and is about to re-read the table; a store
// read that then fails goes to Report, because by that point no caller is left
// to hand it to.
func (s *Scheduler) Refresh() error {
	_, err := s.send(command{kind: commandRefresh})
	return err
}

// NotifyItemEvent feeds one root run's resting transition to the event
// triggers. The app calls it from an ordered queue off the engine's emit path.
func (s *Scheduler) NotifyItemEvent(event ItemEvent) error {
	_, err := s.send(command{kind: commandEvent, event: event})
	return err
}

// RunNow fires an automation on a human's behalf. It bypasses the run-if
// condition — pressing the button *is* the decision — but respects the overlap
// policy, and reports every refusal as an error instead of a recorded skip:
// there is a human present to read it.
func (s *Scheduler) RunNow(automationID string) (string, error) {
	return s.send(command{kind: commandRunNow, automationID: automationID})
}

func (s *Scheduler) send(cmd command) (string, error) {
	cmd.reply = make(chan commandResult, 1)
	select {
	case s.commands <- cmd:
	case <-s.done:
		return "", ErrStopped
	}
	select {
	case result := <-cmd.reply:
		return result.itemID, result.err
	case <-s.done:
		return "", ErrStopped
	}
}

func (s *Scheduler) run() {
	defer close(s.done)
	for {
		wait, release := s.arm()
		// A due occurrence is fired before any queued command is taken. Without
		// this priority, a command racing the timer would be picked half the time
		// and the fire would be postponed behind it — visible to a human as a
		// schedule that occasionally runs late for no reason.
		select {
		case <-wait:
			release()
			s.fireDue()
			continue
		default:
		}
		select {
		case <-s.quit:
			release()
			return
		case cmd := <-s.commands:
			release()
			s.handle(cmd)
		case <-wait:
			release()
			s.fireDue()
		}
	}
}

// arm re-reads the enabled automations, arms a cron occurrence for each, and
// returns the timer for the earliest one. A pending occurrence is carried over
// unchanged; only a new automation or an edited trigger computes a fresh one.
func (s *Scheduler) arm() (<-chan time.Time, func()) {
	now := s.clock.Now()
	enabled, err := s.store.ListEnabledAutomations()
	if err != nil {
		s.report(fmt.Errorf("workflow scheduler: list enabled automations: %w", err))
		s.enabled = nil
		s.nextFires = make(map[string]armedFire)
		return s.clock.Timer(refreshRetry)
	}
	s.enabled = enabled
	armed := make(map[string]armedFire, len(enabled))
	broken := make(map[string]int64, len(s.broken))
	var earliest time.Time
	for _, automation := range enabled {
		trigger, err := ParseTrigger(automation.Trigger)
		if err != nil {
			// A broken trigger is surfaced, never silently skipped. It is reported
			// once per stored version: the row itself (WorkflowListAutomations)
			// carries the standing error a human acts on.
			broken[automation.ID] = automation.UpdatedAt
			if reported, ok := s.broken[automation.ID]; !ok || reported != automation.UpdatedAt {
				s.report(fmt.Errorf("workflow scheduler: automation %q (%s) has an unusable trigger: %w",
					automation.Name, automation.ID, err))
			}
			continue
		}
		key := string(automation.Trigger)
		fire, pending := s.nextFires[automation.ID]
		if !pending || fire.trigger != key {
			next, ok := trigger.Next(now)
			if !ok {
				continue
			}
			fire = armedFire{trigger: key, next: next}
		}
		armed[automation.ID] = fire
		if earliest.IsZero() || fire.next.Before(earliest) {
			earliest = fire.next
		}
	}
	s.nextFires = armed
	s.broken = broken
	if earliest.IsZero() {
		return s.clock.Timer(idleWait)
	}
	return s.clock.Timer(earliest.Sub(now))
}

// fireDue fires every cron automation whose armed occurrence has arrived, oldest
// occurrence first so a backlog fires in schedule order. A fired occurrence is
// dropped, which is what makes the next arm compute the one after it.
func (s *Scheduler) fireDue() {
	now := s.clock.Now()
	due := make([]store.Automation, 0, len(s.enabled))
	for _, automation := range s.enabled {
		armed, ok := s.nextFires[automation.ID]
		if !ok || armed.next.After(now) {
			continue
		}
		due = append(due, automation)
	}
	sort.SliceStable(due, func(left, right int) bool {
		return s.nextFires[due[left].ID].next.Before(s.nextFires[due[right].ID].next)
	})
	for _, automation := range due {
		fire := Fire{Kind: KindCron, At: now, ScheduledFor: s.nextFires[automation.ID].next}
		delete(s.nextFires, automation.ID)
		if _, err := s.attempt(automation.ID, fire, false); err != nil {
			s.report(err)
		}
	}
}

func (s *Scheduler) handle(cmd command) {
	switch cmd.kind {
	case commandRefresh:
		// The re-read happens at the top of the next loop iteration; the ack is
		// what tells the caller its write is now the schedule's input.
		cmd.reply <- commandResult{}
	case commandEvent:
		cmd.reply <- commandResult{err: s.handleEvent(cmd.event)}
	case commandRunNow:
		itemID, err := s.runNow(cmd.automationID)
		cmd.reply <- commandResult{itemID: itemID, err: err}
	default:
		cmd.reply <- commandResult{err: fmt.Errorf("workflow scheduler: unknown command")}
	}
}

func (s *Scheduler) handleEvent(event ItemEvent) error {
	if _, ok := EventKindForState(event.State); !ok {
		return nil
	}
	now := s.clock.Now()
	var errs []error
	for _, automation := range s.enabled {
		trigger, err := ParseTrigger(automation.Trigger)
		if err != nil {
			// Already reported by arm, which parsed the same row this iteration.
			continue
		}
		if !trigger.Matches(automation.ProjectID, event) {
			continue
		}
		matched := event
		fire := Fire{Kind: KindEvent, At: now, Event: &matched}
		if _, err := s.attempt(automation.ID, fire, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Scheduler) runNow(automationID string) (string, error) {
	// A manual fire consults no trigger: Run now works on a disabled automation
	// and on one whose stored trigger no longer parses, because neither fact
	// says anything about the workflow the human asked to run.
	fire := Fire{Kind: KindManual, At: s.clock.Now()}
	return s.attempt(automationID, fire, true)
}
