package providerlifecycleapp

import (
	"time"

	"agent-overflow/internal/provider"
)

type ProbeLoop struct {
	ProbeImmediately   bool
	TurnCompletedSince func(time.Time) bool
	// TurnInFlight, when set, makes a tick probe while a turn is running even
	// though none has completed since the last poll.
	TurnInFlight func() bool
	Probe        func()
	Interval     time.Duration
}

// StartProbeLoop runs loop until the service context ends. Close joins it.
func (s *Service) StartProbeLoop(loop ProbeLoop) {
	interval := loop.Interval
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	ctx := s.context()
	s.goMember(func() {
		if loop.ProbeImmediately {
			loop.Probe()
		}
		var lastPoll time.Time
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !loop.TurnCompletedSince(lastPoll) && (loop.TurnInFlight == nil || !loop.TurnInFlight()) {
					continue
				}
				lastPoll = time.Now()
				loop.Probe()
			}
		}
	})
}

// NoteTurnActivity records a completed turn without probing synchronously.
// Activity is kept per usage family, so a claude-tui turn drives the Claude
// poll.
func (s *Service) NoteTurnActivity(providerName string) {
	family := provider.UsageProviderFamily(providerName)
	s.activityMu.Lock()
	s.activity[family] = time.Now()
	s.activityMu.Unlock()
}

// TurnCompletedSince reports whether a provider's usage family completed a
// turn after mark.
func (s *Service) TurnCompletedSince(providerName string, mark time.Time) bool {
	family := provider.UsageProviderFamily(providerName)
	s.activityMu.Lock()
	last, ok := s.activity[family]
	s.activityMu.Unlock()
	return ok && last.After(mark)
}

// StartClaudePoll starts the activity-gated Claude usage cadence.
func (s *Service) StartClaudePoll() {
	s.startPoll(stringProviderClaude, s.claudeUsageGate().Request)
}

// StartCodexPoll starts the activity-gated Codex usage cadence.
func (s *Service) StartCodexPoll() {
	s.startPoll(stringProviderCodex, s.codexUsageGate().Request)
}

func (s *Service) startPoll(providerName string, probe func()) {
	s.StartProbeLoop(s.pollLoop(providerName, probe))
}

// pollLoop probes on a tick when the provider completed a turn since the last
// poll or has one in flight now.
func (s *Service) pollLoop(providerName string, probe func()) ProbeLoop {
	return ProbeLoop{
		TurnCompletedSince: func(mark time.Time) bool {
			return s.TurnCompletedSince(providerName, mark)
		},
		TurnInFlight: func() bool {
			return s.deps.Sessions.TurnInFlight != nil && s.deps.Sessions.TurnInFlight(providerName)
		},
		Probe: probe,
	}
}

// RequestReading asks for a reading of the selected account of providerName's
// usage family without waiting for it. The request goes through the same
// per-provider gate as the poll, so a burst of requests probes once plus at
// most one trailing run. Unknown providers and requests after Close are
// ignored.
func (s *Service) RequestReading(providerName string) {
	var gate *usageProbeGate
	switch provider.UsageProviderFamily(providerName) {
	case stringProviderClaude:
		gate = s.claudeUsageGate()
	case stringProviderCodex:
		gate = s.codexUsageGate()
	default:
		return
	}
	s.goMember(gate.Request)
}

const (
	stringProviderClaude = "claude"
	stringProviderCodex  = "codex"
)
