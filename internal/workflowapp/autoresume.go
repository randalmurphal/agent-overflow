package workflowapp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/workflow/engine"
)

const (
	AutoResumeBootDelay  = 30 * time.Second
	AutoResumeRetryDelay = 5 * time.Minute
	MaxResumeDelay       = 30 * 24 * time.Hour
)

// Timer is the live, replaceable half of a durable auto-resume schedule.
type Timer interface {
	Stop() bool
	Reset(time.Duration) bool
}

type autoResumeState struct {
	mu       sync.Mutex
	timers   map[string]Timer
	newTimer func(time.Duration, func()) Timer
}

func (s *Service) autoResumeTimer(delay time.Duration, fire func()) Timer {
	if s.autoResume.newTimer != nil {
		return s.autoResume.newTimer(delay, fire)
	}
	return time.AfterFunc(delay, fire)
}

// ConfigureAutoResumeTimer installs the timer constructor used by deterministic
// runtime tests. It must be called before schedules are armed.
func (s *Service) ConfigureAutoResumeTimer(newTimer func(time.Duration, func()) Timer) {
	s.autoResume.mu.Lock()
	s.autoResume.newTimer = newTimer
	s.autoResume.mu.Unlock()
}

// ConfigureClock installs the deterministic application clock used by tests.
// Runtime wiring must complete this before starting the engine or arming work.
func (s *Service) ConfigureClock(now func() time.Time) {
	if now == nil {
		s.deps.Now = time.Now
		return
	}
	s.deps.Now = now
}

func (s *Service) Now() time.Time { return s.deps.Now() }

func (s *Service) SetAutoResume(itemID string, at time.Time) error {
	if err := s.deps.Store.SetWorkItemAutoResumeAt(itemID, at.UnixMilli()); err != nil {
		return err
	}
	s.armAutoResume(itemID, at.Sub(s.deps.Now()))
	return nil
}

func (s *Service) armAutoResume(itemID string, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	s.autoResume.mu.Lock()
	if existing, ok := s.autoResume.timers[itemID]; ok {
		existing.Stop()
	}
	if s.autoResume.timers == nil {
		s.autoResume.timers = make(map[string]Timer)
	}
	s.autoResume.timers[itemID] = s.autoResumeTimer(delay, func() { s.fireAutoResume(itemID) })
	s.autoResume.mu.Unlock()
}

func (s *Service) ArmAutoResume(itemID string, delay time.Duration) {
	s.armAutoResume(itemID, delay)
}

func (s *Service) ClearAutoResume(itemID string) {
	s.autoResume.mu.Lock()
	timer, armed := s.autoResume.timers[itemID]
	if armed {
		timer.Stop()
		delete(s.autoResume.timers, itemID)
	}
	s.autoResume.mu.Unlock()
	if !armed {
		return
	}
	if err := s.deps.Store.SetWorkItemAutoResumeAt(itemID, 0); err != nil {
		s.deps.Logf("workflow auto-resume %s: clear schedule: %v", itemID, err)
	}
}

func (s *Service) fireAutoResume(itemID string) {
	at, err := s.deps.Store.WorkItemAutoResumeAt(itemID)
	if err != nil {
		s.deps.Logf("workflow auto-resume %s: read schedule: %v", itemID, err)
		return
	}
	if at == 0 {
		return
	}
	item, err := s.deps.Store.GetWorkItemSummary(itemID)
	if err != nil {
		s.deps.Logf("workflow auto-resume %s: load run: %v", itemID, err)
		return
	}
	if !AutoResumable(item.State, item.Reason) {
		s.ClearAutoResume(itemID)
		return
	}
	if s.deps.ResumeRun == nil {
		s.deps.Logf("workflow auto-resume %s: resume unavailable (retrying in %s)", itemID, AutoResumeRetryDelay)
		s.armAutoResume(itemID, AutoResumeRetryDelay)
		return
	}
	if err := s.deps.ResumeRun(context.Background(), itemID); err != nil {
		s.deps.Logf("workflow auto-resume %s: resume (retrying in %s): %v", itemID, AutoResumeRetryDelay, err)
		s.armAutoResume(itemID, AutoResumeRetryDelay)
	}
}

func (s *Service) FireAutoResume(itemID string) { s.fireAutoResume(itemID) }

func (s *Service) SweepAutoResumes() {
	resumes, err := s.deps.Store.ListWorkItemAutoResumes()
	if err != nil {
		s.deps.Logf("workflow auto-resume sweep: %v", err)
		return
	}
	now := s.deps.Now()
	for _, resume := range resumes {
		delay := time.UnixMilli(resume.At).Sub(now)
		if delay < AutoResumeBootDelay {
			delay = AutoResumeBootDelay
		}
		s.armAutoResume(resume.ItemID, delay)
	}
}

func (s *Service) StopAutoResumes() {
	s.autoResume.mu.Lock()
	for itemID, timer := range s.autoResume.timers {
		timer.Stop()
		delete(s.autoResume.timers, itemID)
	}
	s.autoResume.mu.Unlock()
}

func AutoResumable(state, reason string) bool {
	return engine.State(state) == engine.StateNeedsHuman && engine.ContinuableReason(engine.Reason(reason))
}

func ContinuableReasonList() string {
	reasons := engine.ContinuableReasons()
	names := make([]string, len(reasons))
	for index, reason := range reasons {
		names[index] = string(reason)
	}
	return strings.Join(names, ", ")
}

func (s *Service) ScheduleResume(itemID, at string) (string, error) {
	now := s.Now()
	resumeAt, err := ParseResumeAt(at, now)
	if err != nil {
		return "", err
	}
	item, err := s.deps.Store.GetWorkItemSummary(itemID)
	if err != nil {
		return "", err
	}
	if !AutoResumable(item.State, item.Reason) {
		return "", fmt.Errorf(
			"schedule workflow resume %s: run is %s(%s); a scheduled resume continues a parked attempt, which applies to %s",
			itemID, item.State, item.Reason, ContinuableReasonList(),
		)
	}
	if err := s.SetAutoResume(itemID, resumeAt); err != nil {
		return "", err
	}
	return resumeAt.Local().Format(time.RFC3339), nil
}

func ParseResumeAt(value string, now time.Time) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("schedule workflow resume: a time is required, as RFC 3339 (2026-08-15T19:56:00Z) or a duration (+36h)")
	}
	var resumeAt time.Time
	if strings.HasPrefix(trimmed, "+") {
		delay, err := time.ParseDuration(strings.TrimPrefix(trimmed, "+"))
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule workflow resume: %q is not a duration: %w", trimmed, err)
		}
		if delay <= 0 {
			return time.Time{}, fmt.Errorf("schedule workflow resume: %q is not in the future", trimmed)
		}
		resumeAt = now.Add(delay)
	} else {
		parsed, err := time.Parse(time.RFC3339, trimmed)
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule workflow resume: %q is neither RFC 3339 nor a duration like +36h: %w", trimmed, err)
		}
		resumeAt = parsed
	}
	if !resumeAt.After(now) {
		return time.Time{}, fmt.Errorf("schedule workflow resume: %s is not in the future", resumeAt.Local().Format(time.RFC3339))
	}
	if resumeAt.Sub(now) > MaxResumeDelay {
		return time.Time{}, fmt.Errorf(
			"schedule workflow resume: %s is more than %s away; cancel the run instead of parking it that long",
			resumeAt.Local().Format(time.RFC3339), MaxResumeDelay,
		)
	}
	return resumeAt, nil
}

func (s *Service) AutoResumeRegistered(itemID string) bool {
	s.autoResume.mu.Lock()
	defer s.autoResume.mu.Unlock()
	_, ok := s.autoResume.timers[itemID]
	return ok
}

func (s *Service) RegisteredAutoResumeTimer(itemID string) (Timer, bool) {
	s.autoResume.mu.Lock()
	defer s.autoResume.mu.Unlock()
	timer, ok := s.autoResume.timers[itemID]
	return timer, ok
}

func (s *Service) AutoResumeTimerCount() int {
	s.autoResume.mu.Lock()
	defer s.autoResume.mu.Unlock()
	return len(s.autoResume.timers)
}
