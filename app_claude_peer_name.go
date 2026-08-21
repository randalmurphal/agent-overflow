package main

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

// app_claude_peer_name.go — the name a thread answers to inside Claude
// Code's cross-session peer registry, and how it stays current.
//
// The registry is machine-wide and keyed on the shared
// `CLAUDE_CONFIG_DIR`, which AO deliberately does not override: an AO
// thread and a `claude` session the user ran in a terminal see each other
// in `ListAgents`. That makes the name the ONLY thing distinguishing one
// AO thread from another in a peer's list, and the CLI's own default —
// the workspace directory's basename — would give every thread of a
// project the identical name.

// claudeCrossSessionOption converts the settings shape into the provider
// transport shape, resolving the inbound policy on the way. Mirrors
// claudeThinkingOption; the packages cannot share a type because
// internal/settings must not import internal/provider.
func claudeCrossSessionOption(cross settings.ClaudeCrossSession) provider.ClaudeCrossSession {
	return provider.ClaudeCrossSession{
		Enabled: cross.Enabled,
		Inbound: cross.EffectiveInbound(),
	}
}

// peerSessionNameShortIDRunes bounds the thread-id suffix in a fallback
// name. Eight hex characters is what the rest of the app shows when it
// abbreviates an id, and it is well clear of a collision inside one
// project's thread list.
const peerSessionNameShortIDRunes = 8

// peerSessionNameForThread derives the name peers address this thread by.
//
// The thread TITLE when there is one: it is the most informative handle,
// it is what the user sees in the sidebar, and a peer asking "who else is
// working here" gets an answer in the user's own vocabulary. Titles are
// generated after the first turn, so a fresh thread falls back to
// `<project>/<short thread id>` — unique by construction, and still
// legible enough to pick out of a list.
//
// Returns the SANITIZED name, i.e. exactly what the CLI will register
// (SanitizePeerSessionName mirrors the binary's own `--name` normalizer),
// so a caller comparing it against a live session's current name is
// comparing like with like.
func (a *App) peerSessionNameForThread(t store.Thread) string {
	// UsableAsArg, not merely non-empty: a title starting with a dash
	// survives the CLI's normalizer but cannot be the value of `--name`
	// (it re-parses as a flag), so it falls through to the structural
	// form exactly as an empty title does. The argv boundary refuses it
	// too; this is what makes the thread still get a name.
	if name := claude.SanitizePeerSessionName(t.Title); claude.PeerSessionNameUsableAsArg(name) {
		return name
	}
	return claude.SanitizePeerSessionName(
		a.projectDisplayNameForThread(t) + "/" + shortThreadIDForPeerName(t.ID),
	)
}

// projectDisplayNameForThread resolves the project's user-facing name,
// falling back to the workspace directory's basename. A failed lookup is
// not worth an error: the name is a label, and a slightly less specific
// label beats refusing to name the session at all.
func (a *App) projectDisplayNameForThread(t store.Thread) string {
	if a.store != nil && t.ProjectID != "" {
		if project, err := a.store.GetProject(t.ProjectID); err == nil {
			if name := strings.TrimSpace(project.Name); name != "" {
				return name
			}
		}
	}
	for _, path := range []string{t.ProjectPath, t.WorkspacePath} {
		if base := filepath.Base(strings.TrimSpace(path)); base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return "agent-overflow"
}

// shortThreadIDForPeerName abbreviates a thread id for a fallback name.
func shortThreadIDForPeerName(id string) string {
	id = strings.TrimSpace(id)
	runes := []rune(id)
	if len(runes) > peerSessionNameShortIDRunes {
		return string(runes[:peerSessionNameShortIDRunes])
	}
	if len(runes) == 0 {
		return "thread"
	}
	return id
}

// syncPeerSessionName brings a live Claude session's peer-visible name
// back in line with its thread row, without a restart.
//
// STATELESS BY DESIGN. It re-derives the wanted name and compares it
// against what the session currently answers to, so there is no pending
// rename to persist, nothing to lose across a crash, and no ordering
// requirement between the callers. The three callers are simply the three
// moments the answer can change or become deliverable:
//
//   - a generated title landing (applyThreadTitleIfCurrent),
//   - a user renaming the thread (RenameThread),
//   - a turn completing (the provider event fan-out), which is what
//     converges a rename that arrived while the session was mid-turn.
//
// MID-TURN IS A SKIP, NOT A QUEUE. `/rename` is an ordinary stdin user
// message; sending one during a running turn would queue it behind the
// turn and land it as a mid-turn steer. Skipping costs nothing because
// the turn's own completion re-runs this function.
//
// Non-Claude threads, threads with no live session, and sessions that
// never joined the peer network all return silently — there is no name to
// correct in any of those cases.
func (a *App) syncPeerSessionName(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || a.store == nil {
		return
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.claude == nil || !sess.claude.CrossSessionEnabled() {
		// The cheapest gate first, and the one that matters: this runs on
		// every completed Claude turn, and a session with no inbox has no
		// peer-visible name to correct — so it must not pay for a thread
		// row and a project row to find that out.
		return
	}
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return
	}
	wanted := a.peerSessionNameForThread(t)
	if wanted == "" || wanted == sess.claude.PeerSessionName() {
		return
	}
	// Sampled AFTER the cheap exits so an idle-thread rename does not pay
	// for a triage lock it does not need.
	if a.triage != nil && a.triage.OpenTurnIndex(threadID) >= 0 {
		return
	}
	if err := sess.claude.RenamePeerSession(context.Background(), wanted); err != nil {
		if errors.Is(err, claude.ErrPeerRenameUnavailable) {
			// Cross-session messaging is off for this session. Expected,
			// and the overwhelmingly common case.
			return
		}
		log.Printf("app: rename peer session for %s: %v", threadID, err)
	}
}

// syncPeerSessionNameAsync runs syncPeerSessionName off the caller's
// goroutine. Used from the provider event fan-out, which is the session's
// own read loop: the rename writes to that session's stdin, and a write
// that blocks would stall the loop that drains its output.
func (a *App) syncPeerSessionNameAsync(threadID string) {
	go a.syncPeerSessionName(threadID)
}

// threadPeerInboxLive reports whether the thread's live session actually
// joined the peer registry. The cheap pre-gate for the per-turn caller: it is
// the same condition syncPeerSessionName opens with, hoisted so the common
// case (cross-session messaging off) costs a map lookup instead of a
// goroutine on every completed turn.
func (a *App) threadPeerInboxLive(threadID string) bool {
	sess, ok := a.sessionManager().get(strings.TrimSpace(threadID))
	return ok && sess.claude != nil && sess.claude.CrossSessionEnabled()
}
