// Integration tests covering the composer + message timeline working
// together. These tests mount the full App and drive user input through
// the composer, then observe the side effects the unified item stream
// drives in the message timeline and composer.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Item, Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import { emitWailsEvent } from '../mocks/wailsio-runtime';
import { emitItemEventDelta, emitItemEventUpsert } from '../helpers/chat';
import { getQueueForThread } from '../../lib/stores/sendQueue.svelte';
import { getActiveTurn } from '../../lib/stores/threadStatuses.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

// Mount App with a single existing thread already selected. Returns the
// rendered result for assertions.
//
// NOTE: callers should install SendMessageWithOptions / InterruptTurn mocks themselves
// before calling this function. The helper intentionally does not overwrite
// them so per-test mocks survive.
async function mountWithActiveThread(thread: Thread = makeThread({ title: 'Messaging Spec Thread' })) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  installComposerDefaults(thread.id);

  const rendered = render(App);
  await flush();
  // Click the thread row to activate it.
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return { ...rendered, thread };
}

describe('App integration — messaging flow', () => {
  beforeEach(() => {
    resetAppState();
    // Default SendMessageWithOptions + InterruptTurn mocks — tests that need to spy
    // or reject reassign these with fresh `setBindingMock` calls.
    setBindingMock('SendMessageWithOptions', async () => makeThread({ id: 'thread-1' }));
    setBindingMock('InterruptTurn', async () => {});
  });

  it('sends a message and clears the composer draft', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    // Re-assign the mock AFTER mount so the call count starts at 0.
    const sendMock = setBindingMock('SendMessageWithOptions', async () => makeThread({ id: 'thread-1' }));

    // Composer textarea is keyed with aria-label "Message input".
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(false);
    await fireEvent.input(textarea, { target: { value: 'hello agent' } });
    await flush();

    const sendBtn = getByTestId('composer-send') as HTMLButtonElement;
    await waitFor(() => expect(sendBtn.disabled).toBe(false));
    await fireEvent.click(sendBtn);
    await waitFor(() => expect(sendMock).toHaveBeenCalled());

    expect(sendMock.mock.calls[0][0]).toBe('thread-1');
    expect(sendMock.mock.calls[0][1]).toBe('hello agent');
    expect(sendMock.mock.calls[0][2]).toEqual({
      attachmentIds: [],
    });
    expect(textarea.value).toBe('');
  });

  it('Enter during an active turn enqueues the message instead of dispatching', async () => {
    // Replaces the older "Cannot send during active turn" block — the
    // composer is always-typeable now and Enter routes mid-round
    // submissions through the per-thread send queue. Drain fires on
    // the next provider:turn_completed regardless of cause (success,
    // error, abort) — that's the uniform rule both reference UIs use
    // (Claude Code's commandQueue + useQueueProcessor; Codex's
    // VecDeque<QueuedUserMessage> + maybe_send_next_queued_input).
    const { getByLabelText } = await mountWithActiveThread();
    const sendMock = setBindingMock('SendMessageWithOptions', async () => makeThread({ id: 'thread-1' }));
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });
    emitItemEventUpsert({
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'response...',
      createdAt: 1,
      updatedAt: 1,
    });
    await flush();

    await fireEvent.input(textarea, { target: { value: 'queue this' } });
    await flush();
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    await flush();

    // Mid-round Enter never reaches SendMessageWithOptions — the
    // message is in the per-thread queue waiting for the round to
    // complete. The textarea clears so the user can stack the next
    // message.
    expect(sendMock).not.toHaveBeenCalled();
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['queue this']);
    expect(textarea.value).toBe('');
  });

  // The legacy frontend-driven drain tests have been removed: the
  // backend now owns the queue (Phases G1–G6) and the trigger fires
  // on the first non-subagent tool_use of a wire round, not on
  // turn_completed. End-to-end queue coverage lives in
  // src/test/integration/send-queue-flow.test.ts (frontend events ↔
  // store ↔ UI) and internal/triage/flush_queue_test.go +
  // app_flush_queue_test.go (backend trigger + dispatcher).

  it('queued items survive a thread switch A → B → A', async () => {
    // Mount thread-1 first.
    const { getByLabelText } = await mountWithActiveThread();
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    // Queue an item on thread-1 mid-round.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'r-1',
      turnIndex: 0,
      startedAt: 1,
    });
    await flush();
    await fireEvent.input(textarea, { target: { value: 'queued on A' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    await flush();
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['queued on A']);

    // Switch panes by setting pane.threadId — reuse the pane store
    // directly. The queue is keyed in a global SvelteMap, so the
    // entries should still be there when we come back. Note: pane
    // switch via UI would require a sidebar click flow that's out
    // of scope for this micro-test; the key assertion is "the queue
    // is keyed globally and survives".
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    setBindingMock('SwitchThread', async () => makeThread({ id: 'thread-2', title: 'Other' }));
    await pane.switchThread(makeThread({ id: 'thread-2', title: 'Other' }));
    await flush();
    // thread-1's queue is still intact even though we're now on thread-2.
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['queued on A']);
    expect(getQueueForThread('thread-2')).toEqual([]);

    // Switch back.
    setBindingMock('SwitchThread', async () => makeThread({ id: 'thread-1' }));
    await pane.switchThread(makeThread({ id: 'thread-1' }));
    await flush();
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['queued on A']);
  });

  it('interrupts an active turn via the Interrupt button', async () => {
    const { getByTestId } = await mountWithActiveThread();
    const interruptMock = setBindingMock('InterruptTurn', async () => {});

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });
    emitItemEventUpsert({
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'streaming...',
      createdAt: 1,
      updatedAt: 1,
    });
    await flush();
    await fireEvent.click(getByTestId('composer-interrupt'));
    await waitFor(() => expect(interruptMock).toHaveBeenCalled());
    expect(interruptMock.mock.calls[0][0]).toBe('thread-1');
  });

  it('renders streaming assistant item updates as they arrive', async () => {
    await mountWithActiveThread();
    emitItemEventUpsert({
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'first ',
      createdAt: 1,
      updatedAt: 1,
    });
    await flush();
    emitItemEventDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: 'second',
      updatedAt: 2,
    });
    await flush();

    await waitFor(() => {
      expect(document.body.textContent).toContain('first second');
    });
  });

  it('renders tool_call rows inline as provider:item_event upserts arrive', async () => {
    const { queryByText, findByText } = await mountWithActiveThread();

    // Backend persisted a tool_call item and pushed the upsert; the
    // timeline should reflect it without any transient grouping.
    emitItemEventUpsert({
      id: 'tool-1',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      summary: 'Bash: ls -la',
      status: 'running',
      isBackground: false,
      createdAt: 1,
      updatedAt: 1,
    });
    expect(await findByText(/ls -la/)).toBeInTheDocument();

    // A second concurrent tool_call shows up as its own row — no
    // grouping chip, no relocation.
    emitItemEventUpsert({
      id: 'tool-2',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'tool_call',
      role: 'assistant',
      summary: 'Read: README.md',
      status: 'running',
      isBackground: false,
      createdAt: 2,
      updatedAt: 2,
    });
    expect(await findByText(/Read: README.md/)).toBeInTheDocument();
    expect(queryByText(/Running 2 tools/i)).toBeNull();
  });

  it('handles SendMessageWithOptions rejection by restoring draft and logging the error', async () => {
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    // Override SendMessageWithOptions to reject AFTER mount so earlier calls don't trip.
    setBindingMock('SendMessageWithOptions', async () => {
      throw new Error('rpc down');
    });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'will fail' } });
    await flush();
    await fireEvent.click(getByTestId('composer-send'));
    await waitFor(() => {
      const call = consoleErr.mock.calls.find((c) =>
        String(c[0] ?? '').includes('Failed to send message'),
      );
      expect(call).toBeDefined();
    });
    // The draft was restored (textarea content matches).
    await waitFor(() => {
      expect(textarea.value).toBe('will fail');
    });
    consoleErr.mockRestore();
  });

  it('marks the pane idle once the streaming item completes', async () => {
    await mountWithActiveThread();
    // Post-refactor getActiveTurn(pane.threadId) !== null only clears on provider:turn_completed
    // (invariant 22). Drive the full turn lifecycle so the assertion is
    // exercising the real wire path.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });
    emitItemEventUpsert({
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'persist me',
      createdAt: 1,
      updatedAt: 1,
    });
    emitItemEventUpsert({
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'completed',
      summary: 'persist me',
      createdAt: 1,
      updatedAt: 2,
    });
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
    });
    await flush(10);

    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    await waitFor(() => expect(getActiveTurn(pane.threadId) !== null).toBe(false));
  });

  // End-to-end turn lifecycle — every stage on the real wire path:
  // turn_started → tool lifecycle → turn_completed → divider renders
  // above the assistant message. This test is intentionally broad: it
  // is the single integration test that proves all of the invariant-22
  // pieces line up together (getActiveTurn(pane.threadId) flip,
  // pane.latestSettledTurn write, response-divider DOM render).
  //
  // We pick the ordering where turn_completed arrives BEFORE the final
  // assistant_text upsert. That is the common real-wire ordering
  // observed in captured ndjson_bash / ndjson_task fixtures — Claude's
  // `result` envelope fires once the stream ends but the final
  // `assistant` envelope's last content block has already been
  // emitted upstream in the stream. The alternate ordering
  // (assistant_text first) is the simpler of the two and is already
  // covered implicitly by the "marks the pane idle" test above. We
  // pick the harder one here so a regression that assumed items must
  // arrive before the turn_completed gets caught.
  it('end-to-end turn cycle: turn_started → tool lifecycle → turn_completed → divider renders', async () => {
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { queryByTestId, findByTestId, findByText } = await mountWithActiveThread();
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();

    // 1. Turn starts. getActiveTurn(pane.threadId) is populated; pane has no
    // settled turn yet. The sidebar pill is the user-facing surface
    // for "turn is running" and is covered by its own tests.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 't1',
      turnIndex: 0,
      startedAt: 1,
    });
    await waitFor(() => {
      expect(getActiveTurn(pane.threadId)).toEqual({ turnId: 't1', turnIndex: 0, startedAt: 1 });
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(true);
    expect(pane.latestSettledTurn).toBeNull();

    // 2. Tool call upserts: running → completed. The row appears in
    // the timeline as a ToolCallCard. This exercises the mid-turn
    // item lifecycle — status flip from running to completed.
    emitItemEventUpsert({
      id: 'tool-xyz',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      summary: 'Bash: echo hello',
      status: 'running',
      isBackground: false,
      createdAt: 10,
      updatedAt: 10,
    });
    await flush();
    expect(await findByText(/echo hello/)).toBeInTheDocument();

    emitItemEventUpsert({
      id: 'tool-xyz',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      summary: 'Bash: echo hello',
      status: 'completed',
      isBackground: false,
      createdAt: 10,
      updatedAt: 20,
    });
    await flush();

    // 3. Turn completes. The provider sent turn_completed BEFORE the
    // final assistant_text item landed — this is the real-world
    // race. getActiveTurn(pane.threadId) MUST flip to null immediately; the
    // working indicator disappears; latestSettledTurn is populated
    // with the assistant message id we're expecting to land next.
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 't1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 5000,
      stopReason: 'end_turn',
      assistantMessageId: 'assist1',
      tokenUsage: JSON.stringify({
        input_tokens: 42,
        output_tokens: 18,
        cache_read_input_tokens: 0,
        total_cost_usd: 0.00123,
      }),
    });
    await waitFor(() => {
      expect(getActiveTurn(pane.threadId)).toBeNull();
    });
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
    expect(pane.latestSettledTurn?.turnId).toBe('t1');
    expect(pane.latestSettledTurn?.assistantMessageId).toBe('assist1');
    expect(pane.latestSettledTurn?.tokenUsage).toEqual({
      inputTokens: 42,
      outputTokens: 18,
      cacheReadInputTokens: 0,
      totalCostUsd: 0.00123,
    });
    // At this point the response divider has nothing to render above
    // because the response text hasn't landed in pane.items yet.
    expect(queryByTestId('response-divider')).toBeNull();

    // 4. The final assistant_text arrives. The structural Response
    // divider mounts immediately before it because tool activity
    // preceded assistant prose in the same turn.
    emitItemEventUpsert({
      id: 'assist1',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'completed',
      summary: 'Here is the result: hello\n',
      createdAt: 6000,
      updatedAt: 6000,
    });
    await flush();
    const divider = await findByTestId('response-divider');
    expect(divider.textContent).toContain('Response');

    // No console errors throughout — if a later emit handler threw
    // or a listener swallowed an error, the divider would still
    // mount but the side-effect would leak here.
    expect(consoleErr).not.toHaveBeenCalled();
    consoleErr.mockRestore();
  });

  // Backgrounded-launch integration: the launch row stays status=running
  // after turn_completed (invariant 24), the BackgroundTaskTray renders
  // it as a pending row, the "…" badge renders on the inline card, and
  // when the sibling tool_completion finally lands the tray row updates
  // rather than duplicating. This pins the cross-cutting behavior that
  // was the chief migration risk out of forge's buffered mode.
  //
  // BackgroundTaskTray's retention clock is 2 s from completion.createdAt
  // (COMPLETION_RETENTION_MS). We stamp createdAt using Date.now()
  // rather than a small static number so the pruning window starts at
  // test-wall-clock, otherwise the completion would be "aged out"
  // before it ever renders.
  it('backgrounded tool outlives turn; badge renders; sibling arrives after', async () => {
    const { queryByTestId, findByTestId } = await mountWithActiveThread();
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();

    // BackgroundTaskTray sources its rows from ListLiveBackgroundTasks
    // (thread-scoped, independent of the paged timeline). Install a
    // stateful mock AFTER mount so it isn't overwritten by the
    // installThreadViewDefaults → empty default. Each
    // provider:item_event upsert that also mutates `liveBackgroundItems`
    // lands in the tray after its 100 ms debounced refresh fires.
    const liveBackgroundItems: Item[] = [];
    setBindingMock('ListLiveBackgroundTasks', async () => [...liveBackgroundItems]);

    const startedAt = Date.now();

    // 1. Turn starts.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'tbg',
      turnIndex: 0,
      startedAt,
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId) !== null).toBe(true));

    // 2. Backgrounded tool_call launch (run_in_background:true on the
    // Claude wire). The launch row stays status=running per invariant
    // 24 — triage never flips it even at turn-complete.
    const launchItem: Item = {
      id: 'bg-launch',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      summary: 'Bash: sleep 10 && echo done',
      status: 'running',
      isBackground: true,
      toolName: 'Bash',
      createdAt: startedAt + 10,
      updatedAt: startedAt + 10,
    };
    liveBackgroundItems.push(launchItem);
    emitItemEventUpsert(launchItem);
    await flush();

    // The inline command row renders "…" in the status chip while
    // status=running AND isBackground=true. This is the spec's sole
    // render signal for "work dispatched, waiting for sibling" —
    // invariant 24 + §UI components driven by this state. The chip
    // replaces "running"; no separate badge element is rendered.
    const status = await findByTestId('command-output-status');
    expect(status.textContent?.trim()).toBe('…');
    expect(status.getAttribute('aria-label')).toBe('Backgrounded');

    // 3. Turn ends while the backgrounded work is still in flight.
    // getActiveTurn(pane.threadId) clears, the working indicator hides, and the
    // launch row is UNTOUCHED (no status flip, no sibling row yet).
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'tbg',
      turnIndex: 0,
      startedAt,
      completedAt: startedAt + 500,
      stopReason: 'end_turn',
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId)).toBeNull());
    await waitFor(() => expect(queryByTestId('chat-working-indicator')).toBeNull());

    // The "…" label is still present — invariant 24. The launch row
    // renders as background+running until the sibling terminal lands.
    expect(status.textContent?.trim()).toBe('…');

    // The background tray now renders the launch — the tray consumes
    // pane.items and only filters by isBackground/kind/completionOf,
    // so it picks up the launch regardless of the turn state.
    const tray = await findByTestId('background-task-tray');
    expect(tray).toBeInTheDocument();
    expect((await findByTestId('background-task-tray-count')).textContent).toBe('1');

    // 4. The sibling tool_completion arrives later (task_updated via
    // EventBackgroundTaskTerminal → triage idempotent sibling upsert).
    // The tray must pair it with the launch and update the row in
    // place rather than growing to 2 rows. The completion's createdAt
    // is near the current wall clock so the tray's 2 s retention
    // window starts NOW, not in the past.
    const completionItem: Item = {
      id: 'complete:bg-launch',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'tool_completion',
      role: 'assistant',
      summary: 'Bash: sleep 10 && echo done -> done',
      status: 'completed',
      isBackground: true,
      completionOf: 'bg-launch',
      toolName: 'Bash',
      createdAt: Date.now(),
      updatedAt: Date.now(),
    };
    liveBackgroundItems.push(completionItem);
    emitItemEventUpsert(completionItem);
    await flush();

    // BackgroundTaskTray pairs launch + completion by completionOf;
    // the tray count stays at 1 (one logical task), and the row
    // status flips to completed. Use waitFor so the 100 ms debounced
    // tray refresh has time to pick up the completion. The tray
    // defaults to collapsed in production, so expand it before
    // inspecting the row.
    expect((await findByTestId('background-task-tray-count')).textContent).toBe('1');
    await fireEvent.click(await findByTestId('background-task-tray-header'));
    await waitFor(async () => {
      const rowStatus = await findByTestId('background-task-tray-row-status');
      expect(rowStatus.getAttribute('data-status')).toBe('completed');
    });

    // Cross-cutting end-to-end check: the sibling tool_completion row
    // in the chat timeline must render the unified success badge
    // (kind=tool_completion + status=completed → success per
    // deriveCompletionStatus). The launch row above it still shows
    // `…` per the transcript-stability invariant — pin both.
    await waitFor(async () => {
      const badges = document.querySelectorAll('[data-testid="completion-badge"]');
      const successBadge = Array.from(badges).find(
        (b) => b.getAttribute('data-status') === 'success',
      );
      expect(successBadge).toBeDefined();
    });
    // The launch row keeps `…` even after the sibling completes.
    const launchStatus = document.querySelector(
      '[data-testid="command-output-status"]',
    );
    expect(launchStatus?.textContent?.trim()).toBe('…');
  });

  // Multi-result-per-turn cascade: the backend emits one
  // provider:turn_started/turn_completed pair per wire `result`
  // envelope (see internal/triage/AGENTS.md "Wire-round vs
  // logical-turn"). The frontend's working indicator + composer block
  // must flip OFF between rounds — the model is genuinely idle while
  // a backgrounded task hasn't yet produced its notification — and
  // re-flip ON for round 2 when the task notification provokes
  // another model call.
  it('flips off between rounds in a multi-result-per-turn cascade and re-flips on round 2', async () => {
    const { queryByTestId, findByTestId } = await mountWithActiveThread();
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();

    // Round 1 begins.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'round-1',
      turnIndex: 0,
      startedAt: Date.now(),
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId)?.turnId).toBe('round-1'));
    expect(await findByTestId('chat-working-indicator')).toBeInTheDocument();

    // Round 1 ends — model handed off to backgrounded work and is
    // idle. Frontend MUST observe no active turn during the gap.
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'round-1',
      turnIndex: 0,
      startedAt: Date.now(),
      completedAt: Date.now() + 500,
      stopReason: 'end_turn',
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId)).toBeNull());
    expect(queryByTestId('chat-working-indicator')).toBeNull();

    // Round 2 begins (Claude system.init re-emit after a
    // task_notification provoked another model call). Distinct
    // turnId, fresh startedAt — the elapsed-time anchor resets and
    // the indicator re-renders.
    const round2StartedAt = Date.now() + 5_000;
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'round-2',
      turnIndex: 0,
      startedAt: round2StartedAt,
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId)?.turnId).toBe('round-2'));
    expect(getActiveTurn(pane.threadId)?.startedAt).toBe(round2StartedAt);
    expect(await findByTestId('chat-working-indicator')).toBeInTheDocument();

    // Round 2 ends — the cascade is complete and the indicator
    // hides for good.
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'round-2',
      turnIndex: 0,
      startedAt: round2StartedAt,
      completedAt: round2StartedAt + 800,
      stopReason: 'end_turn',
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId)).toBeNull());
    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  // Composer is enabled between rounds — the user can send a
  // follow-up prompt during the bg-wait gap. Matches Claude Code's
  // actual behaviour and is the canonical wire-round emission
  // contract. Without this assertion, a regression that gates the
  // composer on logical-turn-active would silently lock the user out
  // of the gap window.
  it('composer is enabled between rounds in a multi-result-per-turn cascade', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();

    // Round 1 starts → composer disabled (mid-round).
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'round-1',
      turnIndex: 0,
      startedAt: Date.now(),
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId)?.turnId).toBe('round-1'));

    // Round 1 ends → composer should be enabled during the gap.
    // The Send button flips back to its operational state and Enter
    // is no longer blocked by the mid-turn guard.
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'round-1',
      turnIndex: 0,
      startedAt: Date.now(),
      completedAt: Date.now() + 500,
      stopReason: 'end_turn',
    });
    await waitFor(() => expect(getActiveTurn(pane.threadId)).toBeNull());

    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(false);
    await fireEvent.input(textarea, { target: { value: 'follow-up between rounds' } });
    await flush();

    const sendBtn = getByTestId('composer-send') as HTMLButtonElement;
    await waitFor(() => expect(sendBtn.disabled).toBe(false));
  });
});
