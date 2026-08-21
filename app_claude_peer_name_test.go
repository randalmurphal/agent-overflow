package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

// The peer registry is machine-wide and shared with the user's own
// terminal `claude` sessions, and the CLI's default name is the cwd
// basename — which would name every thread of a project identically. The
// derivation exists to make a thread pickable out of a peer's ListAgents.
func TestPeerSessionNameForThreadPrefersTheTitle(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.CreateProject(store.Project{
		ID: "p1", Path: "/repos/agent-overflow", Name: "Agent Overflow", Slug: "agent-overflow",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	thread := store.Thread{
		ID:          "9f2c81ab-4d5e-4c11-9a03-77b3ce2f0011",
		ProjectID:   "p1",
		ProjectPath: "/repos/agent-overflow",
		Title:       "Harbor kite wiring",
	}
	if got := app.peerSessionNameForThread(thread); got != "Harbor kite wiring" {
		t.Fatalf("peerSessionNameForThread = %q, want the thread title", got)
	}

	// Titles are generated after the first turn, so a fresh thread needs a
	// name that is unique by construction and still legible in a list.
	thread.Title = ""
	if got := app.peerSessionNameForThread(thread); got != "Agent Overflow/9f2c81ab" {
		t.Fatalf("untitled thread name = %q, want <project>/<short id>", got)
	}
}

// A project row that cannot be read is not worth refusing to name the
// session over — the name is a label. It degrades to the workspace
// basename, which is still project-specific.
func TestPeerSessionNameForThreadFallsBackToTheWorkspaceBasename(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := store.Thread{
		ID:            "abcdef0123456789",
		ProjectID:     "missing-project",
		WorkspacePath: "/repos/some-checkout",
	}
	if got := app.peerSessionNameForThread(thread); got != "some-checkout/abcdef01" {
		t.Fatalf("peerSessionNameForThread = %q, want the workspace basename", got)
	}
}

// The derived name must already be what the CLI will register, because
// the rename skip compares AO's recorded name against the next candidate.
// A title that only sanitizes down to nothing must not produce an empty
// name — the fallback carries it.
func TestPeerSessionNameForThreadIsAlreadySanitized(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := store.Thread{
		ID:            "0123456789abcdef",
		WorkspacePath: "/repos/demo",
		Title:         "  A\ttitle​with invisibles  ",
	}
	got := app.peerSessionNameForThread(thread)
	if got != claude.SanitizePeerSessionName(got) {
		t.Fatalf("peerSessionNameForThread returned an unsanitized name: %q", got)
	}
	if strings.Contains(got, "\t") || strings.Contains(got, "​") {
		t.Fatalf("name kept invisibles: %q", got)
	}

	thread.Title = "​\t"
	if got := app.peerSessionNameForThread(thread); got != "demo/01234567" {
		t.Fatalf("invisible-only title = %q, want the fallback", got)
	}
}

// The transport shape resolves the policy on the way across, so an
// enabled session can never carry an empty inbound value — empty would
// land the CLI in its mode-parity hold, which drops peer messages
// silently after a timeout.
func TestClaudeCrossSessionOptionResolvesThePolicy(t *testing.T) {
	got := claudeCrossSessionOption(settings.ClaudeCrossSession{Enabled: true})
	if !got.Enabled || got.Inbound != settings.ClaudeCrossSessionInboundAccept {
		t.Fatalf("claudeCrossSessionOption(enabled, unset) = %+v", got)
	}
	got = claudeCrossSessionOption(settings.ClaudeCrossSession{Enabled: true, Inbound: settings.ClaudeCrossSessionInboundRefuse})
	if got.Inbound != settings.ClaudeCrossSessionInboundRefuse {
		t.Fatalf("claudeCrossSessionOption(refuse) = %+v", got)
	}
	// Disabled says nothing at all.
	if got := claudeCrossSessionOption(settings.ClaudeCrossSession{Inbound: "accept"}); got.Enabled || got.Inbound != "" {
		t.Fatalf("claudeCrossSessionOption(disabled) = %+v, want zero", got)
	}
}

// syncPeerSessionName is called from three places that can all fire on a
// thread with no live session at all. It must be inert there rather than
// panicking on a nil session or a missing row.
func TestSyncPeerSessionNameIsInertWithoutALiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	app.syncPeerSessionName("")
	app.syncPeerSessionName("no-such-thread")
}
