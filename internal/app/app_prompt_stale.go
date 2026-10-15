package app

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"agent-overflow/internal/promptoverride"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/settings"
)

// The per-thread "system prompt changed" banner (docs/decisions.md
// 2026-10-14).
//
// A prompt change never applies to a running session: it applies at the
// thread's next session start. A live thread whose next start would use a
// different settings-level prompt is flagged here, and the flag is pushed as
// a thread-scoped `provider:status` of kind `prompt_stale` the frontend shows
// with a Restart button and a dismiss. Like the stale-binary flag
// (app_provider_binary_watch.go) it is a claim about one running process: it
// is pinned to the session token that earned it, withdrawn the moment that
// session leaves, and mirrored into GetThreadLiveState for a reconnecting
// pane.
//
// "Differs" is decided on the stored, unrendered override text
// (SessionOptions.SystemPromptOverrideSource), the same value the spawn
// records. A rendered comparison would flag every session whose workspace's
// git state moved under a {{GIT_BLOCK}}.

// promptStaleMessage is the banner text.
const promptStaleMessage = "The system prompt for this thread changed. Restart the session to use it."

// appPromptStaleState is the set of flagged threads. Process-local: the flag
// describes a running subprocess and dies with the process.
type appPromptStaleState struct {
	// mu serializes every reconcile, dismissal and withdrawal end to end,
	// emission included, so two triggers racing cannot publish an enter and
	// a withdraw in the opposite order from the state they leave behind.
	mu sync.Mutex
	// revision is the counter every transition into or within stale draws
	// from. One process-wide counter keeps each thread's revisions
	// increasing without a per-thread map, and a revision is never reused,
	// so a dismissal of one change can never hide a later one.
	revision int
	stale    map[string]promptStaleEntry
}

// promptStaleEntry is one flagged thread.
type promptStaleEntry struct {
	provider string
	// token is the session the flag describes.
	token string
	// next is the stored override text the thread's next start would use,
	// "" for none. A change to it while flagged is a new revision.
	next     string
	revision int
	// dismissed is the revision the user dismissed, 0 for none. The banner
	// shows while revision != dismissed.
	dismissed int
}

func (e promptStaleEntry) visible() bool { return e.revision != e.dismissed }

// promptStaleVerdict is one live thread's answer: whether its session runs
// a prompt other than what its next start would use, and that next prompt.
type promptStaleVerdict struct {
	provider string
	token    string
	next     string
	stale    bool
}

// reconcileStalePromptSessions recomputes the flag for every live session.
// Triggers: a settings change to either override key (an edit in the app or
// a prompt file reload), and the start of any session. Cheap: one settings
// snapshot, then per live session a thread row read and a slug comparison;
// nothing is rendered.
func (a *App) reconcileStalePromptSessions() {
	if a.settings == nil || a.store == nil {
		return
	}
	snapshot := a.settings.Get()
	ps := &a.promptStale
	ps.mu.Lock()
	defer ps.mu.Unlock()
	live := a.sessionManager().runtime.Snapshot()
	for threadID, entry := range live {
		verdict, ok := a.promptStaleVerdict(threadID, entry, snapshot)
		if !ok {
			continue
		}
		a.applyPromptStaleVerdictLocked(threadID, verdict)
	}
	for threadID, flagged := range ps.stale {
		if current, ok := live[threadID]; ok && current.Token == flagged.token {
			continue
		}
		a.withdrawPromptStaleLocked(threadID, flagged)
	}
}

// reconcileStalePromptSession recomputes the flag for one thread. Used where
// only that thread's answer can have moved: its session just started, or
// its model changed.
func (a *App) reconcileStalePromptSession(threadID string) {
	if a.settings == nil || a.store == nil {
		return
	}
	snapshot := a.settings.Get()
	ps := &a.promptStale
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, live := a.sessionManager().get(threadID)
	if !live {
		if flagged, ok := ps.stale[threadID]; ok {
			a.withdrawPromptStaleLocked(threadID, flagged)
		}
		return
	}
	verdict, ok := a.promptStaleVerdict(threadID, entry, snapshot)
	if !ok {
		return
	}
	a.applyPromptStaleVerdictLocked(threadID, verdict)
}

// promptStaleVerdict answers one live session. ok is false when the thread
// row cannot be read; the flag is then left as it was, with a log line, and
// the next trigger retries.
func (a *App) promptStaleVerdict(threadID string, entry session, snapshot settings.Settings) (promptStaleVerdict, bool) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("thread %s: prompt change check: load thread: %v", threadID, err)
		return promptStaleVerdict{}, false
	}
	thread = a.sanitizeThreadModelSettings(thread)
	verdict := promptStaleVerdict{provider: entry.Provider, token: entry.Token}
	launch := entry.LaunchOptions
	// Match skips an entry whose text is empty, which is also what an entry
	// whose file has a Problem loads as: neither is an override.
	if matched, ok := promptoverride.Match(snapshot.PromptOverridesForProvider(thread.Provider), thread.Provider, thread.Model); ok {
		verdict.next = matched.Prompt
	}
	verdict.stale = verdict.next != launch.SystemPromptOverrideSource
	return verdict, true
}

// applyPromptStaleVerdictLocked moves one thread's flag to the verdict and
// emits the transition. Caller holds promptStale.mu.
func (a *App) applyPromptStaleVerdictLocked(threadID string, verdict promptStaleVerdict) {
	ps := &a.promptStale
	flagged, had := ps.stale[threadID]
	if !verdict.stale {
		if had {
			a.withdrawPromptStaleLocked(threadID, flagged)
		}
		return
	}
	if had && flagged.token == verdict.token && flagged.next == verdict.next {
		return
	}
	ps.revision++
	next := promptStaleEntry{
		provider: verdict.provider,
		token:    verdict.token,
		next:     verdict.next,
		revision: ps.revision,
	}
	if ps.stale == nil {
		ps.stale = make(map[string]promptStaleEntry, 1)
	}
	ps.stale[threadID] = next
	a.emitProviderStatus(providerstatus.Event{
		Provider:   next.provider,
		Kind:       providerstatus.KindPromptStale,
		ThreadID:   threadID,
		Message:    promptStaleMessage,
		Actionable: true,
		Revision:   next.revision,
	})
}

// withdrawPromptStaleLocked drops one thread's flag and withdraws its banner.
// Caller holds promptStale.mu.
func (a *App) withdrawPromptStaleLocked(threadID string, flagged promptStaleEntry) {
	delete(a.promptStale.stale, threadID)
	if len(a.promptStale.stale) == 0 {
		a.promptStale.stale = nil
	}
	a.emitSessionBannerWithdraw(threadID, flagged.provider, providerstatus.KindPromptStale)
}

// emitSessionBannerWithdraw withdraws one kind of thread-scoped session
// banner: "ready" is the clear signal, and WithdrawKind names the banner, so
// a pane showing both session banners drops only this one.
func (a *App) emitSessionBannerWithdraw(threadID, providerName, kind string) {
	a.emitProviderStatus(providerstatus.Event{
		Provider:     providerName,
		Status:       "ready",
		ThreadID:     threadID,
		WithdrawKind: kind,
	})
}

// withdrawPromptStaleForEndedSession withdraws a thread's banner when the
// session that earned it has left the session map, at the moment it leaves
// rather than at the next trigger: a dead session shows no banner. A flag
// whose session is still live (a credential-only disconnect) is kept.
func (a *App) withdrawPromptStaleForEndedSession(threadID string) {
	ps := &a.promptStale
	ps.mu.Lock()
	defer ps.mu.Unlock()
	flagged, ok := ps.stale[threadID]
	if !ok {
		return
	}
	if current, live := a.sessionManager().get(threadID); live && current.Token == flagged.token {
		return
	}
	a.withdrawPromptStaleLocked(threadID, flagged)
}

// promptStaleRevision answers GetThreadLiveState: whether the thread's
// banner shows, and its revision. A dismissed banner, or one whose session
// has been replaced since the flag was set, does not show.
func (a *App) promptStaleRevision(threadID string) (bool, int) {
	ps := &a.promptStale
	ps.mu.Lock()
	flagged, ok := ps.stale[threadID]
	ps.mu.Unlock()
	if !ok || !flagged.visible() {
		return false, 0
	}
	if current, live := a.sessionManager().get(threadID); !live || current.Token != flagged.token {
		return false, 0
	}
	return true, flagged.revision
}

// dismissPromptStale records the user's dismissal of the thread's current
// revision and withdraws the banner. A no-op when no banner shows. The
// withdraw is emitted under the same lock as every enter, so it cannot land
// after the enter of a later revision.
func (a *App) dismissPromptStale(threadID string) {
	ps := &a.promptStale
	ps.mu.Lock()
	defer ps.mu.Unlock()
	flagged, ok := ps.stale[threadID]
	if !ok || !flagged.visible() {
		return
	}
	flagged.dismissed = flagged.revision
	ps.stale[threadID] = flagged
	a.emitSessionBannerWithdraw(threadID, flagged.provider, providerstatus.KindPromptStale)
}

// DismissSessionBanner hides a thread's session banner of one kind until
// the condition behind it changes again: `prompt_stale` until the prompt
// the next start would use changes again, `binary_stale` until the
// installed CLI version changes again. Either way the change still applies
// when the session next restarts. Every attached client gets the withdraw.
//
//ao:scope threads:operate
func (a *App) DismissSessionBanner(threadID, kind string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("dismiss session banner: empty thread id")
	}
	switch kind {
	case providerstatus.KindPromptStale:
		a.dismissPromptStale(threadID)
	case providerstatus.KindBinaryStale:
		a.dismissBinaryStale(threadID)
	default:
		return fmt.Errorf("dismiss session banner: unknown banner kind %q", kind)
	}
	return nil
}
