package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
)

// Transcript tails a branch-aware scan would turn into a resume cursor.
// Each one is a shape whose cursor the CLI has rejected or could reject.
var coldResumeTranscripts = map[string]string{
	"normal tail": `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hello"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hi"}]}}
`,
	"orphaned advisor tail": `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"advise"}}
{"type":"assistant","uuid":"a-bash","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}
{"type":"assistant","uuid":"a-advisor","parentUuid":"a-bash","message":{"id":"m1","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}
`,
	"crash tail with a dangling tool_use": `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"run it"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"running"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}
`,
}

// A cold resume of an ordinary Claude thread hands the CLI the session
// alone. The CLI then picks its own default leaf and applies its own
// resume filters, so no tail shape can make it reject the start.
func TestColdClaudeResumePassesNoResumeCursor(t *testing.T) {
	t.Parallel()
	offBranch := wireCapture(t, "claude/session_api_error_offbranch.jsonl")
	shapes := map[string]string{"api_error off-branch tail": string(offBranch)}
	for name, jsonl := range coldResumeTranscripts {
		shapes[name] = jsonl
	}
	for name, jsonl := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			workspace := t.TempDir()
			const sessionID = "cold-resume-session"
			writeClaudeProjectSession(t, testProviderHome(t, app), workspace, sessionID, jsonl)

			thread := e2eThread("thread-cold-resume", string(provider.Claude), workspace)
			thread.SessionRef = sessionID
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			argvPath := filepath.Join(t.TempDir(), "argv.txt")
			if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath)}); err != nil {
				t.Fatal(err)
			}
			if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			t.Cleanup(func() { stopSessionChecked(t, app, thread.ID) })

			argv := spawnedClaudeArgv(t, argvPath)
			if !hasArgPair(argv, "--resume", sessionID) {
				t.Errorf("argv does not resume %s: %v", sessionID, argv)
			}
			if slices.Contains(argv, "--resume-session-at") || slices.Contains(argv, "--fork-session") {
				t.Errorf("an ordinary resume passed a cursor or a fork: %v", argv)
			}
		})
	}
}

// A pinned fork's first start still cuts the source at its stored pin,
// repaired against the CLI's resume filters.
func TestPinnedClaudeForkStartPassesItsResumeCursor(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	const sourceID = "fork-source-session"
	writeClaudeProjectSession(t, testProviderHome(t, app), workspace, sourceID,
		coldResumeTranscripts["crash tail with a dangling tool_use"])

	fork := e2eThread("thread-pinned-fork", string(provider.Claude), workspace)
	fork.PendingForkRef = sourceID
	// a2 is the dangling tool_use the CLI's filters drop; a1 survives.
	fork.PendingForkResumeAt = "a2"
	if err := app.store.CreateThread(fork); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath)}); err != nil {
		t.Fatal(err)
	}
	if err := app.startSessionTakingLock(context.Background(), fork.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { stopSessionChecked(t, app, fork.ID) })

	argv := spawnedClaudeArgv(t, argvPath)
	if !hasArgPair(argv, "--resume", sourceID) || !hasArgPair(argv, "--resume-session-at", "a1") || !slices.Contains(argv, "--fork-session") {
		t.Errorf("pinned fork start argv = %v, want --resume %s --resume-session-at a1 --fork-session", argv, sourceID)
	}
}

// coldResumedMidTurnSource starts a cold resume of a Claude thread over
// midTurnSourceJSONL with the given mock CLI, then lays down an open turn
// so a tail fork of it is a mid-turn fork of a live session.
func coldResumedMidTurnSource(t *testing.T, binary string) (*App, store.Thread, *claude.Session) {
	t.Helper()
	app, _ := setupE2EApp(t)
	fixture := newMidTurnForkFixture(t, testProviderHome(t, app), "mid-turn-session", midTurnSourceJSONL)
	source := e2eThread("thread-cold-resume-fork", string(provider.Claude), fixture.workspace)
	source.SessionRef = fixture.sessionID
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.startSessionTakingLock(context.Background(), source.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { stopSessionChecked(t, app, source.ID) })
	sess, ok := app.activeClaudeSession(source.ID)
	if !ok {
		t.Fatal("no live Claude session after StartSession")
	}
	seedMidTurnSourceRows(t, app.store, source.ID)
	return app, source, sess
}

// Right after a cold resume the live session has seen no row and reports
// no leaf, so a mid-turn fork takes its cut from the transcript on disk.
func TestMidTurnForkRightAfterColdResumeCutsAtTheDiskLeaf(t *testing.T) {
	t.Parallel()
	app, source, sess := coldResumedMidTurnSource(t, writeArgvRecordingClaude(t, filepath.Join(t.TempDir(), "argv.txt")))
	if leaf := sess.CanonicalLeafUUID(); leaf != "" {
		t.Fatalf("live leaf after a cold resume = %q, want empty until the CLI emits a row", leaf)
	}
	forked, err := app.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}
	if forked.PendingForkRef != source.SessionRef || forked.PendingForkResumeAt != "a1" {
		t.Errorf("fork pin = %q@%q, want %q@a1 from the disk scan", forked.PendingForkRef, forked.PendingForkResumeAt, source.SessionRef)
	}
}

// Once the resumed session emits a row, the live leaf is ahead of the
// file and the mid-turn fork pins it.
func TestMidTurnForkAfterResumedRowsCutsAtTheLiveLeaf(t *testing.T) {
	t.Parallel()
	script := "#!/bin/sh\nprintf '%s\\n' '" +
		`{"type":"assistant","uuid":"a-live","parentUuid":"a1","session_id":"mid-turn-session","parent_tool_use_id":null,"message":{"id":"m-live","role":"assistant","content":[{"type":"text","text":"more"}]}}` +
		"'\ncat >/dev/null\n"
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "claude-live-row.sh"), script)
	app, source, sess := coldResumedMidTurnSource(t, binary)
	deadline := time.Now().Add(5 * time.Second)
	for sess.CanonicalLeafUUID() != "a-live" {
		if time.Now().After(deadline) {
			t.Fatalf("live leaf = %q, want a-live from the emitted row", sess.CanonicalLeafUUID())
		}
		time.Sleep(10 * time.Millisecond)
	}
	forked, err := app.ForkThread(t.Context(), source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}
	if forked.PendingForkRef != source.SessionRef || forked.PendingForkResumeAt != "a-live" {
		t.Errorf("fork pin = %q@%q, want %q@a-live from the live session", forked.PendingForkRef, forked.PendingForkResumeAt, source.SessionRef)
	}
}
