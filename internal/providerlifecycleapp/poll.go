package providerlifecycleapp

import "time"

type ProbeLoop struct {
	ProbeImmediately   bool
	TurnCompletedSince func(time.Time) bool
	Probe              func()
	Interval           time.Duration
}

func (s *Service) StartProbeLoop(loop ProbeLoop) {
	interval := loop.Interval
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	ctx := s.context()
	go func() {
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
				if !loop.TurnCompletedSince(lastPoll) {
					continue
				}
				lastPoll = time.Now()
				loop.Probe()
			}
		}
	}()
}

// NoteTurnActivity records a completed turn without probing synchronously.
func (s *Service) NoteTurnActivity(providerName string) {
	s.activityMu.Lock()
	s.activity[providerName] = time.Now()
	s.activityMu.Unlock()
}

// TurnCompletedSince reports whether a provider completed a turn after mark.
func (s *Service) TurnCompletedSince(providerName string, mark time.Time) bool {
	s.activityMu.Lock()
	last, ok := s.activity[providerName]
	s.activityMu.Unlock()
	return ok && last.After(mark)
}

// StartClaudePoll starts the activity-gated Claude usage cadence.
func (s *Service) StartClaudePoll() {
	s.startPoll(stringProviderClaude, s.claudeUsageGate().Request, false, 0)
}

// StartCodexPoll starts the activity-gated Codex usage cadence.
func (s *Service) StartCodexPoll() {
	s.startPoll(stringProviderCodex, s.codexUsageGate().Request, false, 0)
}

func (s *Service) startPoll(providerName string, probe func(), probeImmediately bool, interval time.Duration) {
	s.StartProbeLoop(ProbeLoop{
		ProbeImmediately: probeImmediately,
		TurnCompletedSince: func(mark time.Time) bool {
			return s.TurnCompletedSince(providerName, mark)
		},
		Probe: probe, Interval: interval,
	})
}

const (
	stringProviderClaude = "claude"
	stringProviderCodex  = "codex"
)
