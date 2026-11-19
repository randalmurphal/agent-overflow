package app

import (
	"log"
	"sync"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
)

// A watch whose script keeps failing tells the person once
// (docs/specs/standing-agents.md, "Wake supervisor"). A failed run is
// otherwise only an outcome on the watch's row, and a watch whose
// credential expired looks like a quiet one.
//
// Each watch is in a failure episode from its first failed run until its
// next clean one: an interval run that exits cleanly, with or without
// output, or a line from a long-running watch. Cancelling, replacing or
// pausing the watch ends the episode too. A run the app stopped (a cancel,
// an archive, assistants turned off, a quit) is not a failure. The episode
// notifies once: at once when the watch's process could not be started,
// otherwise at the first failed run at least agentWatchFailureWindow after
// the episode's first, so a passing fault says nothing and an hourly watch
// notifies at its second failed run. Its end withdraws the notification.
// Nothing here runs the script again: the watch's own schedule and its
// restart backoff make every attempt.
//
// The episode lives in memory. A raised notice is also marked on the row
// (store.MarkAgentWatchFailureNoticed), because the notification outlives
// a restart: the boot resumes each marked episode (loadAgentWatchFailures),
// so the run that ends it after the restart still withdraws it. An episode
// that had not notified starts again after a restart.

// agentWatchFailureWindow is how long a watch keeps failing before the
// person is told; a harness boot shortens it
// (IsolationConfig.AgentWatchFailureWindow).
const agentWatchFailureWindow = 5 * time.Minute

// agentWatchFailures is every watch's open failure episode. Zero value ready.
type agentWatchFailures struct {
	// mu orders a notice's raise against its withdrawal, as they reach the
	// notification queue. Taken after the wake lifecycle lock, never before.
	mu       sync.Mutex
	episodes map[string]agentWatchEpisode
	// windowOverride replaces agentWatchFailureWindow on a harness boot.
	windowOverride time.Duration
}

type agentWatchEpisode struct {
	// since is the episode's first failed run.
	since   time.Time
	noticed bool
}

func (a *App) agentWatchFailureWindow() time.Duration {
	if window := a.agentWakes.failures.windowOverride; window > 0 {
		return window
	}
	return agentWatchFailureWindow
}

// noteAgentWatchFailed records one failed run of a watch, ended at now, and
// raises the episode's notice when it is due. A caller holds the wake
// lifecycle lock and has checked that the run was not stopped, so a cancel
// either ended the episode after this or kept this from running.
func (a *App) noteAgentWatchFailed(row store.AgentWake, failure notify.WatchFailure, now time.Time) {
	f := &a.agentWakes.failures
	f.mu.Lock()
	defer f.mu.Unlock()
	episode, open := f.episodes[row.ID]
	if !open {
		episode = agentWatchEpisode{since: now}
	}
	if !episode.noticed && (failure.Cause == notify.WatchNotStarted || now.Sub(episode.since) >= a.agentWatchFailureWindow()) {
		episode.noticed = true
		// Marked before it is raised, so the notice is never out without
		// the mark that withdraws it after a restart.
		if err := a.store.MarkAgentWatchFailureNoticed(row.ID, now.UnixMilli()); err != nil {
			log.Printf("agent wakes: the failure notice of watch %q on %s will not be withdrawn after a restart: %v", row.Label, row.ThreadID, err)
		}
		a.notifyAgentWatchFailing(row, failure)
	}
	if f.episodes == nil {
		f.episodes = make(map[string]agentWatchEpisode)
	}
	f.episodes[row.ID] = episode
}

// endAgentWatchFailure ends a watch's failure episode, if it has one, and
// withdraws its notice if it raised one.
func (a *App) endAgentWatchFailure(wakeID string) {
	f := &a.agentWakes.failures
	f.mu.Lock()
	defer f.mu.Unlock()
	episode, open := f.episodes[wakeID]
	if !open {
		return
	}
	delete(f.episodes, wakeID)
	if !episode.noticed {
		return
	}
	a.withdrawAgentWatchFailing(wakeID)
	// A mark a failed clear leaves withdraws the notice again after a
	// restart, which changes nothing.
	if err := a.store.ClearAgentWatchFailureNoticed(wakeID); err != nil {
		log.Printf("agent wakes: %v", err)
	}
}

// loadAgentWatchFailures resumes the episodes whose notice a previous
// process raised. Boot runs it before any watch runs.
func (a *App) loadAgentWatchFailures() error {
	marks, err := a.store.AgentWatchFailureNotices()
	if err != nil {
		return err
	}
	f := &a.agentWakes.failures
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.episodes == nil {
		f.episodes = make(map[string]agentWatchEpisode, len(marks))
	}
	for id, at := range marks {
		f.episodes[id] = agentWatchEpisode{since: time.UnixMilli(at), noticed: true}
	}
	return nil
}
