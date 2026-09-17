//go:build providersmoke

package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/usermessage"
)

// TestProviderSmokeClaudeCrossDrainMergeFold proves the one queue shape no
// scripted provider can attest: two messages AO dispatches in SEPARATE flush
// drains while a headless Claude turn is streaming, which the real CLI merges
// into ONE transcript entry at the turn boundary (claude-wire.md §Queued-message
// consumption, boundary drain). It asserts AO folds its two rows into the
// survivor when the merged echo arrives, that the native transcript holds one
// user entry for the pair, and that a revert anchored on the survivor cuts
// exactly that entry.
//
// Haiku is deliberate: the merge is the CLI's queue behaviour, not the
// model's, and this scenario spends three real turns.
func TestProviderSmokeClaudeCrossDrainMergeFold(t *testing.T) {
	smoke := providerSmokeClaudeCase()
	smoke.model = "claude-haiku-4-5"
	app, _ := setupE2EApp(t)
	app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
	// setupE2EApp builds a bare router; the queue cannot reach the provider
	// until the flush dispatcher is wired, exactly as production's
	// initSubsystems does.
	app.configureTriageQueueCallbacks()
	collector := &providerSmokeCollector{}
	app.triage.SetEventHook(collector.observeProviderEvent)
	binary := preflightProviderBinary(t, app, smoke)
	preflightProviderAuth(t, app, smoke, binary)
	workspace := testutil.InitGitRepo(t)
	thread := e2eThread(uuid.NewString(), smoke.providerName, workspace)
	thread.Title = "Agent Overflow cross-drain merge smoke"
	thread.Model = smoke.model
	thread.ContextWindow = 200000
	thread.RuntimeMode = string(provider.RuntimeReadOnly)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerSmokeRunDeadline)
	defer cancel()
	driver := newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)
	sessionRef := func() string {
		t.Helper()
		row, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.SessionRef != "" {
			driver.trackSession(row.SessionRef)
		}
		return row.SessionRef
	}
	t.Cleanup(func() {
		sessionRef()
		if err := app.StopSession(thread.ID); err != nil {
			t.Errorf("stop smoke session: %v", err)
		}
	})

	kept, discarded := providerSmokeCodeword(), providerSmokeCodeword()
	// A long tool-free answer keeps the first turn in pure text streaming, the
	// phase in which the CLI holds every stdin message for the boundary drain.
	first, err := app.sendMessageWithOptions(ctx, thread.ID,
		"Remember this codeword for later: "+kept+". Do not use any tools. Then count from 1 to 300, one number per line, and write nothing else.",
		sendMessageOptions{SendID: uuid.NewString()})
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	waitProviderSmokeAssistantStreaming(t, ctx, app, thread.ID, first.TurnIndex)

	const textA = "Reply with exactly the word ALPHA and nothing else. Do not use any tools."
	textB := "Remember this codeword for later: " + discarded + ". Reply with exactly the word BRAVO and nothing else. Do not use any tools."
	queue := func(sendID, content string) store.Item {
		t.Helper()
		if _, err := app.sendMessageWithOptions(ctx, thread.ID, content, sendMessageOptions{
			SendID: sendID, ReconcileBySendID: true, QueueIfActive: true,
		}); err != nil {
			t.Fatalf("queue %s: %v", sendID, err)
		}
		return waitProviderSmokeDispatched(t, ctx, app, collector, thread.ID, sendID)
	}
	sendA, sendB := uuid.NewString(), uuid.NewString()
	rowA := queue(sendA, textA)
	rowB := queue(sendB, textB)
	if rowA.ID == rowB.ID {
		t.Fatalf("both messages left in one AO drain (row %s); the same-drain join ran instead of the cross-drain fold", rowA.ID)
	}
	turns, err := app.store.ListRecentTurns(thread.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		if turn.TurnIndex == first.TurnIndex && turn.CompletedAt != nil {
			t.Fatalf("first turn completed before both messages were dispatched; the CLI took them as separate turns, not a boundary merge: %+v", turn)
		}
	}

	survivor := waitProviderSmokeFold(t, ctx, app, collector, thread.ID, rowA.ID, rowB.ID)
	if want := textA + usermessage.JoinSeparator + textB; survivor.Summary != want {
		t.Fatalf("survivor summary = %q, want %q", survivor.Summary, want)
	}
	meta, err := usermessage.FromItem(survivor)
	if err != nil {
		t.Fatal(err)
	}
	if meta.SendID != sendA || strings.Join(meta.JoinedSendIDs, ",") != sendA+","+sendB {
		t.Fatalf("survivor identity: sendId=%s joined=%v, want %s + [%s %s]", meta.SendID, meta.JoinedSendIDs, sendA, sendA, sendB)
	}
	for _, sendID := range []string{sendA, sendB} {
		record, accepted, err := app.findRecordedSend(thread.ID, sendID)
		if err != nil || !accepted || record.item.ID != survivor.ID {
			t.Fatalf("send %s resolves to %+v accepted=%v err=%v, want survivor %s", sendID, record.item.ID, accepted, err, survivor.ID)
		}
	}
	// A flushed row is displayed in the turn it was queued under and answered
	// in the response turn reserved for it. Each queued send reserved its own,
	// and the fold leaves the folded member's reservation unused, so the
	// merged answer is the first turn completed after the first one.
	mergedTurn, mergedReply := waitProviderSmokeTurnReplyAfter(t, ctx, app, collector, thread.ID, first.TurnIndex)
	t.Logf("merged turn %d reply: %q", mergedTurn, providerSmokeTruncate(mergedReply))

	survivorUUID := usermessage.ReadProviderItemID(survivor.Meta)
	if survivorUUID == "" {
		t.Fatalf("survivor row %s carries no provider uuid: %s", survivor.ID, survivor.Meta)
	}
	firstUUID := usermessage.ReadProviderItemID(mustProviderSmokeItem(t, app, thread.ID, first.ID).Meta)
	prompts := readProviderSmokePromptEntries(t, sessionRef(), workspace)
	if len(prompts) != 2 || prompts[0].uuid != firstUUID || prompts[1].uuid != survivorUUID {
		t.Fatalf("transcript prompt entries = %+v, want exactly [%s %s]", prompts, firstUUID, survivorUUID)
	}
	if got := prompts[1].texts; len(got) != 2 || got[0] != textA || got[1] != textB {
		t.Fatalf("merged transcript entry blocks = %q, want [A B]", got)
	}
	t.Log("cross-drain merge: AO folded two rows into the survivor and the transcript holds one entry under its uuid")

	const recall = "What codeword did I ask you to remember? Reply with the codeword and nothing else. Do not use tools."
	result, err := app.RevertConversationAndResendMessage(ctx, thread.ID, survivor.ID, RevertAndResendOptions{SendID: uuid.NewString(), Content: recall})
	if err != nil || result.Failure != "" || result.Warning != "" || result.Cut == nil || result.Cut.Replacement == nil {
		t.Fatalf("revert to survivor: %+v err=%v", result, err)
	}
	answer := waitProviderSmokeMessage(t, ctx, app, thread.ID, result.Cut.Replacement.ID)
	assertProviderSmokeRecall(t, answer, kept, discarded)
	if _, exists, err := app.store.GetThreadItem(thread.ID, survivor.ID); err != nil || exists {
		t.Fatalf("survivor row outlived the cut: exists=%v err=%v", exists, err)
	}
	// The cut writes a fresh transcript whose entries are re-minted and
	// restamped onto AO's rows, so the first prompt is matched by its row's
	// CURRENT uuid, not the one it carried before the cut.
	firstUUID = usermessage.ReadProviderItemID(mustProviderSmokeItem(t, app, thread.ID, first.ID).Meta)
	prompts = readProviderSmokePromptEntries(t, sessionRef(), workspace)
	if len(prompts) != 2 || prompts[0].uuid != firstUUID || len(prompts[1].texts) != 1 || prompts[1].texts[0] != recall {
		t.Fatalf("transcript after cut = %+v, want exactly [first %s, recall]", prompts, firstUUID)
	}
	for _, entry := range prompts {
		for _, text := range entry.texts {
			if text == textA || text == textB {
				t.Fatalf("merged content survived the cut: %+v", prompts)
			}
		}
	}
	t.Log("revert anchored on the folded survivor cut exactly the merged entry; the native prefix answered with the kept codeword")
}

// waitProviderSmokeAssistantStreaming returns once the turn has persisted its
// first assistant text, which is the earliest point a stdin message is
// certain to reach the CLI mid-turn.
func waitProviderSmokeAssistantStreaming(t *testing.T, ctx context.Context, app *App, threadID string, turnIndex int) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		items, err := app.store.ListItems(threadID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range items {
			if row.TurnIndex == turnIndex && row.Kind == "assistant_text" && row.ParentID == "" && row.Summary != "" {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for turn %d to stream: %v", turnIndex, ctx.Err())
		}
	}
}

// waitProviderSmokeDispatched returns the user row a queued send produced once
// the flush dispatcher wrote it to the provider.
func waitProviderSmokeDispatched(t *testing.T, ctx context.Context, app *App, collector *providerSmokeCollector, threadID, sendID string) store.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, accepted, err := app.findRecordedSend(threadID, sendID)
		if err != nil {
			t.Fatal(err)
		}
		if accepted && record.dispatched {
			return record.item
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			dumpProviderSmokeMergeState(t, app, collector, threadID)
			t.Fatalf("waiting for send %s to dispatch: %v", sendID, ctx.Err())
		}
	}
}

// waitProviderSmokeFold returns the survivor once the merged echo has folded
// the earlier row into it.
func waitProviderSmokeFold(t *testing.T, ctx context.Context, app *App, collector *providerSmokeCollector, threadID, foldedID, survivorID string) store.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, foldedExists, err := app.store.GetThreadItem(threadID, foldedID)
		if err != nil {
			t.Fatal(err)
		}
		survivor, survivorExists, err := app.store.GetThreadItem(threadID, survivorID)
		if err != nil {
			t.Fatal(err)
		}
		if !survivorExists {
			t.Fatalf("survivor row %s vanished", survivorID)
		}
		if !foldedExists {
			return survivor
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			dumpProviderSmokeMergeState(t, app, collector, threadID)
			t.Fatalf("row %s was never folded into %s: %v", foldedID, survivorID, ctx.Err())
		}
	}
}

// waitProviderSmokeTurnReplyAfter waits for the lowest turn after afterTurn
// to complete with no turn still running, and returns its index and top-level
// assistant text.
func waitProviderSmokeTurnReplyAfter(t *testing.T, ctx context.Context, app *App, collector *providerSmokeCollector, threadID string, afterTurn int) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		turns, err := app.store.ListRecentTurns(threadID, 8)
		if err != nil {
			t.Fatal(err)
		}
		completed := -1
		running := false
		for _, turn := range turns {
			if turn.TurnIndex <= afterTurn {
				continue
			}
			if turn.CompletedAt == nil {
				running = true
				continue
			}
			if turn.ErrorMessage != "" || turn.StopReason == "interrupted" {
				t.Fatalf("provider turn failed: %+v", turn)
			}
			if completed < 0 || turn.TurnIndex < completed {
				completed = turn.TurnIndex
			}
		}
		if completed >= 0 && !running {
			items, err := app.store.ListItems(threadID)
			if err != nil {
				t.Fatal(err)
			}
			var answer strings.Builder
			for _, row := range items {
				if row.TurnIndex == completed && row.Kind == "assistant_text" && row.ParentID == "" {
					answer.WriteString(row.Summary)
				}
			}
			if answer.Len() > 0 {
				return completed, answer.String()
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			dumpProviderSmokeMergeState(t, app, collector, threadID)
			t.Fatalf("waiting for a turn after %d to complete: %v", afterTurn, ctx.Err())
		}
	}
}

// dumpProviderSmokeMergeState logs every row, turn, durable queue item and
// collected wire event for the thread so a stalled wait can be read.
func dumpProviderSmokeMergeState(t *testing.T, app *App, collector *providerSmokeCollector, threadID string) {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Logf("DIAGNOSTICS items: %v", err)
	}
	for _, item := range items {
		t.Logf("DIAGNOSTICS item: id=%s turn=%d idx=%d kind=%s status=%s parent=%s summary=%s meta=%s",
			item.ID, item.TurnIndex, item.ItemIndex, item.Kind, item.Status, item.ParentID,
			providerSmokeTruncate(item.Summary), providerSmokeTruncate(item.Meta))
	}
	turns, err := app.store.ListRecentTurns(threadID, 8)
	if err != nil {
		t.Logf("DIAGNOSTICS turns: %v", err)
	}
	for _, turn := range turns {
		t.Logf("DIAGNOSTICS turn: %+v", turn)
	}
	queued, err := app.store.ListFlushQueueItems(threadID)
	if err != nil {
		t.Logf("DIAGNOSTICS flush queue: %v", err)
	}
	for _, row := range queued {
		t.Logf("DIAGNOSTICS flush queue row: id=%s sendId=%s message=%s", row.ID, row.SendID, providerSmokeTruncate(row.Message))
	}
	t.Logf("DIAGNOSTICS triage queued flush items: %d", app.triage.QueuedFlushItemCount(threadID))
	entries, dropped := collector.snapshot()
	for _, entry := range entries {
		t.Logf("DIAGNOSTICS %s", entry)
	}
	if dropped > 0 {
		t.Logf("DIAGNOSTICS: %d further provider diagnostics dropped at the retention cap", dropped)
	}
}

func mustProviderSmokeItem(t *testing.T, app *App, threadID, itemID string) store.Item {
	t.Helper()
	item, exists, err := app.store.GetThreadItem(threadID, itemID)
	if err != nil || !exists {
		t.Fatalf("item %s: exists=%v err=%v", itemID, exists, err)
	}
	return item
}

// providerSmokePromptEntry is one `type:"user"` transcript entry whose content
// is text blocks (a prompt), as opposed to a tool_result carrier.
type providerSmokePromptEntry struct {
	uuid  string
	texts []string
}

func readProviderSmokePromptEntries(t *testing.T, sessionID, workspace string) []providerSmokePromptEntry {
	t.Helper()
	if sessionID == "" {
		t.Fatal("thread has no session ref")
	}
	path, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), sessionID, workspace)
	if err != nil {
		t.Fatalf("locate transcript %s: %v", sessionID, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []providerSmokePromptEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry struct {
			Type    string `json:"type"`
			UUID    string `json:"uuid"`
			IsMeta  bool   `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Type != "user" || entry.IsMeta {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil || len(blocks) == 0 {
			continue
		}
		prompt := providerSmokePromptEntry{uuid: entry.UUID}
		for _, block := range blocks {
			if block.Type != "text" {
				prompt.texts = nil
				break
			}
			prompt.texts = append(prompt.texts, block.Text)
		}
		if prompt.texts != nil {
			prompts = append(prompts, prompt)
		}
	}
	return prompts
}
