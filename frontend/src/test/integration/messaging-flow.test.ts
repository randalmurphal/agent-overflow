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
// NOTE: callers should install SendMessage / InterruptTurn mocks themselves
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
    // Default SendMessage + InterruptTurn mocks — tests that need to spy
    // or reject reassign these with fresh `setBindingMock` calls.
    setBindingMock('SendMessage', async () => {});
    setBindingMock('InterruptTurn', async () => {});
  });

  it('sends a message and clears the composer draft', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    // Re-assign the mock AFTER mount so the call count starts at 0.
    const sendMock = setBindingMock('SendMessage', async () => {});

    // Composer textarea is keyed with aria-label "Message input".
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(false);
    await fireEvent.input(textarea, { target: { value: 'hello agent' } });
    await flush();

    const sendBtn = getByTestId('composer-send') as HTMLButtonElement;
    await waitFor(() => expect(sendBtn.disabled).toBe(false));
    await fireEvent.click(sendBtn);
    await waitFor(() => expect(sendMock).toHaveBeenCalled());

    expect(sendMock.mock.calls[0][0]).toBe('thread-1');
    expect(sendMock.mock.calls[0][1]).toBe('hello agent');
    expect(textarea.value).toBe('');
  });

  it('blocks Enter during an active turn and surfaces the mid-turn banner', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    const sendMock = setBindingMock('SendMessage', async () => {});
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'queued message' } });
    await flush();

    // Post-refactor isTurnActive is wire-pushed (invariant 22). Simulate
    // the real Go → frontend path by emitting provider:turn_started. A
    // streaming item no longer flips the composer's active-turn guard
    // on its own.
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

    // Enter must not fire SendMessage while a turn is active — the
    // interrupt button is the only path to cancellation. A polite
    // mid-turn error is announced inline in the composer.
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    await flush();
    expect(sendMock).not.toHaveBeenCalled();
    const err = getByTestId('composer-midturn-error');
    expect(err.textContent).toMatch(/Cannot send/);
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
    expect(await findByText(/Bash: ls -la/)).toBeInTheDocument();

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

  it('handles SendMessage rejection by restoring draft and logging the error', async () => {
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    // Override SendMessage to reject AFTER mount so earlier calls don't trip.
    setBindingMock('SendMessage', async () => {
      throw new Error('rpc down');
    });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

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
    // Post-refactor pane.isTurnActive only clears on provider:turn_completed
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
    await waitFor(() => expect(pane.isTurnActive).toBe(false));
  });

  // End-to-end turn lifecycle — every stage on the real wire path:
  // turn_started → tool lifecycle → turn_completed → divider renders
  // above the assistant message. This test is intentionally broad: it
  // is the single integration test that proves all of the invariant-22
  // pieces line up together (pane.activeTurn flip, pane.latestSettledTurn
  // write, CompletionDivider DOM render).
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

    // 1. Turn starts. pane.activeTurn is populated; pane has no
    // settled turn yet. The sidebar pill is the user-facing surface
    // for "turn is running" and is covered by its own tests.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 't1',
      turnIndex: 0,
      startedAt: 1,
    });
    await waitFor(() => {
      expect(pane.activeTurn).toEqual({ turnId: 't1', turnIndex: 0, startedAt: 1 });
    });
    expect(pane.isTurnActive).toBe(true);
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
    expect(await findByText(/Bash: echo hello/)).toBeInTheDocument();

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
    // race. pane.activeTurn MUST flip to null immediately; the
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
      expect(pane.activeTurn).toBeNull();
    });
    expect(pane.isTurnActive).toBe(false);
    expect(pane.latestSettledTurn?.turnId).toBe('t1');
    expect(pane.latestSettledTurn?.assistantMessageId).toBe('assist1');
    expect(pane.latestSettledTurn?.tokenUsage).toEqual({
      inputTokens: 42,
      outputTokens: 18,
      cacheReadInputTokens: 0,
      totalCostUsd: 0.00123,
    });
    // At this point the divider has nothing to render above because
    // assist1 hasn't landed in pane.items yet.
    expect(queryByTestId('completion-divider')).toBeNull();

    // 4. The final assistant_text arrives. CompletionDivider mounts
    // immediately before it in the DOM because
    // latestSettledTurn.assistantMessageId matches.
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
    const divider = await findByTestId('completion-divider');
    expect(divider.getAttribute('data-turn-id')).toBe('t1');

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
    await waitFor(() => expect(pane.isTurnActive).toBe(true));

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

    // The inline ToolCallCard renders "…" in the status chip while
    // status=running AND isBackground=true. This is the spec's sole
    // render signal for "work dispatched, waiting for sibling" —
    // invariant 24 + §UI components driven by this state. The chip
    // replaces "running"; no separate badge element is rendered.
    const status = await findByTestId('tool-call-card-status');
    expect(status.textContent?.trim()).toBe('…');
    expect(status.getAttribute('aria-label')).toBe('Backgrounded');

    // 3. Turn ends while the backgrounded work is still in flight.
    // pane.activeTurn clears, the working indicator hides, and the
    // launch row is UNTOUCHED (no status flip, no sibling row yet).
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'tbg',
      turnIndex: 0,
      startedAt,
      completedAt: startedAt + 500,
      stopReason: 'end_turn',
    });
    await waitFor(() => expect(pane.activeTurn).toBeNull());
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
    // tray refresh has time to pick up the completion.
    expect((await findByTestId('background-task-tray-count')).textContent).toBe('1');
    await waitFor(async () => {
      const rowStatus = await findByTestId('background-task-tray-row-status');
      expect(rowStatus.getAttribute('data-status')).toBe('completed');
    });
  });
});
