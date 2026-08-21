package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// queuedRow is one row in the fake app-server's provider queue.
type queuedRow struct {
	id       string
	clientID string
	text     string
}

func (r queuedRow) wire() string {
	return fmt.Sprintf(`{"id":%q,"input":[{"type":"text","text":%q}],"clientUserMessageId":%q}`,
		r.id, r.text, r.clientID)
}

// codexQueueBinaryOpts shapes the fake app-server below. The zero value is
// the ordinary queue-native server with an empty queue.
type codexQueueBinaryOpts struct {
	// silentAdd accepts a `thread/queue/add` and never answers it — the
	// ambiguous shape, where the row may be persisted (and may already have
	// been dispatched) but the ack is lost.
	silentAdd bool
	// rows seeds the provider queue. `list` reports what is left and `delete`
	// removes from it, so a test can assert on the queue AFTER a purge rather
	// than only on the delete frames. `add` does not append: a test that cares
	// what a later list shows seeds the row itself, which is also how the
	// ambiguous-add case states "the row landed, the ack did not".
	rows []queuedRow
	// listError refuses every `thread/queue/list` with an internal error, on a
	// server that DOES have the route. The shape a reconcile has to survive:
	// the version gate is open, so AO is expected to be able to answer "which
	// of these rows are mine", and the one call that could is failing.
	listError bool
	// deleteRefused names one submission id whose `thread/queue/delete` is
	// answered with an internal error while every other row deletes normally.
	// That asymmetry is the partial purge: PurgeQueue goes row by row, so a
	// rollback can abort with some rows already out of codex's queue and
	// nothing left to run them.
	deleteRefused string
	// preQueue makes the handshake report a build from BEFORE the queue
	// family shipped, and refuses every `thread/queue/*` method with the
	// `-32601` a server with no such route answers. Both halves together:
	// the version is what closes `ThreadQueueNative`, and the refusal is what
	// turns a caller that ignored the gate into a visible failure rather than
	// a silently empty queue.
	preQueue bool
}

// writeCodexProviderQueueBinary is a fake app-server that reports a 0.149
// build on the handshake — which is what opens the `thread/queue` gate — and
// records every request frame to capturePath so a test can prove which verb
// the flush dispatcher chose.
//
// It also answers `thread/resume` and `thread/fork`, because a rollback cuts
// history through a throwaway session spawned from the same binary and the
// queue purge that rides that connection is only observable on a run that gets
// all the way through. The resumed thread states no `historyMode`, so it is a
// legacy-history thread and the cut is the fork — the purge is what these
// tests are about, not which cut ran.
// Returns the binary path and the path of the queue-state file, which a test
// reads back to assert what is LEFT in the provider's queue.
func writeCodexProviderQueueBinary(
	t *testing.T, threadID, capturePath string, opts codexQueueBinaryOpts,
) (binary, queueStatePath string) {
	t.Helper()
	addBranch := `printf '{"jsonrpc":"2.0","id":%s,"result":{"queuedSubmission":{"id":"sub-1","input":[{"type":"text","text":"queued"}],"clientUserMessageId":"user:7:flush:1"}}}\n' "$id"`
	if opts.silentAdd {
		addBranch = `: # the row lands, the ack never does`
	}
	// The version the handshake reports, and — for a pre-queue build — the
	// blanket refusal that stands in for routes this app-server does not have.
	userAgent := "codex_cli_rs/0.149.0 (test)"
	queueRefusalBranch := ""
	if opts.preQueue {
		userAgent = "codex_cli_rs/0.147.0 (test)"
		queueRefusalBranch = `
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/'; then
        /bin/echo "$line" >> ` + fmt.Sprintf("%q", capturePath) + `
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
        continue
    fi`
	}
	// The queue's whole state is this file: one `id|json` line per row, which
	// is what lets the mock's list shrink as deletes land without a JSON
	// parser in sh.
	queueState := filepath.Join(t.TempDir(), "queue-state")
	var seed strings.Builder
	for _, row := range opts.rows {
		fmt.Fprintf(&seed, "%s|%s\n", row.id, row.wire())
	}
	if err := os.WriteFile(queueState, []byte(seed.String()), 0o644); err != nil {
		t.Fatalf("write queue state: %v", err)
	}
	listBranch := fmt.Sprintf(
		`rows=$(/usr/bin/cut -d'|' -f2- %q | /usr/bin/paste -sd, -)
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[%%s],"nextCursor":null}}\n' "$id" "$rows"`,
		queueState)
	if opts.listError {
		listBranch = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"thread store is busy"}}\n' "$id"`
	}
	deleteRefusalBranch := ""
	if opts.deleteRefused != "" {
		deleteRefusalBranch = fmt.Sprintf(`
        if [ "$sub" = %q ]; then
            printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32603,"message":"thread store is busy"}}\n' "$id"
            continue
        fi`, opts.deleteRefused)
	}
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"userAgent":"%s"}}\n' "$id"
        continue
    fi%s
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/add"'; then
        /bin/echo "$line" >> %q
        %s
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/list"'; then
        /bin/echo "$line" >> %q
        %s
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/delete"'; then
        /bin/echo "$line" >> %q
        sub=$(/bin/echo "$line" | /usr/bin/grep -o '"queuedSubmissionId":"[^"]*"' | /usr/bin/cut -d'"' -f4)%s
        if /usr/bin/grep -q "^$sub|" %q; then
            /usr/bin/grep -v "^$sub|" %q > %q.tmp
            /bin/mv %q.tmp %q
            printf '{"jsonrpc":"2.0","id":%%s,"result":{"deleted":true}}\n' "$id"
        else
            printf '{"jsonrpc":"2.0","id":%%s,"result":{"deleted":false}}\n' "$id"
        fi
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        /bin/echo "$line" >> %q
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":[]}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        cut=$(/bin/echo "$line" | /usr/bin/grep -o '"lastTurnId":"[^"]*"' | /usr/bin/cut -d'"' -f4)
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"forked-%s","turns":[{"id":"%%s"}]}}}\n' "$id" "$cut"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/steer"'; then
        /bin/echo "$line" >> %q
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, userAgent, queueRefusalBranch, threadID, capturePath, addBranch, capturePath, listBranch, capturePath,
		deleteRefusalBranch, queueState, queueState, queueState, queueState, queueState,
		capturePath, threadID, threadID, capturePath)

	path := filepath.Join(t.TempDir(), "codex-provider-queue.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex provider-queue binary: %v", err)
	}
	return path, queueState
}

// readQueueState reports the submission ids still in a mock's queue.
func readQueueState(t *testing.T, statePath string) []string {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read queue state: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, _, _ := strings.Cut(line, "|")
		ids = append(ids, id)
	}
	return ids
}

// foreignQueueClientID is what `codex queue --thread` writes: a v7 uuid, never
// an AO item id (rust-v0.149.0 codex-rs/tui/src/session_queue_commands.rs:48).
const foreignQueueClientID = "0199e3a1-0000-7000-8000-000000000001"

func newProviderQueueSession(t *testing.T, thread store.Thread, capturePath string) *codex.Session {
	t.Helper()
	return newProviderQueueSessionWith(t, thread, capturePath, codexQueueBinaryOpts{})
}

func newProviderQueueSessionWith(
	t *testing.T, thread store.Thread, capturePath string, opts codexQueueBinaryOpts,
) *codex.Session {
	t.Helper()
	binary, _ := writeCodexProviderQueueBinary(t, thread.ID+"-codex", capturePath, opts)
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  binary,
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	codex.SetActiveTurnIDForTest(sess, "active-turn")
	return sess
}

// TestFlushDispatch_ProviderQueueSessionQueuesInsteadOfSteering is the app
// half of the two-queues-are-mutually-exclusive rule. On a 0.148+ app-server
// a mid-turn message is handed to the PROVIDER's queue, which starts a turn
// of its own when the thread goes idle — so AO must neither steer it into the
// running turn nor place its row there.
func TestFlushDispatch_ProviderQueueSessionQueuesInsteadOfSteering(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-provider-queue")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-7", ThreadID: thread.ID, TurnIndex: 7, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	// The running turn already holds content. Without it "next turn" and
	// "active turn" resolve to the same number and the assertion below could
	// not tell the two branches apart.
	if err := app.store.InsertItem(store.Item{
		ID: "assistant:7:0", ThreadID: thread.ID, TurnIndex: 7, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "streaming",
		Summary: "working", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSession(t, thread, capture)
	if !codexSess.ThreadQueueNative() {
		t.Fatal("ThreadQueueNative() = false; the 0.149 handshake should have opened the gate")
	}
	sess := session{provider: string(provider.Codex), codex: codexSess}

	// Placement: the NEXT turn, not the running one. A steered message is
	// context for a turn already underway; a queued one opens a turn of its
	// own, and placing its row in the active turn puts the prompt below the
	// answer to the PREVIOUS prompt.
	got, active, err := app.resolveFlushTurnPlacement(thread.ID, sess)
	if err != nil {
		t.Fatalf("resolveFlushTurnPlacement: %v", err)
	}
	if active {
		t.Error("activeAtResolution = true; a provider-queued message joins no running turn")
	}
	if got != 8 {
		t.Errorf("turn index = %d, want 8 (its own turn); 7 is the running turn a steer would join", got)
	}

	// Verb: thread/queue/add carrying AO's optimistic row id, never turn/steer.
	handoff, err := app.dispatchFlushToProvider(
		sess, thread.ID, "queued", provider.SendOptions{}, "user:7:flush:1",
	)
	if err != nil {
		t.Fatalf("dispatchFlushToProvider: %v", err)
	}
	if handoff != codexQueueHandoffConfirmed {
		t.Errorf("handoff = %v, want confirmed; an acked add owns the message", handoff)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	frames := string(raw)
	if strings.Contains(frames, "turn/steer") {
		t.Fatalf("a provider-queue session steered; the two queues must be mutually exclusive:\n%s", frames)
	}
	if !strings.Contains(frames, `"method":"thread/queue/add"`) {
		t.Fatalf("no thread/queue/add on the wire:\n%s", frames)
	}
	if !strings.Contains(frames, `"clientUserMessageId":"user:7:flush:1"`) {
		t.Fatalf("queue add did not carry AO's optimistic row id:\n%s", frames)
	}
}

// TestFlushDispatch_ProviderQueuedMessageSurvivesSessionDeath is finding 9(a).
// Once `thread/queue/add` succeeds the message is codex's, not AO's: it is
// durable in the app-server's SQLite and `QueuedItemService::on_thread_idle`
// dispatches it on the next resume. Restoring it to the composer draft — what
// every OTHER unconfirmed flush row gets — would leave the user holding a
// draft of a prompt that is also scheduled to run, and sending it would run
// the message twice.
func TestFlushDispatch_ProviderQueuedMessageSurvivesSessionDeath(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-provider-queue-death")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-7", ThreadID: thread.ID, TurnIndex: 7, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSession(t, thread, capture)
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "queue-token",
		codex:    codexSess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:owned", Message: "queued", Payload: json.RawMessage(`{}`)},
	})
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("no pending-send marker after the queue add; the row is not awaiting an echo")
	}

	if requeued := app.restoreUnconfirmedQueueOnSessionDeath(thread.ID); len(requeued) != 0 {
		t.Fatalf("session death requeued a provider-owned message: %+v", requeued)
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if strings.TrimSpace(draft.Content) != "" {
		t.Fatalf("draft content = %q; a message the provider queue owns must not come back as a draft", draft.Content)
	}
	if restored := emittedQueueRestored(rec); len(restored) != 0 {
		t.Fatalf("queue restored events = %+v, want none", restored)
	}
	// Ownership is DURABLE, not a process-local marker: the row itself
	// survives the death carrying it, which is what a fresh process reads
	// after an app restart.
	row, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1")
	if err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	}
	if !found {
		t.Fatal("the provider-owned row was deleted by the session death; nothing records the message any more")
	}
	if !itemmeta.IsProviderQueued(row.Meta) {
		t.Errorf("row meta = %q, want the provider-queued marker", row.Meta)
	}
}

// TestFlushDispatch_AmbiguousQueueAddIsResolvedByTheListNotARetry is finding
// 10. `thread/queue/add` has no idempotency key upstream, so a lost ack must
// never be answered with a second send — the message would run twice. The
// dispatcher asks the queue instead.
func TestFlushDispatch_AmbiguousQueueAddIsResolvedByTheListNotARetry(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-provider-queue-timeout")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		silentAdd: true,
		// The row DID land — which is exactly what makes the timeout
		// ambiguous rather than a failure.
		rows: []queuedRow{{id: "sub-1", clientID: "user:7:flush:1", text: "queued"}},
	})
	codex.SetRequestTimeoutForTest(codexSess, 300*time.Millisecond)
	sess := session{provider: string(provider.Codex), codex: codexSess}

	// Not an error: the message is queued, the caller must keep its pending
	// confirmation and wait for the provider echo.
	handoff, err := app.dispatchFlushToProvider(
		sess, thread.ID, "queued", provider.SendOptions{}, "user:7:flush:1",
	)
	if err != nil {
		t.Fatalf("an ambiguous queue/add surfaced as a dispatch failure: %v", err)
	}
	if handoff != codexQueueHandoffConfirmed {
		t.Errorf("handoff = %v, want confirmed; the read-back found the row, which is what resolves the ambiguity", handoff)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	frames := strings.Split(strings.TrimSpace(string(raw)), "\n")
	adds, lists := 0, 0
	for _, frame := range frames {
		switch {
		case strings.Contains(frame, `"method":"thread/queue/add"`):
			adds++
		case strings.Contains(frame, `"method":"thread/queue/list"`):
			lists++
		}
	}
	if adds != 1 {
		t.Errorf("thread/queue/add frames = %d, want exactly 1; a retry is a second turn", adds)
	}
	if lists == 0 {
		t.Error("the ambiguous add was never resolved against thread/queue/list")
	}
	if strings.Contains(string(raw), "turn/steer") {
		t.Error("the timeout fell back to a steer; the two queues must stay mutually exclusive")
	}
	// Ownership is not recorded here at all — the caller
	// (dispatchFlushItem) stamped it on the persisted row BEFORE this call,
	// which is what makes every ambiguous return path above, including the
	// one where the read-back itself fails, keep the fact.
}

// TestRearmCodexProviderQueueClaimsSplitsOwnershipOnResume is finding 9(b).
// The claim ledger is in-memory — it describes turns THIS connection will see
// — so a session that comes back to a non-empty provider queue has to relearn
// which rows are its own, or the next dispatch of AO's own message is stamped
// `external-queue`.
func TestRearmCodexProviderQueueClaimsSplitsOwnershipOnResume(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-provider-queue-rearm")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	app.ensureTriageRouter()
	// The row AO persisted and marked before its `thread/queue/add`. It is the
	// ownership token: the id on the wire is only AO's if THIS app's store has
	// the row that put it there.
	queuedMeta, err := itemmeta.MarkProviderQueued("")
	if err != nil {
		t.Fatalf("MarkProviderQueued: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7, "mine", queuedMeta)

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		// One row AO added before the old session died, and one a
		// `codex queue --thread` run wrote — which mints a v7 uuid, never an
		// AO item id.
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:7:flush:1", text: "mine"},
			{id: "sub-2", clientID: foreignQueueClientID, text: "theirs"},
		},
	})

	app.rearmCodexProviderQueueClaims(thread.ID, codexSess)

	claims := codex.SelfQueuedClaimIDsForTest(codexSess)
	if len(claims) != 1 || claims[0] != "user:7:flush:1" {
		t.Fatalf("re-armed claims = %v, want only AO's own row id", claims)
	}
}

// TestProviderQueuedRowOwnershipSurvivesAnAppRestart is finding 12. The
// process that made the `thread/queue/add` may never come back: the message
// is durable in codex's SQLite and dispatches on the next resume, so the
// record of who owns it has to be durable too. It lives on the persisted row
// (itemmeta.MarkProviderQueued, stamped before the add), and a fresh process
// reads it back off the queue listing — otherwise the dispatched turn's
// `userMessage` echo finds no pending send to claim and lands as injected
// provider context beneath a row the user can already see.
func TestProviderQueuedRowOwnershipSurvivesAnAppRestart(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-provider-queue-restart")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.ensureTriageRouter()

	queuedMeta, err := itemmeta.MarkProviderQueued("")
	if err != nil {
		t.Fatalf("MarkProviderQueued: %v", err)
	}
	// The same marker on a row whose echo ALREADY arrived. It must not be
	// re-armed: the row is history, and a second claim would let the next
	// echo on the thread overwrite it.
	echoedMeta, err := itemmeta.MarkProviderQueued(`{"provider_item_id":"item-42"}`)
	if err != nil {
		t.Fatalf("MarkProviderQueued: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7, "mine", queuedMeta)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:2", 7, "already ran", echoedMeta)

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:7:flush:1", text: "mine"},
			{id: "sub-2", clientID: "user:7:flush:2", text: "already ran"},
			{id: "sub-3", clientID: foreignQueueClientID, text: "theirs"},
		},
	})

	// Twice on purpose: a session restart that did NOT drain the FIFO must
	// not leave two entries claiming one row.
	app.rearmCodexProviderQueueClaims(thread.ID, codexSess)
	app.rearmCodexProviderQueueClaims(thread.ID, codexSess)

	head, ok := app.triage.PeekPendingSendHeadForTest(thread.ID)
	if !ok {
		t.Fatal("no pending send after the re-arm; the queue dispatch's echo has no row to claim")
	}
	if head.AOItemID != "user:7:flush:1" {
		t.Fatalf("re-armed head = %q, want the queued row AO owns", head.AOItemID)
	}
	if head.TurnIndex != 7 {
		t.Errorf("re-armed turn index = %d, want the row's own turn (the app-server opens it for the dispatch)", head.TurnIndex)
	}

	drained := app.triage.DrainUnconfirmedFlushItems(thread.ID)
	if len(drained) != 1 {
		t.Fatalf("pending entries = %d, want exactly 1 (the foreign row and the already-echoed row are not AO's to claim): %+v", len(drained), drained)
	}
	if drained[0].QuietItem == nil {
		t.Fatal("the re-armed entry carries no quiet row; the echo would persist a second copy of the message")
	}
	// And the durable marker is what the NEXT session death reads, in a
	// process that never saw the add.
	if kept := dropProviderQueuedFlushItems(thread.ID, drained); len(kept) != 0 {
		t.Fatalf("session death would restore a provider-owned message to the draft: %+v", kept)
	}
}

// TestConversationRollbackForgetsTheProviderThreadCostWhenTheThreadMoves is
// finding 16. `provider_thread_cost` is keyed by the AO thread id but
// DESCRIBES the Codex thread the figure was read from. A rollback that forks
// repoints this thread at a new Codex thread carrying a shorter history, and
// the turn-0 branch drops the reference entirely — either way the stored total
// belongs to a thread this one no longer is, and nothing would correct it
// until some later settled turn happened to re-read.
//
// The in-place revert is the case that must NOT forget, and it is excluded by
// construction rather than by a second fixture: it keeps the thread id, so the
// forget is gated on SessionRef actually changing.
func TestConversationRollbackForgetsTheProviderThreadCostWhenTheThreadMoves(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-cost-forget", "codex", workspace)
	thread.SessionRef = "provider-cost-forget"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	seedMessageAnchor(t, app.store, thread.ID, "user:0", 0, "", "")
	if err := app.store.PutProviderThreadCost(store.ProviderThreadCost{
		ThreadID: thread.ID,
		// The provider thread the figure was read from (migration v68). It is
		// what the rollback moves the thread away from, and what makes the row
		// unreadable from that moment whether or not the delete lands.
		SessionRef:    thread.SessionRef,
		Provider:      string(provider.Codex),
		CostUSDMicros: 4_200_000,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("PutProviderThreadCost: %v", err)
	}

	// Rolling back to the row that opens turn 0 keeps no provider history at
	// all: the reference clears and the next send starts a fresh Codex thread.
	// No app-server is spawned for it, which is also why this needs no binary.
	if err := rollbackToMessage(app, thread.ID, "user:0"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	rolled, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if rolled.SessionRef != "" {
		t.Fatalf("SessionRef after a turn-0 rollback = %q, want cleared", rolled.SessionRef)
	}
	if _, found, err := app.store.GetProviderThreadCost(thread.ID); err != nil {
		t.Fatalf("GetProviderThreadCost: %v", err)
	} else if found {
		t.Error("the provider cost survived a rollback that dropped the Codex thread it described")
	}
}

// TestConversationRollbackPurgesTheProviderQueue is finding 7. AO's own
// flushqueue is cleared in process by the rollback, but a message already
// handed to `thread/queue/add` is a row in codex's SQLite: it outlives the
// session stop and the app-server's idle hook dispatches it on the next
// resume — re-running, onto a thread the user just truncated, exactly the
// message they rolled back. The purge has to run while the connection is
// still live, so it happens BEFORE the stop.
func TestConversationRollbackPurgesTheProviderQueue(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-queue-purge", "codex", workspace)
	thread.SessionRef = "provider-queue-purge"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	binary, queueState := writeCodexProviderQueueBinary(t, thread.SessionRef, capture, codexQueueBinaryOpts{
		// One row AO queued, one a `codex queue --thread` run wrote. Both
		// would re-run on resume, so both go.
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:1:flush:1", text: "mine"},
			{id: "sub-2", clientID: foreignQueueClientID, text: "theirs"},
		},
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	sess, err := codex.NewSession(context.Background(), thread.ID, codex.Config{
		Binary:         binary,
		Model:          "test-model",
		WorkDir:        workspace,
		ResumeThreadID: thread.SessionRef,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex),
		token:    "queue-purge-token",
		codex:    sess,
	})
	// What a real session start does, and what makes the purge able to tell
	// the two rows apart in its log.
	app.rearmCodexProviderQueueClaims(thread.ID, sess)

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	var deleted []string
	for _, frame := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(frame, `"method":"thread/queue/delete"`) {
			continue
		}
		for _, id := range []string{"sub-1", "sub-2"} {
			if strings.Contains(frame, `"queuedSubmissionId":"`+id+`"`) {
				deleted = append(deleted, id)
			}
		}
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted queued submissions = %v, want both rows dropped before the stop:\n%s", deleted, raw)
	}
	if left := readQueueState(t, queueState); len(left) != 0 {
		t.Fatalf("provider queue after the rollback = %v, want empty", left)
	}
	// And the session really is gone afterwards — the purge cannot have
	// happened after the stop, because there is no connection to run it on.
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("Codex session still active after the rollback")
	}
}

// TestConversationRollbackPurgesTheProviderQueueWithNoLiveSession is the
// no-session half of the same hazard. Nothing is running to purge through, but
// the rollback has to resume the thread anyway to cut its history — and an
// in-place revert keeps the thread id, so a row left behind runs a rolled-back
// message on the user's next send. The purge rides the connection the cut
// already opened; nothing extra is spawned.
func TestConversationRollbackPurgesTheProviderQueueWithNoLiveSession(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-queue-purge-cold", "codex", workspace)
	thread.SessionRef = "provider-queue-purge-cold"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	binary, queueState := writeCodexProviderQueueBinary(t, thread.SessionRef, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:1:flush:1", text: "mine"},
			{id: "sub-2", clientID: foreignQueueClientID, text: "theirs"},
		},
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	// No session in the manager at all: the throwaway resume the cut opens is
	// the only connection this rollback gets.
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("the fixture installed a session; this case is the cold one")
	}

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if left := readQueueState(t, queueState); len(left) != 0 {
		t.Fatalf("provider queue after a no-session rollback = %v, want empty", left)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	// The purge went first on that connection — ahead of the RESUME, not just
	// ahead of the cut. Resuming is what LOADS the thread, and a loaded
	// thread's idle hook is what dispatches the queue, so a purge issued after
	// it is racing a turn it can no longer stop.
	frames := strings.Split(strings.TrimSpace(string(raw)), "\n")
	firstDelete, firstResume, firstCut := -1, -1, -1
	for i, frame := range frames {
		switch {
		case firstDelete < 0 && strings.Contains(frame, `"method":"thread/queue/delete"`):
			firstDelete = i
		case firstResume < 0 && strings.Contains(frame, `"method":"thread/resume"`):
			firstResume = i
		case firstCut < 0 && (strings.Contains(frame, `"method":"thread/fork"`) ||
			strings.Contains(frame, `"method":"thread/revert"`)):
			firstCut = i
		}
	}
	if firstDelete < 0 {
		t.Fatalf("no queue delete reached the cut connection:\n%s", raw)
	}
	if firstResume >= 0 && firstDelete > firstResume {
		t.Errorf("the purge ran after thread/resume (delete at %d, resume at %d); the thread was already loaded and could dispatch a queued row",
			firstDelete, firstResume)
	}
	if firstCut >= 0 && firstDelete > firstCut {
		t.Errorf("the purge ran after the cut (delete at %d, cut at %d); a loaded thread can dispatch a queued row first",
			firstDelete, firstCut)
	}
}

// newProviderQueueResumeSession starts a session that RESUMES thread.SessionRef
// with the real `Config.BeforeResume` wiring the app installs, so the test
// exercises the hook body and the handshake gate rather than calling the
// reconcile helper by hand.
func newProviderQueueResumeSession(
	t *testing.T, app *App, thread store.Thread, capturePath string, opts codexQueueBinaryOpts,
) *codex.Session {
	t.Helper()
	binary, _ := writeCodexProviderQueueBinary(t, thread.SessionRef, capturePath, opts)
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:         binary,
			WorkDir:        thread.WorkspacePath,
			ResumeThreadID: thread.SessionRef,
			BeforeResume: func(resumed *codex.Session) {
				app.reconcileCodexProviderQueueOnResume(thread.ID, resumed)
			},
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// codexQueueUnsupportedNoticeRows returns the thread's downgrade-notice rows.
func codexQueueUnsupportedNoticeRows(t *testing.T, app *App, threadID string) []store.Item {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var out []store.Item
	for _, item := range items {
		if item.Kind == "notification" && strings.Contains(item.Meta, codexQueueUnsupportedNotificationKind) {
			out = append(out, item)
		}
	}
	return out
}

// TestCodexDowngradeNoticesTheQueuedRowsItCannotSee is the version-downgrade
// gap in the provider-owned queue. A 0.148+ session hands a message to
// `thread/queue/add`; the message is then durable in codex's SQLite and its AO
// row is persisted and marked. If the NEXT session for that thread runs on an
// OLDER Codex there is no `thread/queue/*` at all — nothing can list the row,
// purge it, or run it — so AO silently swapped to its in-process queue and the
// message waited invisibly until some later upgrade dispatched it out of turn.
//
// The two halves this pins are equally load-bearing. The rows must be left
// exactly alone (still persisted, still marked, not restored to the draft, not
// re-armed, not re-registered on AO's queue) because they are the provider's
// and every one of those moves either loses the message or runs it twice on
// upgrade — and the user must be TOLD, because otherwise nothing on any
// surface says the thread has work parked in a queue this binary cannot see.
func TestCodexDowngradeNoticesTheQueuedRowsItCannotSee(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-provider-queue-downgrade")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	thread.SessionRef = "provider-queue-downgrade"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	queuedMeta, err := itemmeta.MarkProviderQueued("")
	if err != nil {
		t.Fatalf("MarkProviderQueued: %v", err)
	}
	// A row whose echo already landed carries the same permanent marker and is
	// NOT outstanding — counting it would report messages that already ran.
	echoedMeta, err := itemmeta.MarkProviderQueued(`{"provider_item_id":"item-42"}`)
	if err != nil {
		t.Fatalf("MarkProviderQueued: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7, "first queued", queuedMeta)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:8:flush:1", 8, "second queued", queuedMeta)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:6:flush:1", 6, "already ran", echoedMeta)

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	oldSess := newProviderQueueResumeSession(t, app, thread, capture, codexQueueBinaryOpts{
		preQueue: true,
		// Seeded but unreachable: this app-server refuses every queue method,
		// which is the point — the rows exist and nothing here can see them.
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:7:flush:1", text: "first queued"},
			{id: "sub-2", clientID: "user:8:flush:1", text: "second queued"},
		},
	})
	if oldSess.ThreadQueueNative() {
		t.Fatal("ThreadQueueNative() = true on a 0.147 handshake; the gate must fail closed")
	}

	// Nothing was asked of the provider queue. A call that ignored the gate
	// would be a -32601 in the capture, not a silent no-op.
	raw, err := os.ReadFile(capture)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read captured requests: %v", err)
	}
	if strings.Contains(string(raw), `"method":"thread/queue/`) {
		t.Fatalf("a pre-0.148 session called a thread/queue method:\n%s", raw)
	}

	// (a) The rows stay exactly where the 0.148+ session left them.
	for _, id := range []string{"user:7:flush:1", "user:8:flush:1"} {
		row, found, err := app.store.GetThreadItem(thread.ID, id)
		if err != nil {
			t.Fatalf("GetThreadItem %s: %v", id, err)
		}
		if !found {
			t.Fatalf("row %s was dropped; the only record of a message the provider still holds is gone", id)
		}
		if !itemmeta.IsProviderQueued(row.Meta) {
			t.Errorf("row %s meta = %q, want the provider-queued marker retained", id, row.Meta)
		}
	}
	// Not re-armed and not requeued: nothing on this session can produce the
	// echo a pending send waits for, and a requeue would send the message a
	// second time the moment the thread went idle.
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Error("a pending send was armed on a session that can never see the queued dispatch")
	}
	if drained := app.triage.DrainUnconfirmedFlushItems(thread.ID); len(drained) != 0 {
		t.Errorf("provider-owned rows entered AO's in-process queue: %+v", drained)
	}
	if queued := app.triage.QueuedFlushItems(thread.ID); len(queued) != 0 {
		t.Errorf("provider-owned rows were registered as queue items: %+v", queued)
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if strings.TrimSpace(draft.Content) != "" {
		t.Errorf("draft content = %q; a message the provider queue owns must not come back as a draft", draft.Content)
	}
	if restored := emittedQueueRestored(rec); len(restored) != 0 {
		t.Errorf("queue restored events = %+v, want none", restored)
	}

	// (b) One notice, naming the count of OUTSTANDING rows only.
	notices := codexQueueUnsupportedNoticeRows(t, app, thread.ID)
	if len(notices) != 1 {
		t.Fatalf("downgrade notice rows = %d, want exactly 1: %+v", len(notices), notices)
	}
	notice := notices[0]
	if notice.ToolName != codexQueueUnsupportedNotificationKind {
		t.Errorf("notice kind = %q, want %q", notice.ToolName, codexQueueUnsupportedNotificationKind)
	}
	if !strings.HasPrefix(notice.Summary, "2 queued messages were handed to Codex 0.148+") {
		t.Errorf("notice summary = %q, want it to lead with the outstanding count (2, not 3)", notice.Summary)
	}
	if !strings.Contains(notice.Summary, "They run when Codex is upgraded.") {
		t.Errorf("notice summary = %q, want it to say the messages are not lost", notice.Summary)
	}

	if err := oldSess.Close(); err != nil {
		t.Fatalf("close the downgraded session: %v", err)
	}

	// The upgrade path: a queue-native session re-arms the same rows normally
	// and adds no second notice — the row it would duplicate is the one that
	// already told the user, and the condition it described is over.
	nativeCapture := filepath.Join(t.TempDir(), "requests-native.jsonl")
	nativeSess := newProviderQueueResumeSession(t, app, thread, nativeCapture, codexQueueBinaryOpts{
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:7:flush:1", text: "first queued"},
			{id: "sub-2", clientID: "user:8:flush:1", text: "second queued"},
		},
	})
	if !nativeSess.ThreadQueueNative() {
		t.Fatal("ThreadQueueNative() = false on a 0.149 handshake")
	}
	head, ok := app.triage.PeekPendingSendHeadForTest(thread.ID)
	if !ok {
		t.Fatal("the queue-native session did not re-arm; the dispatched turn's echo has no row to claim")
	}
	if head.AOItemID != "user:7:flush:1" {
		t.Errorf("re-armed head = %q, want the oldest outstanding queued row", head.AOItemID)
	}
	if claims := codex.SelfQueuedClaimIDsForTest(nativeSess); len(claims) != 2 {
		t.Errorf("re-armed self-queued claims = %v, want both AO rows", claims)
	}
	if after := codexQueueUnsupportedNoticeRows(t, app, thread.ID); len(after) != 1 {
		t.Fatalf("downgrade notice rows after the upgrade = %d, want the original 1: %+v", len(after), after)
	}
}

// providerQueueReconcileThread is the fixture the reconcile tests share: a
// Codex thread with a triage router wired, since every assertion below is
// about what the router and the store agree on after a session start.
func providerQueueReconcileThread(t *testing.T, app *App, name string) store.Thread {
	t.Helper()
	thread := testThread(name)
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.ensureTriageRouter()
	return thread
}

// markedQueueRowMeta returns the meta a provider-queue dispatch stamps on its
// row: proven when the add was acked, unproven when it was not.
func markedQueueRowMeta(t *testing.T, proven bool) string {
	t.Helper()
	var (
		meta string
		err  error
	)
	if proven {
		meta, err = itemmeta.MarkProviderQueued("")
	} else {
		meta, err = itemmeta.MarkProviderQueueHandoff("")
	}
	if err != nil {
		t.Fatalf("mark provider-queued meta: %v", err)
	}
	return meta
}

// TestProviderQueueReconcileReturnsAMessageTheProviderNeverTook pins the
// sequence finding H1 is about: persist + mark, then die before
// `thread/queue/add` lands.
//
// The row is marked, so every recovery path steps around it as the provider's
// — and the provider does not have it. Nothing else in the system will ever
// run that message, so before this reconcile it sat in the timeline forever
// as a prompt with no answer. The proven row in the same queue-absent listing
// is the control: it is absent because it RAN, and returning it to the
// composer would offer the user a re-send of something already answered.
func TestProviderQueueReconcileReturnsAMessageTheProviderNeverTook(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-never-taken")

	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7,
		"never taken", markedQueueRowMeta(t, false))
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:2", 7,
		"already ran", markedQueueRowMeta(t, true))

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	// The provider's queue is empty: it holds neither row.
	sess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{})

	app.rearmCodexProviderQueueClaims(thread.ID, sess)

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if found {
		t.Error("the never-taken row is still in the timeline; nothing will ever run it")
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:2"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if !found {
		t.Error("a message the provider already ran was pulled out of the timeline")
	}

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if !strings.Contains(draft.Content, "never taken") {
		t.Errorf("draft = %q, want the never-taken message back in the composer", draft.Content)
	}
	if strings.Contains(draft.Content, "already ran") {
		t.Errorf("draft = %q, want no copy of a message that already ran", draft.Content)
	}

	// And neither row is armed for an echo: one has no message left to
	// deliver, the other's already arrived.
	if claims := codex.SelfQueuedClaimIDsForTest(sess); len(claims) != 0 {
		t.Errorf("re-armed claims = %v, want none; the provider holds neither row", claims)
	}
	if pending := app.triage.DrainUnconfirmedFlushItems(thread.ID); len(pending) != 0 {
		t.Errorf("pending sends = %+v, want none", pending)
	}
}

// TestUnconfirmedQueueAddStaysReconcilableUntilTheNextSessionStart is finding
// H4 joined to H1, which is the only way either is worth anything.
//
// A `thread/queue/add` whose ack is lost and whose read-back finds nothing is
// genuinely undecidable AT THAT MOMENT: the write may have landed. So the
// dispatch settles without claiming ownership it cannot substantiate, and the
// row keeps its unproven hand-off — which is exactly what the next session
// start needs to ask the queue about it and hand the message back.
func TestUnconfirmedQueueAddStaysReconcilableUntilTheNextSessionStart(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-unconfirmed-add")

	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7,
		"lost ack", markedQueueRowMeta(t, false))

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		// The ack never comes AND the read-back finds nothing, which is the
		// case the acked-and-listed test next door does not cover.
		silentAdd: true,
	})
	codex.SetRequestTimeoutForTest(codexSess, 300*time.Millisecond)
	sess := session{provider: string(provider.Codex), codex: codexSess}

	handoff, err := app.dispatchFlushToProvider(
		sess, thread.ID, "lost ack", provider.SendOptions{}, "user:7:flush:1")
	if err != nil {
		t.Fatalf("an ambiguous add surfaced as a dispatch failure: %v", err)
	}
	if handoff != codexQueueHandoffUnconfirmed {
		t.Fatalf("handoff = %v, want unconfirmed; nothing proved the provider took the message", handoff)
	}

	// The caller only promotes a CONFIRMED hand-off, so the row is still
	// unproven — and that is what makes the reconcile below able to act.
	item, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1")
	if err != nil || !found {
		t.Fatalf("GetThreadItem: found=%v err=%v", found, err)
	}
	if !itemmeta.IsProviderQueueHandoffPending(item.Meta) {
		t.Fatal("the row was settled as provider-owned on an unproven add; the next session start can no longer tell it from a dispatched one")
	}

	app.rearmCodexProviderQueueClaims(thread.ID, codexSess)

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if found {
		t.Error("the unconfirmed row survived a reconcile against an empty queue; nothing will ever run it")
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if !strings.Contains(draft.Content, "lost ack") {
		t.Errorf("draft = %q, want the unrun message back in the composer", draft.Content)
	}
}

// TestProviderQueueReconcileReArmsFromTheStoreWhenTheQueueCannotBeRead is
// finding H2.
//
// `thread/resume` follows this hook immediately and a loaded thread's idle
// hook dispatches its queue, so a reconcile that gave up on a failed list
// would watch AO's own queued message start a turn it cannot account for: the
// echo is stamped external-queue, triage refuses to pop the pending send for a
// foreign echo, and the user's prompt lands a SECOND time as an injected row.
// The store answers the ownership question without the wire, so that is where
// the answer comes from, and the half it cannot answer becomes a notice.
func TestProviderQueueReconcileReArmsFromTheStoreWhenTheQueueCannotBeRead(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-unreadable")

	// PROVEN: the app-server acked this row's add, so the store alone is a
	// complete answer about who owns it. The unproven case is the other
	// direction and has its own test — see
	// TestProviderQueueReconcileLeavesAnUnprovenRowAloneWhenTheQueueCannotBeRead.
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7,
		"mine", markedQueueRowMeta(t, true))

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	sess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{listError: true})

	app.rearmCodexProviderQueueClaims(thread.ID, sess)

	claims := codex.SelfQueuedClaimIDsForTest(sess)
	if len(claims) != 1 || claims[0] != "user:7:flush:1" {
		t.Fatalf("re-armed claims = %v, want AO's own row; an unreadable queue must not make AO's message foreign", claims)
	}
	head, ok := app.triage.PeekPendingSendHeadForTest(thread.ID)
	if !ok || head.AOItemID != "user:7:flush:1" {
		t.Fatalf("pending send head = %+v (ok=%v), want the marked row; its dispatch echo has nothing to claim otherwise", head, ok)
	}

	// NOT restored: the queue may well hold it, and handing back a message
	// the provider is about to run is the one outcome worse than waiting.
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1"); err != nil || !found {
		t.Fatalf("the row was returned to the composer on an unreadable queue: found=%v err=%v", found, err)
	}

	// One retry before giving up: the shape this covers is a first request
	// racing an app-server that has just finished starting.
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	if lists := strings.Count(string(raw), `"method":"thread/queue/list"`); lists != 2 {
		t.Errorf("thread/queue/list frames = %d, want 2 (one retry)", lists)
	}

	// And the user is told, because the reconcile is genuinely partial: a row
	// that was never taken stays stranded until a later start can read the
	// queue. A log line would not say that to anyone (CLAUDE.md principle 5).
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	noticed := false
	for _, item := range items {
		if strings.Contains(item.Meta, codexQueueUnreconciledNotificationKind) {
			noticed = true
		}
	}
	if !noticed {
		t.Fatalf("no unreconciled-queue notice on the timeline: %+v", items)
	}
}

// TestProviderQueueReconcileIgnoresAForeignSubmissionWearingAnAOItemID is
// finding H7. AO's queued ids (`user:<turn>:flush:<n>`) are deterministic, so
// they are not a credential: a second Agent Overflow profile against the same
// Codex home mints the same ones, and anything speaking `thread/queue/add` can
// simply supply one. Recognising the GRAMMAR would re-arm that submission as
// AO's and render its author's message as this user's own.
//
// The persisted row is the token instead. It cannot be forged from the wire,
// because only this app's own dispatch writes it.
func TestProviderQueueReconcileIgnoresAForeignSubmissionWearingAnAOItemID(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-forged-id")

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	sess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		// AO's id grammar exactly, and no row in this app's store behind it.
		rows: []queuedRow{{id: "sub-1", clientID: "user:7:flush:1", text: "not mine"}},
	})

	app.rearmCodexProviderQueueClaims(thread.ID, sess)

	if claims := codex.SelfQueuedClaimIDsForTest(sess); len(claims) != 0 {
		t.Fatalf("re-armed claims = %v, want none; no row in this store ever queued that id", claims)
	}
	if pending := app.triage.DrainUnconfirmedFlushItems(thread.ID); len(pending) != 0 {
		t.Fatalf("pending sends = %+v, want none; the echo would be claimed as the user's own message", pending)
	}
}

// TestRollbackRefusesWhenTheProviderQueueCannotBePurged is finding H3.
//
// A queued row survives `stopSession` in codex's own SQLite and its idle hook
// dispatches it on the next resume. Truncating history over a queue that could
// not be emptied therefore arms the removed messages to re-run against the
// shortened thread, silently and possibly days later. Refusing is visible and
// the user can retry; the replay is neither.
func TestRollbackRefusesWhenTheProviderQueueCannotBePurged(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-purge-refusal")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{listError: true})
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex), codex: codexSess, token: "live",
	})

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("the rollback truncated history over a queue it could not empty")
	}
	if !strings.Contains(err.Error(), "queued") {
		t.Errorf("error = %v, want it to name the queued messages as the reason", err)
	}

	// Nothing was mutated: the refusal lands before the session stop, so the
	// user's retry starts from where they were.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after the refused rollback = %d, want both still there: %+v", len(items), items)
	}
	if _, ok := app.sessionManager().get(thread.ID); !ok {
		t.Error("the session was stopped by a rollback that refused; the refusal must land first")
	}
}

// TestRollbackRefusalRestoresTheMessagesThePartialPurgeAlreadyDeleted is K1 at
// the app boundary.
//
// The purge deletes row by row. With A and B queued, deleting A can succeed
// and B fail — and the caller then aborts the rollback and leaves history
// untouched, which is what the refusal promises. But A is already out of
// codex's queue: no idle hook will dispatch it, no echo will claim its row,
// and every recovery path steps around a row marked provider-queued. Without
// this, abandoning a rollback silently eats a message the user queued.
func TestRollbackRefusalRestoresTheMessagesThePartialPurgeAlreadyDeleted(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-partial-purge")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	// The message AO handed to the provider's queue, proven and unclaimed.
	insertUserItemWithMeta(t, app.store, thread.ID, "user:2:flush:1", 2,
		"queued and then purged", markedQueueRowMeta(t, true))
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	var restored []QueueRestoredEvent
	app.testEmitHook = func(name string, data any) {
		if name != "provider:queue_restored" {
			return
		}
		if evt, ok := data.(QueueRestoredEvent); ok {
			restored = append(restored, evt)
		}
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			{id: "sub-mine", clientID: "user:2:flush:1", text: "queued and then purged"},
			{id: "sub-stuck", clientID: foreignQueueClientID, text: "somebody else's"},
		},
		deleteRefused: "sub-stuck",
	})
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex), codex: codexSess, token: "live",
	})

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("the rollback truncated history over a queue it could not empty")
	}
	if !strings.Contains(err.Error(), "composer") {
		t.Errorf("error = %v, want it to say where the already-deleted message went", err)
	}

	// History is untouched, and the session is still up: the refusal lands
	// before either mutation.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.ID == "user:2:flush:1" {
			t.Fatal("the purged row is still in the timeline; nothing will ever run it")
		}
	}
	if len(items) != 2 {
		t.Fatalf("items after the refused rollback = %d, want the two history rows: %+v", len(items), items)
	}
	if _, ok := app.sessionManager().get(thread.ID); !ok {
		t.Error("the session was stopped by a rollback that refused")
	}

	// The message the purge deleted is back where the user can send it.
	draft, found, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !found {
		t.Fatalf("thread draft after the refused rollback: found=%v err=%v", found, err)
	}
	if !strings.Contains(draft.Content, "queued and then purged") {
		t.Fatalf("draft = %q, want the purged message returned to the composer", draft.Content)
	}
	if len(restored) != 1 || restored[0].Reason != queueRestoredReasonPurgeAborted {
		t.Fatalf("queue_restored events = %+v, want one with the purge-aborted reason", restored)
	}
	if len(restored[0].UserItemIDs) != 1 || restored[0].UserItemIDs[0] != "user:2:flush:1" {
		t.Fatalf("restored ids = %v, want exactly AO's own purged row", restored[0].UserItemIDs)
	}
}

// TestPurgeRefusalDoesNotRestoreAForeignSubmissionItDeleted is the other half
// of the same rule. A foreign producer's row is deleted by the purge too — it
// carries the same replay hazard — but AO cannot put it back: there is no
// `thread/queue/add` that preserves another producer's authorship, and
// re-adding it would render that author's text as this user's own message.
// So it is reported and counted, never restored.
func TestPurgeRefusalDoesNotRestoreAForeignSubmissionItDeleted(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-foreign-purge")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	var restored []QueueRestoredEvent
	app.testEmitHook = func(name string, data any) {
		if name != "provider:queue_restored" {
			return
		}
		if evt, ok := data.(QueueRestoredEvent); ok {
			restored = append(restored, evt)
		}
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			// Deleted first, and not this app's: no store row backs it.
			{id: "sub-foreign", clientID: foreignQueueClientID, text: "somebody else's"},
			{id: "sub-stuck", clientID: "0199e3a1-0000-7000-8000-000000000002", text: "also theirs"},
		},
		deleteRefused: "sub-stuck",
	})
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex), codex: codexSess, token: "live",
	})

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("the rollback truncated history over a queue it could not empty")
	}
	if !strings.Contains(err.Error(), "not added by Agent Overflow") &&
		!strings.Contains(err.Error(), "outside Agent Overflow") {
		t.Errorf("error = %v, want it to say the dropped message was not this app's and is unrecoverable", err)
	}
	if len(restored) != 0 {
		t.Fatalf("queue_restored events = %+v, want none; a foreign submission has no AO row to restore", restored)
	}
	draft, found, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if found && strings.Contains(draft.Content, "somebody else's") {
		t.Fatalf("draft = %q; a foreign producer's message was put in this user's composer", draft.Content)
	}
}

// TestProviderQueueReconcileLeavesAnUnprovenRowAloneWhenTheQueueCannotBeRead
// is K2.
//
// The store-only fallback used to arm a claim AND a pending send for every
// marked row, unproven ones included. An unproven row's `thread/queue/add` was
// written and never acked, so overwhelmingly the provider never took it — and
// then no echo can ever consume either piece of state. The message is stranded
// outside both the provider and the composer, and the pending send sits in the
// FIFO where HasPendingSendForThread reads it and refuses every
// revert-and-resend on the thread.
func TestProviderQueueReconcileLeavesAnUnprovenRowAloneWhenTheQueueCannotBeRead(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-unproven-unreadable")

	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7,
		"acked", markedQueueRowMeta(t, true))
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:2", 7,
		"never acked", markedQueueRowMeta(t, false))

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	sess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{listError: true})

	app.rearmCodexProviderQueueClaims(thread.ID, sess)

	// Only the PROVEN row is re-armed. For it the store is a complete answer:
	// the provider acked the add, so either the row is still queued (the claim
	// is what its dispatch needs) or it already ran (the claim is inert).
	claims := codex.SelfQueuedClaimIDsForTest(sess)
	if len(claims) != 1 || claims[0] != "user:7:flush:1" {
		t.Fatalf("re-armed claims = %v, want only the proven row", claims)
	}
	pending := app.triage.DrainUnconfirmedFlushItems(thread.ID)
	if len(pending) != 1 || pending[0].QuietItem == nil || pending[0].QuietItem.ID != "user:7:flush:1" {
		t.Fatalf("pending sends = %+v, want only the proven row; an unproven one blocks revert-and-resend forever", pending)
	}

	// Neither row is restored: the queue may hold either, and handing back a
	// message the provider is about to run is the one outcome worse than
	// waiting.
	for _, id := range []string{"user:7:flush:1", "user:7:flush:2"} {
		if _, found, err := app.store.GetThreadItem(thread.ID, id); err != nil || !found {
			t.Fatalf("row %s was returned to the composer on an unreadable queue: found=%v err=%v", id, found, err)
		}
	}

	// And the notice names the unproven row, so the user learns the state
	// rather than losing the message silently.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	notice := ""
	for _, item := range items {
		if strings.Contains(item.Meta, codexQueueUnreconciledNotificationKind) {
			notice = item.Summary
		}
	}
	if notice == "" {
		t.Fatalf("no unreconciled-queue notice on the timeline: %+v", items)
	}
	if !strings.Contains(notice, "may never have reached Codex") {
		t.Fatalf("notice = %q, want it to name the row that may never have reached Codex", notice)
	}
	if !strings.Contains(notice, "may still be waiting") {
		t.Fatalf("notice = %q, want it to name the proven row too", notice)
	}
}
