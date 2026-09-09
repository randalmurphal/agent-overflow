package codex

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

const settingsPushTimeout = 5 * time.Second

type settingsSyncState struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	running bool
	closing bool
	pending ThreadSettingsPush
}

// QueueThreadSettings coalesces optional between-turn synchronization. Requested
// settings are already installed by ApplyLiveUpdate; callers never wait for IPC.
// One worker belongs to this session and Close cancels and joins it.
func (s *Session) QueueThreadSettings(push ThreadSettingsPush) {
	if push.Empty() {
		return
	}
	s.settingsSync.mu.Lock()
	defer s.settingsSync.mu.Unlock()
	if s.settingsSync.closing || s.closing.Load() {
		return
	}
	pending := &s.settingsSync.pending
	pending.Model = pending.Model || push.Model
	pending.Effort = pending.Effort || push.Effort
	pending.ServiceTier = pending.ServiceTier || push.ServiceTier
	if s.settingsSync.running {
		return
	}
	s.settingsSync.running = true
	s.settingsSync.wg.Add(1)
	go s.syncThreadSettings()
}

func (s *Session) syncThreadSettings() {
	defer s.settingsSync.wg.Done()
	for {
		s.settingsSync.mu.Lock()
		push := s.settingsSync.pending
		s.settingsSync.pending = ThreadSettingsPush{}
		if push.Empty() || s.settingsSync.closing {
			s.settingsSync.running = false
			s.settingsSync.mu.Unlock()
			return
		}
		s.settingsSync.mu.Unlock()

		ctx, cancel := context.WithTimeout(s.ctx, settingsPushTimeout)
		err := s.PushThreadSettings(ctx, push)
		cancel()

		if err != nil && !s.closing.Load() {
			var rejection *RPCError
			if errors.As(err, &rejection) {
				s.emitThreadSettingsEchoError(err.Error())
			} else {
				log.Printf("codex: settings sync failed; next turn will reassert the selection: %v", err)
			}
		}
	}
}
