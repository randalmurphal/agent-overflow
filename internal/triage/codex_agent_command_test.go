package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A Codex agent's background command (unifiedExecStartup) and its stdin
// interactions are the agent's history: saved under the agent, in the
// agent's turn, whether or not the root is running; nothing once a cut
// removed the agent; and a member only of the agent execution that started
// the command.

func seedAgentSpawnRow(t *testing.T, st *store.Store, threadID, spawnID string, turnIndex int) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := st.InsertItem(store.Item{
		ID: spawnID, ThreadID: threadID, TurnIndex: turnIndex, Kind: itemKindToolCall,
		Role: "assistant", Status: statusCompleted, Summary: "spawn_agent", ToolName: "collab_agent",
		Meta:      `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"]}}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed spawn %s: %v", spawnID, err)
	}
}

func startAgentCommand(t *testing.T, r *Router, threadID, spawnID, itemID, pid, childTurnID string) {
	t.Helper()
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID,
		ItemType: "commandExecution", TurnID: childTurnID, ParentToolUseID: spawnID,
		Meta: buildUnifiedExecStartMeta(t, pid, "sleep 60"), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent command start %s: %v", itemID, err)
	}
}

func finishAgentCommand(t *testing.T, r *Router, threadID, spawnID, itemID, pid, childTurnID string) {
	t.Helper()
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: itemID,
		ItemType: "commandExecution", TurnID: childTurnID, ParentToolUseID: spawnID,
		Content: "done\n", Meta: buildUnifiedExecCompleteMeta(t, "completed", pid, "sleep 60", 0),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent command complete %s: %v", itemID, err)
	}
}

func agentInteraction(t *testing.T, r *Router, threadID, spawnID, pid, childTurnID string) {
	t.Helper()
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: threadID, TurnID: childTurnID, ParentToolUseID: spawnID,
		Meta: terminalInteractionMetaBlob(t, pid, "y\n"), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent interaction %s: %v", pid, err)
	}
}

func codexTrackerPresent(r *Router, threadID, itemID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackgroundIfPresent(threadID)
	return state != nil && state.unifiedExec[itemID] != nil
}

func rowsOfKind(t *testing.T, st *store.Store, threadID, kind string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var out []store.Item
	for _, item := range items {
		if item.Kind == kind {
			out = append(out, item)
		}
	}
	return out
}

func endRootTurn(t *testing.T, r *Router, threadID string, turnIndex int) {
	t.Helper()
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: threadID + ":" + strconv.Itoa(turnIndex),
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prior) })
	return &logs
}

// An agent's command that completes after a conversation cut removed the
// agent writes nothing, and says so, whatever the cut's timing: the scope
// gone before the completion, with the root idle or running a turn that
// reuses the cut index; or the cut landing while the completion is held
// right before its write. A cut after the write removes the row with the
// agent.
func TestCodexAgentCommandCompletionWritesNothingOnceACutRemovedItsAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// cutAt says when the cut runs: "before" the completion, "held"
		// while the completion waits at its write, "after" the write.
		cutAt string
		// rootRunning opens a root turn on the cut index before the
		// completion arrives.
		rootRunning bool
	}{
		{name: "scope cut before the completion, root idle", cutAt: "before"},
		{name: "scope cut before the completion, root running the reused turn", cutAt: "before", rootRunning: true},
		{name: "cut while the completion is held at its write", cutAt: "held"},
		{name: "cut after the write", cutAt: "after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, emissions := newTestRouter(t)
			logs := captureLogs(t)
			createCodexBackgroundTestThread(t, st, "t1")
			seedOpenTurn(t, r, st, "t1", 0)
			endRootTurn(t, r, "t1", 0)
			seedOpenTurn(t, r, st, "t1", 1)
			seedAgentSpawnRow(t, st, "t1", "spawn-agent", 1)
			startAgentCommand(t, r, "t1", "spawn-agent", "child-cmd", "pid-child", "child-turn")
			endRootTurn(t, r, "t1", 1)

			cut := func() {
				if _, _, err := st.DeleteConversationFromTurn("t1", 1); err != nil {
					t.Errorf("cut: %v", err)
				}
				r.ForgetToolCallLinks("t1")
			}
			switch tc.cutAt {
			case "before":
				cut()
				if tc.rootRunning {
					seedOpenTurn(t, r, st, "t1", 1)
				}
			case "held":
				held := false
				r.SetScopedWriteGate(func(item store.Item) {
					if item.ID == "child-cmd" && !held {
						held = true
						cut()
					}
				})
				defer r.SetScopedWriteGate(nil)
			}
			emissions.reset()
			finishAgentCommand(t, r, "t1", "spawn-agent", "child-cmd", "pid-child", "child-turn")
			if codexTrackerPresent(r, "t1", "child-cmd") {
				t.Fatalf("completion kept the command's tracker")
			}
			if tc.cutAt == "after" {
				if _, found, err := st.GetThreadItem("t1", "child-cmd"); err != nil || !found {
					t.Fatalf("command row before the cut: found=%v err=%v", found, err)
				}
				cut()
			}

			if _, found, err := st.GetThreadItem("t1", "child-cmd"); err != nil || found {
				t.Fatalf("command row survives its agent's cut: found=%v err=%v", found, err)
			}
			if tc.cutAt == "after" {
				return
			}
			for _, item := range filterItemEventUpserts(emissions.snapshot()) {
				if item.ID == "child-cmd" {
					t.Fatalf("a dropped command emitted an upsert: %+v", item)
				}
			}
			if !strings.Contains(logs.String(), "dropping child-cmd on thread t1: scope spawn-agent is no longer in history") {
				t.Fatalf("dropped command not logged: %q", logs.String())
			}
		})
	}
}

// The store refuses an agent's command whose scope row is not a tool_call,
// or whose id is already stored (here on another turn, as the newest-turn
// fallback before the scoped write could leave it): the completion writes
// nothing, leaves the stored row as it was, and says so.
func TestCodexAgentCommandTheStoreRefusesWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope string
		seed  func(t *testing.T, st *store.Store)
		want  string
	}{
		{
			name:  "scope is a text row",
			scope: "note",
			seed: func(t *testing.T, st *store.Store) {
				if err := st.InsertItem(store.Item{ID: "note", ThreadID: "t1", TurnIndex: 0, ItemIndex: 5, Kind: "assistant_text", Role: "assistant", Status: statusCompleted, Summary: "hello"}); err != nil {
					t.Fatal(err)
				}
			},
			want: `parent kind "assistant_text" is not tool_call`,
		},
		{
			name:  "command already stored on a later turn",
			scope: "spawn-agent",
			seed: func(t *testing.T, st *store.Store) {
				seedAgentSpawnRow(t, st, "t1", "spawn-agent", 0)
				if err := st.WithSubagentCard("t1", "spawn-agent", func(card *store.SubagentCard) error {
					return st.InsertItem(store.Item{ID: "child-cmd", ThreadID: "t1", TurnIndex: 1, ItemIndex: 1, Kind: itemKindToolCall, Role: "assistant", Status: statusCompleted, Summary: "old", ParentID: "spawn-agent", SubagentCard: card})
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: `t1/child-cmd is already stored`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, _ := newTestRouter(t)
			logs := captureLogs(t)
			createCodexBackgroundTestThread(t, st, "t1")
			seedOpenTurn(t, r, st, "t1", 0)
			endRootTurn(t, r, "t1", 0)
			seedOpenTurn(t, r, st, "t1", 1)
			tc.seed(t, st)
			before, existed, err := st.GetThreadItem("t1", "child-cmd")
			if err != nil {
				t.Fatal(err)
			}
			startAgentCommand(t, r, "t1", tc.scope, "child-cmd", "pid-child", "child-turn")
			finishAgentCommand(t, r, "t1", tc.scope, "child-cmd", "pid-child", "child-turn")

			after, found, err := st.GetThreadItem("t1", "child-cmd")
			if err != nil || found != existed || (found && (after.TurnIndex != before.TurnIndex || after.ItemIndex != before.ItemIndex || after.Summary != before.Summary)) {
				t.Fatalf("command row changed: %+v -> %+v (found=%v err=%v)", before, after, found, err)
			}
			if codexTrackerPresent(r, "t1", "child-cmd") {
				t.Fatalf("refused completion kept the command's tracker")
			}
			if !strings.Contains(logs.String(), "triage: refusing child-cmd on thread t1") || !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("refusal not logged with %q: %q", tc.want, logs.String())
			}
		})
	}
}

// A non-empty stdin interaction with an agent's background terminal is the
// agent's history like the agent's command: on the agent's turn while the
// root is idle or running a later turn, tagged with the execution Codex
// reports it in (the one that wrote the stdin), and nothing once a cut
// removed the agent. A
// top-level interaction is unchanged: TestCodexUnifiedExecStartWaitsForTypedTerminalInteraction.
func TestCodexAgentTerminalInteractionLandsOnAgentTurn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rootRunning bool
		cut         bool
		// held cuts the agent while the interaction waits at its write.
		held bool
	}{
		{name: "root idle"},
		{name: "root running a later turn", rootRunning: true},
		{name: "agent cut", cut: true},
		{name: "agent cut while the interaction is held at its write", cut: true, held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, _ := newTestRouter(t)
			logs := captureLogs(t)
			createCodexBackgroundTestThread(t, st, "t1")
			seedOpenTurn(t, r, st, "t1", 0)
			seedAgentSpawnRow(t, st, "t1", "spawn-agent", 0)
			startAgentCommand(t, r, "t1", "spawn-agent", "child-cmd", "pid-child", "child-turn")
			endRootTurn(t, r, "t1", 0)
			cut := func() {
				if _, _, err := st.DeleteConversationFromTurn("t1", 0); err != nil {
					t.Errorf("cut: %v", err)
				}
				r.ForgetToolCallLinks("t1")
			}
			switch {
			case tc.rootRunning:
				seedOpenTurn(t, r, st, "t1", 1)
			case tc.held:
				r.SetScopedWriteGate(func(store.Item) { cut() })
				defer r.SetScopedWriteGate(nil)
			case tc.cut:
				cut()
			}

			agentInteraction(t, r, "t1", "spawn-agent", "pid-child", "child-turn")

			rows := rowsOfKind(t, st, "t1", string(provider.ItemTerminalInteraction))
			if tc.cut {
				if len(rows) != 0 {
					t.Fatalf("interaction saved for a cut agent: %+v", rows)
				}
				if !strings.Contains(logs.String(), "scope spawn-agent is no longer in history") {
					t.Fatalf("dropped interaction not logged: %q", logs.String())
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("interaction rows = %+v, want one", rows)
			}
			row := rows[0]
			if row.TurnIndex != 0 || row.ParentID != "spawn-agent" {
				t.Fatalf("interaction turn=%d parent=%q, want the agent's turn 0 under spawn-agent", row.TurnIndex, row.ParentID)
			}
			if tag := decodeItemMetaMap(t, row.Meta)[store.CodexExecutionTurnMetaKey]; tag != "child-turn" {
				t.Fatalf("interaction execution tag = %v, want child-turn", tag)
			}
		})
	}
}

// Two executions of one Codex agent: execution A leaves a background
// command running and finishes; a follow-up starts execution B; A's command
// exits during B. The command is in the agent pane at the position it
// completed, and the background tray's tool list shows it, but it is in no
// execution's card or count: A's card was saved before it exited and never
// changes, and B did not start it. B's stdin to another command A started
// is B's: in B's aggregates, not A's.
func TestCodexAgentCommandFromAnEarlierExecutionIsNotCountedByALaterOne(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")

	childTurnStatus(t, r, "t1", "spawn", "child", "A", "running")
	childToolCall(t, r, "t1", "spawn", "a1")
	startAgentCommand(t, r, "t1", "spawn", "cmd-a", "pid-a", "A")
	startAgentCommand(t, r, "t1", "spawn", "cmd-a2", "pid-a2", "A")
	childTurnStatus(t, r, "t1", "spawn", "child", "A", "completed")
	deliverMailbox(t, r, "t1", "spawn", rootAnswer(t, "final-a", "First answer."))
	cardA, found, err := st.GetThreadItem("t1", "complete:spawn:turn:A")
	if err != nil || !found {
		t.Fatalf("A's card: found=%v err=%v", found, err)
	}
	if progress, _ := persistedProgressFor(t, st, "t1", cardA.ID); progress.ToolUses != 1 {
		t.Fatalf("A's card counts %d tools, want 1", progress.ToolUses)
	}
	completeParentTurn(t, r, "t1")

	seedOpenTurn(t, r, st, "t1", 1)
	childTurnStatus(t, r, "t1", "spawn", "child", "B", "running")
	childToolCall(t, r, "t1", "spawn", "b1")
	finishAgentCommand(t, r, "t1", "spawn", "cmd-a", "pid-a", "A")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 1 {
		t.Fatalf("B's live count = %d after A's command exited, want 1", got)
	}
	childToolCall(t, r, "t1", "spawn", "b2")
	// B writes to A's other command, still running: Codex reports the
	// interaction in B, the execution that wrote it.
	agentInteraction(t, r, "t1", "spawn", "pid-a2", "B")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 2 {
		t.Fatalf("B's live count = %d, want 2", got)
	}
	childTurnStatus(t, r, "t1", "spawn", "child", "B", "completed")
	deliverMailbox(t, r, "t1", "spawn", rootAnswer(t, "final-b", "Second answer."))

	command, found, err := st.GetThreadItem("t1", "cmd-a")
	if err != nil || !found {
		t.Fatalf("A's command row: found=%v err=%v", found, err)
	}
	if command.ParentID != "spawn" || command.TurnIndex != 0 || decodeItemMetaMap(t, command.Meta)[store.CodexExecutionTurnMetaKey] != "A" {
		t.Fatalf("A's command row = %+v, want under spawn on turn 0 tagged A", command)
	}
	const interactionID = "interacted:pid-a2:0:0"
	interaction, found, err := st.GetThreadItem("t1", interactionID)
	if err != nil || !found {
		t.Fatalf("B's interaction row: found=%v err=%v", found, err)
	}
	if interaction.ParentID != "spawn" || interaction.TurnIndex != 0 {
		t.Fatalf("B's interaction row = %+v, want under spawn on turn 0", interaction)
	}

	cardB, found, err := st.GetThreadItem("t1", "complete:spawn:turn:B")
	if err != nil || !found {
		t.Fatalf("B's card: found=%v err=%v", found, err)
	}
	if progress, _ := persistedProgressFor(t, st, "t1", cardB.ID); progress.ToolUses != 2 {
		t.Fatalf("B's card counts %d tools, want 2", progress.ToolUses)
	}
	var bMeta struct {
		Start       int    `json:"codex_execution_child_start_index"`
		End         int    `json:"codex_execution_child_end_index"`
		Descendants int    `json:"subagentDescendantCount"`
		Latest      string `json:"subagentLatestChildSummary"`
	}
	if err := json.Unmarshal([]byte(cardB.Meta), &bMeta); err != nil {
		t.Fatalf("decode B's card: %v", err)
	}
	if command.ItemIndex <= bMeta.Start || command.ItemIndex > bMeta.End {
		t.Fatalf("fixture: A's command at %d is outside B's range (%d, %d]", command.ItemIndex, bMeta.Start, bMeta.End)
	}
	if interaction.ItemIndex <= bMeta.Start || interaction.ItemIndex > bMeta.End {
		t.Fatalf("fixture: B's interaction at %d is outside B's range (%d, %d]", interaction.ItemIndex, bMeta.Start, bMeta.End)
	}
	inB := 0
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ParentID == "spawn" && item.TurnIndex == 0 && item.ItemIndex > bMeta.Start && item.ItemIndex <= bMeta.End && item.ID != "cmd-a" {
			inB++
		}
	}
	if bMeta.Descendants != inB || bMeta.Latest != interaction.Summary {
		t.Fatalf("B's card aggregates = %d rows, latest %q; want %d rows without A's command, latest %q", bMeta.Descendants, bMeta.Latest, inB, interaction.Summary)
	}

	again, _, err := st.GetThreadItem("t1", cardA.ID)
	if err != nil || again.Meta != cardA.Meta || again.UpdatedAt != cardA.UpdatedAt {
		t.Fatalf("A's saved card changed: %v\n%s\n->\n%s", err, cardA.Meta, again.Meta)
	}

	slice := func(selection store.TimelineSelection) []string {
		t.Helper()
		page, err := st.ListThreadSliceAround(context.Background(), "t1", "", 40, 30, selection, store.AgentMessagesAreMembers)
		if err != nil {
			t.Fatalf("read %+v: %v", selection, err)
		}
		ids := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			ids = append(ids, item.ID)
		}
		return ids
	}
	contains := func(ids []string, id string) bool {
		for _, got := range ids {
			if got == id {
				return true
			}
		}
		return false
	}
	if ids := slice(store.TimelineSelection{ScopeRootID: "spawn", DigestItemID: cardB.ID}); contains(ids, "cmd-a") || !contains(ids, "b1") || !contains(ids, "b2") {
		t.Fatalf("B's card body = %v, want b1 and b2 without cmd-a", ids)
	}
	if ids := slice(store.TimelineSelection{ScopeRootID: "spawn", DigestItemID: cardA.ID}); contains(ids, "cmd-a") || !contains(ids, "a1") {
		t.Fatalf("A's card body = %v, want a1 without cmd-a", ids)
	}
	pane := slice(store.TimelineSelection{ScopeRootID: "spawn"})
	if order := strings.Join(pane, ","); !strings.Contains(order, "b1,cmd-a,b2,"+interactionID) {
		t.Fatalf("agent pane = %v, want A's command between b1 and b2 where it completed, then B's interaction", pane)
	}
	if ids := slice(store.TimelineSelection{ScopeRootID: "spawn", Tools: true}); !contains(ids, "cmd-a") {
		t.Fatalf("tray tool list = %v, want A's command", ids)
	}
	if tag := decodeItemMetaMap(t, interaction.Meta)[store.CodexExecutionTurnMetaKey]; tag != "B" {
		t.Fatalf("B's interaction is tagged %v, want B, the execution that wrote it", tag)
	}
}
