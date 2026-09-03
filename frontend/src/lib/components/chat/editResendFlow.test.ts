// The edit-and-resend flow as ChatView drives it: the stage machine's
// invalidation matrix, the attachment-record policy on every exit, and
// the committed-failure recovery that rebuilds the composer from LIVE
// state. ChatView.test.ts owns the happy path and the basic stage
// assertions; this file owns the interleavings — a thread switch or an
// anchor removal arriving at each stage, a second submit racing the
// first, and the four recovery branches, which are the parts that are a
// nightmare to reach by hand.

import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';

// The transport-class branch waits for the socket to come back before it
// touches the composer. Drive that from the test instead of a real
// connection; everything else in the store (notably the classifier the
// branch is selected by) stays real.
const transport = vi.hoisted(() => ({
  connected: null as { promise: Promise<void>; resolve: () => void } | null,
}));
vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../stores/transportStatus.svelte')>();
  return {
    ...actual,
    whenTransportConnected: () => transport.connected?.promise ?? Promise.resolve(),
  };
});

import ChatView from './ChatView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { focusPane, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import {
  getComposerDraftForPane,
  resetComposerDraftRegistryForTest,
} from '../../stores/composerDraftRegistry.svelte';
import { resetForTest as resetThreadStatuses } from '../../stores/threadStatuses.svelte';
import {
  applyUserMessageReverted,
  resetResendRevertMarkersForTest,
} from '../../stores/eventsMessageRevert';
import { getToasts } from '../../stores/toast.svelte';
import { DisconnectedError, TransportError } from '../../transport/wsClient';
import { resetEditResendExecutionForTest } from './editResendFlow.svelte';
import type { Item, Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import {
  installPaneMocks,
  installThreadSwitchMocks,
  makeItem,
  stubScrollController,
} from '../../../test/helpers/chat';
import { resetLayoutMetricsForTest } from '../../stores/layoutMetrics.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { idleWorkspaceActivity } from '../../../test/helpers/workspaceLock';

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() { onfinish?.(); },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() { return onfinish; },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
      };
  }
});

beforeEach(() => {
  Object.defineProperty(window, 'innerWidth', {
    configurable: true,
    writable: true,
    value: 1400,
  });
  resetLayoutMetricsForTest();
  resetPaneLayoutForTest();
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetThreadStatuses();
  resetComposerDraftSnapshotsForTest();
  resetComposerDraftRegistryForTest();
  resetResendRevertMarkersForTest();
  resetEditResendExecutionForTest();
  transport.connected = null;
});

function seedThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

interface DraftRow {
  content: string;
  attachmentIds?: string[];
  terminalChips?: unknown[];
  sourceProposedPlan?: unknown;
}

/**
 * Backend draft rows keyed by thread, mutable mid-test so a mocked RPC
 * can simulate the backend staging (or a racing save overwriting) the
 * crash copy while the flow is in flight.
 */
function mockDraftRows(rows: Map<string, DraftRow>): Map<string, DraftRow> {
  setBindingMock('GetDraft', async (threadId: string) => {
    const row = rows.get(threadId);
    return {
      threadId,
      content: row?.content ?? '',
      attachmentIds: row?.attachmentIds ?? [],
      terminalChips: row?.terminalChips ?? [],
      sourceProposedPlan: row?.sourceProposedPlan ?? null,
      updatedAt: 0,
    };
  });
  return rows;
}

function userItem(id: string, turnIndex: number, summary: string, threadId = 'thread-1'): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    kind: 'user_text',
    role: 'user',
    summary,
  });
}

/** A sent message that carried a file attachment, as its meta records it. */
function userItemWithFileAttachment(id: string, turnIndex: number, summary: string): Item {
  return makeItem({
    id,
    threadId: 'thread-1',
    turnIndex,
    itemIndex: 0,
    kind: 'user_text',
    role: 'user',
    summary,
    meta: JSON.stringify({
      attachments: [{
        id: 'att-sent-pdf',
        threadId: 'thread-1',
        filename: 'report.pdf',
        mimeType: 'application/pdf',
        size: 2048,
        kind: 'file',
      }],
    }),
  });
}

async function buildPane(
  thread: Thread = seedThread(),
  items: Item[] = [],
  paneId = 'pane',
): Promise<ReturnType<typeof createThreadPane>> {
  installPaneMocks(items);
  setBindingMock('SwitchThread', async () => thread);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  setBindingMock('CountRunningBackgroundTasks', async () => 0);
  setBindingMock('GetGitStatus', async () => ({
    isRepo: false,
    branch: '',
    hasChanges: false,
    hasUpstream: false,
    isDefaultBranch: false,
    aheadCount: 0,
    behindCount: 0,
    openPrUrl: '',
    dirty: false,
    files: [],
  }));
  setBindingMock('GitListBranches', async () => []);
  mockDraftRows(new Map([[thread.id, { content: '' }]]));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('DeleteAttachment', async () => {});
  setBindingMock('UploadAttachment', async () => ({
    id: 'att-pasted',
    threadId: thread.id,
    filename: 'shot.png',
    mimeType: 'image/png',
    size: 64,
    relativePath: `${thread.id}/shot.png`,
    createdAt: 1,
    kind: 'image',
  }));

  const pane = createThreadPane({ paneId });
  registerPaneForTest(paneId, pane);
  focusPane(paneId);
  await pane.switchThread(thread);
  return pane;
}

interface EditorQueries {
  getAllByLabelText(text: string): HTMLElement[];
  getByTestId(id: string): HTMLElement;
}

async function openMessageEditor(view: EditorQueries, index = 0): Promise<HTMLElement> {
  await fireEvent.click(view.getAllByLabelText('Edit message and resend from here')[index]);
  await waitFor(() => expect(view.getByTestId('user-message-editor')).toBeInTheDocument());
  return view.getByTestId('user-message-editor');
}

async function typeInEditor(editor: HTMLElement, value: string): Promise<void> {
  const textarea = within(editor).getByLabelText('Message Input') as HTMLTextAreaElement;
  await fireEvent.input(textarea, { target: { value } });
  textarea.setSelectionRange(value.length, value.length);
  await fireEvent.select(textarea);
  await tick();
}

/** Paste an image into the editor so the session owns an uploaded record. */
async function pasteImageIntoEditor(editor: HTMLElement): Promise<void> {
  const textarea = within(editor).getByLabelText('Message Input') as HTMLTextAreaElement;
  const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
  Object.defineProperty(event, 'clipboardData', {
    value: {
      items: [{
        kind: 'file',
        type: 'image/png',
        getAsFile: () => new File(['x'], 'shot.png', { type: 'image/png' }),
      }],
    },
  });
  textarea.dispatchEvent(event);
  await waitFor(() => expect(
    within(editor).queryByTestId('composer-attachment-row'),
  ).not.toBeNull());
}

/** A promise the test resolves/rejects by hand, plus its settle handles. */
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve: (v: T) => void = () => {};
  let reject: (e: unknown) => void = () => {};
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function errorToastCount(): number {
  return getToasts().filter((t) => t.type === 'error').length;
}

/**
 * `render` binds its queries to `document.body`, so two mounted panes
 * would answer each other's lookups. Scope every query to the pane's own
 * container instead.
 */
function paneQueries(container: HTMLElement) {
  const q = within(container);
  return {
    async openEditor(): Promise<HTMLElement> {
      await fireEvent.click(q.getAllByLabelText('Edit message and resend from here')[0]);
      await waitFor(() => expect(q.getByTestId('user-message-editor')).toBeInTheDocument());
      return q.getByTestId('user-message-editor');
    },
    send(): HTMLElement {
      return q.getByTestId('user-message-edit-send');
    },
    editorPresent(): boolean {
      return q.queryByTestId('user-message-editor') !== null;
    },
  };
}

describe('edit-and-resend flow — invalidation matrix', () => {
  // Every cell parks the flow at a known stage, then fires the
  // invalidation. What must hold is the same three facts each time: the
  // editor is gone, the destructive RPC never ran (or ran exactly once
  // and is allowed to finish), and the attachment records this session
  // uploaded are cleaned up IFF the send does not own them yet.

  it('voids a preflight flow on a thread switch and reclaims its uploads', async () => {
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const count = deferred<number>();
    setBindingMock('CountRunningBackgroundTasks', () => count.promise);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    // Parked in preflight: the count RPC has not answered.
    expect(resend).not.toHaveBeenCalled();

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);

    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    // Nothing was sent, so the pasted record backs nothing. The reclaim
    // names the EDITED message's thread, not the one the pane switched
    // to — deletion is ownership-checked, so the pane's current thread
    // would be refused.
    await waitFor(() => expect(deleteAttachment).toHaveBeenCalledWith(thread.id, 'att-pasted'));
    count.resolve(0);
    await tick();
    expect(resend).not.toHaveBeenCalled();
  });

  it('voids a confirm-stage flow on a thread switch and reclaims its uploads', async () => {
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 3);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await view.findByText('Revert to this message?');

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);

    await waitFor(() => expect(view.queryByText('Revert to this message?')).toBeNull());
    expect(view.queryByTestId('user-message-editor')).toBeNull();
    await waitFor(() => expect(deleteAttachment).toHaveBeenCalledWith(thread.id, 'att-pasted'));
    expect(resend).not.toHaveBeenCalled();
  });

  it('voids an executing flow on a thread switch but leaves its uploads alone', async () => {
    // The exemption with teeth: the send is already carrying those ids,
    // so deleting the records would break a message that may well land.
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => resend.promise);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(view.getByTestId('user-message-edit-send')).toBeDisabled());

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());

    resend.resolve();
    await tick();
    expect(deleteAttachment).not.toHaveBeenCalled();
  });

  it('voids a preflight flow when the anchor row is removed externally', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const count = deferred<number>();
    setBindingMock('CountRunningBackgroundTasks', () => count.promise);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    // An un-send, or a concurrent revert reflected from a second pane.
    pane.removeRevertedItems(item.turnIndex, []);
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());

    count.resolve(0);
    await tick();
    // The flow was voided before the count answered, so the doomed RPC
    // against a deleted anchor never fires.
    expect(resend).not.toHaveBeenCalled();
  });

  it('voids a confirm-stage flow when the anchor row is removed externally', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 2);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await view.findByText('Revert to this message?');

    pane.removeRevertedItems(item.turnIndex, []);
    await waitFor(() => expect(view.queryByText('Revert to this message?')).toBeNull());
    expect(view.queryByTestId('user-message-editor')).toBeNull();
    expect(resend).not.toHaveBeenCalled();
  });

  it('sends once when Send is double-clicked before the stage repaints', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);
    const send = view.getByTestId('user-message-edit-send');
    // No await between them: the second click lands in the same task, so
    // a guard that only relied on the button's rendered `disabled` would
    // let it through.
    void fireEvent.click(send);
    void fireEvent.click(send);
    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));
    await tick();
    expect(resend).toHaveBeenCalledTimes(1);
  });

  it('sends once when the kill confirmation is double-confirmed', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 1);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await view.findByText('Revert to this message?');
    const confirm = within(view.getByRole('dialog')).getByText('Revert & send');

    void fireEvent.click(confirm);
    void fireEvent.click(confirm);
    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));
    await tick();
    expect(resend).toHaveBeenCalledTimes(1);
    expect(resend).toHaveBeenCalledWith(
      thread.id,
      item.id,
      expect.objectContaining({ killRunningBackgroundTasks: true }),
    );
  });
});

describe('edit-and-resend flow — attachment-record policy on exit', () => {
  it('reclaims a session upload when the edit is discarded', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);

    await fireEvent.click(view.getByTestId('user-message-edit-cancel'));
    await view.findByText('Discard changes?');
    await fireEvent.click(view.getByText('Discard'));

    await waitFor(() => expect(deleteAttachment).toHaveBeenCalledWith(thread.id, 'att-pasted'));
  });

  it('a SEEDED file chip removed in the editor keeps its record — the sent message still owns it', async () => {
    const thread = seedThread();
    const item = userItemWithFileAttachment('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);

    // The edit copy seeds the file the same way it seeds an image.
    await waitFor(() => expect(within(editor).getByTestId('attachment-file-chip')).toBeInTheDocument());
    await fireEvent.click(within(editor).getByLabelText('Remove report.pdf'));

    await waitFor(() => expect(within(editor).queryByTestId('attachment-file-chip')).toBeNull());
    // Dropping it from THIS draft must not destroy the record the original
    // message references — only ids this session uploaded are reclaimable.
    expect(deleteAttachment).not.toHaveBeenCalled();
  });

  it('reclaims nothing when the resend succeeds — the sent message owns the ids', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));
    expect(resend).toHaveBeenCalledWith(
      expect.any(String),
      expect.any(String),
      expect.objectContaining({ attachmentIds: ['att-pasted'] }),
    );
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    expect(deleteAttachment).not.toHaveBeenCalled();
  });

  it('reclaims a session upload when the pane is destroyed mid-edit', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);

    view.unmount();
    await waitFor(() => expect(deleteAttachment).toHaveBeenCalledWith(thread.id, 'att-pasted'));
  });

  it('leaves a session upload alone when the pane is destroyed mid-execute', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => resend.promise);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(view.getByTestId('user-message-edit-send')).toBeDisabled());

    view.unmount();
    resend.resolve();
    await tick();
    expect(deleteAttachment).not.toHaveBeenCalled();
  });
});

describe('edit-and-resend flow — composer save pipeline', () => {
  it('waits out an in-flight composer save before the destructive RPC', async () => {
    // The rider fix: a composer save that is already on the wire would
    // otherwise land AFTER the backend stages its merged crash copy and
    // overwrite it. `prepareForExternalDraftReplace` makes the RPC wait.
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);

    const order: string[] = [];
    const save = deferred<void>();
    setBindingMock('SaveDraft', () => {
      order.push('save');
      return save.promise;
    });
    setBindingMock('RevertConversationAndResendMessage', async () => {
      order.push('revert');
    });

    const view = render(ChatView, { props: { pane } });
    const composerDraft = getComposerDraftForPane(pane.paneId);
    expect(composerDraft).toBeDefined();
    composerDraft!.setContent('composer work in progress');
    // Let the 500ms debounce fire so the save is genuinely in flight.
    await waitFor(() => expect(order).toEqual(['save']), { timeout: 3000 });

    await openMessageEditor(view);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    // Still parked: the RPC is behind the unresolved save.
    await tick();
    await tick();
    expect(order).toEqual(['save']);

    save.resolve();
    await waitFor(() => expect(order).toEqual(['save', 'revert']));
  });

  it('drops a debounced composer save rather than letting it land after the RPC', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);

    const order: string[] = [];
    setBindingMock('SaveDraft', async () => { order.push('save'); });
    setBindingMock('RevertConversationAndResendMessage', async () => { order.push('revert'); });

    const view = render(ChatView, { props: { pane } });
    const composerDraft = getComposerDraftForPane(pane.paneId);
    // Armed but NOT yet fired — the whole point of the window.
    composerDraft!.setContent('composer work in progress');

    await openMessageEditor(view);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(order).toContain('revert'));

    // Well past the debounce: the armed save was cancelled, not deferred.
    await new Promise((resolve) => setTimeout(resolve, 800));
    expect(order).toEqual(['revert']);
  });
});

describe('edit-and-resend flow — committed-failure recovery', () => {
  // The revert committed and the resend did not. The editor's row is
  // gone, so the composer is the only surface left — and the recovery
  // rebuilds from LIVE frontend state rather than trusting the backend's
  // crash-copy row, because a composer save fired during the RPC can
  // have overwritten it.

  // Mirror a committed revert the way the wire delivers it: through the
  // event handler, which both truncates the panes AND records the
  // draftPendingResend marker the failure handler consults. A test that
  // truncated the pane directly would leave the marker unset and be
  // (correctly) classified as a mere refusal.
  function commitRevert(item: Item): void {
    applyUserMessageReverted({
      threadId: item.threadId,
      userItemId: item.id,
      turnIndex: item.turnIndex,
      keptAnchorTurnItemIds: [],
      draftPendingResend: true,
    });
  }

  function commitRevertThenFail(item: Item) {
    return () => {
      commitRevert(item);
      return Promise.reject(new Error('revert and resend: resend failed: provider died'));
    };
  }

  it('keeps BOTH texts when the user typed into the composer during the RPC', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDraftRows(new Map([[thread.id, { content: '' }]]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => {
      commitRevert(item);
      return resend.promise;
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());

    // Keystrokes DURING the RPC. The crash copy in the backend row
    // cannot know about these — only live state does.
    const composerDraft = getComposerDraftForPane(pane.paneId);
    composerDraft!.setContent('typed while the saga ran');
    await tick();

    resend.reject(new Error('revert and resend: resend failed: provider died'));
    await waitFor(() => {
      expect(view.getByLabelText('Message Input')).toHaveValue(
        'Update TWO of the lines\n\ntyped while the saga ran',
      );
    });
  });

  it('paints the recovered draft even when persisting it fails', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    setBindingMock('RevertConversationAndResendMessage', commitRevertThenFail(item));
    setBindingMock('SaveDraft', async () => { throw new Error('disk is gone'); });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    // The paint happens first and unconditionally: the text is on screen
    // and sendable even though nothing durable holds it.
    await waitFor(() => {
      expect(view.getByLabelText('Message Input')).toHaveValue('Update TWO of the lines');
    });
  });

  it('writes nothing when the switched-away thread still holds the intact crash copy', async () => {
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const rows = mockDraftRows(new Map<string, DraftRow>([
      [thread.id, { content: '' }],
      [other.id, { content: '' }],
    ]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => {
      commitRevert(item);
      // The backend staged its merged crash copy before truncating.
      rows.set(thread.id, { content: 'Update TWO of the lines\n\nleftover WIP' });
      return resend.promise;
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);
    await tick();

    const saveDraft = setBindingMock('SaveDraft', async () => {});
    // Re-mocked as a spy so the negative below can wait for the recovery
    // to have actually READ the row before concluding it wrote nothing.
    const getDraft = setBindingMock('GetDraft', async (threadId: string) => {
      const row = rows.get(threadId);
      return {
        threadId,
        content: row?.content ?? '',
        attachmentIds: row?.attachmentIds ?? [],
        terminalChips: [],
        sourceProposedPlan: null,
        updatedAt: 0,
      };
    });
    resend.reject(new Error('revert and resend: resend failed: provider died'));
    await waitFor(() => {
      expect(getDraft.mock.calls.some((call) => call[0] === thread.id)).toBe(true);
    });
    // Let the whole recovery continuation drain — a write, if it were
    // going to happen, is two microtask hops past the read.
    await new Promise((resolve) => setTimeout(resolve, 50));

    // The row already CONTAINS the edited text, so it is the intact
    // backend crash copy — rewriting it could only race a fresher save.
    const writesToOldThread = saveDraft.mock.calls.filter((call) => call[0] === thread.id);
    expect(writesToOldThread).toHaveLength(0);
    // ...and the thread the user is now looking at is untouched.
    expect(view.getByLabelText('Message Input')).toHaveValue('');
  });

  it('merges edited-first into a switched-away row that was overwritten', async () => {
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const rows = mockDraftRows(new Map<string, DraftRow>([
      [thread.id, { content: '' }],
      [other.id, { content: '' }],
    ]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => {
      commitRevert(item);
      // A composer save (or the switch flush) landed on top of the crash
      // copy: the edited text is NOT in the row any more.
      rows.set(thread.id, { content: 'a racing composer save' });
      return resend.promise;
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);
    await tick();

    const saveDraft = setBindingMock('SaveDraft', async () => {});
    resend.reject(new Error('revert and resend: resend failed: provider died'));

    await waitFor(() => {
      const write = saveDraft.mock.calls.find((call) => call[0] === thread.id);
      expect(write).toBeDefined();
      expect(write![1]).toBe('Update TWO of the lines\n\na racing composer save');
    });
    // The composer the user is actually looking at never took the paint.
    expect(view.getByLabelText('Message Input')).toHaveValue('');
  });

  it('surfaces an error rather than losing the edit when the old row cannot be read', async () => {
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => {
      commitRevert(item);
      return resend.promise;
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);
    await tick();

    setBindingMock('GetDraft', async () => { throw new Error('store is closed'); });
    const before = errorToastCount();
    resend.reject(new Error('revert and resend: resend failed: provider died'));

    // Two toasts: the failure itself, and the explicit "could not put
    // your text anywhere durable" — silence here would be a silent loss.
    await waitFor(() => expect(errorToastCount()).toBeGreaterThanOrEqual(before + 2));
    expect(getToasts().some((t) => /restore edited message to the draft/i.test(t.message)))
      .toBe(true);
  });

  it('treats a refusal after a mid-RPC thread switch as nothing-committed', async () => {
    // The regression this pins: with the old structural check
    // (`pane.items.some(anchor)`), a pane that switched threads mid-RPC
    // holds ANOTHER thread's rows, so a plain guard refusal — no event,
    // nothing committed — was misread as a committed failure and the
    // "recovery" wrote the edited text into the old thread's draft row
    // while the original message still sat in its conversation. The
    // reverted-event marker is the discriminator now: no event, no
    // marker, no write — and the session upload the executing-stage void
    // deliberately left behind (for a send that never happened) is
    // reclaimed.
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDraftRows(new Map<string, DraftRow>([
      [thread.id, { content: '' }],
      [other.id, { content: '' }],
    ]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => resend.promise);

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await typeInEditor(editor, 'Update TWO of the lines');
    const saveDraft = setBindingMock('SaveDraft', async () => {});
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);
    await tick();
    expect(deleteAttachment).not.toHaveBeenCalled();

    resend.reject(new Error('revert and resend: cannot revert while a turn is in progress'));
    await waitFor(() => {
      expect(deleteAttachment.mock.calls.some((call) => call[1] === 'att-pasted')).toBe(true);
    });
    expect(getToasts().some((t) => t.message.startsWith('Edit failed:'))).toBe(true);
    // No recovery write anywhere: the old thread still holds its message,
    // and the new thread's composer was never involved.
    expect(saveDraft.mock.calls.filter((call) => call[0] === thread.id)).toHaveLength(0);
    expect(view.getByLabelText('Message Input')).toHaveValue('');
  });

});

describe('edit-and-resend flow — pending-cut dimming', () => {
  it('spares anchor-turn rows that PRECEDE the anchor and clears on success', async () => {
    // Claude's cut is item-granular, so a row sharing the anchor's turn
    // at a LOWER item index survives — dimming it would promise a
    // destruction the backend will not perform.
    const thread = seedThread();
    const sameTurnBefore = makeItem({
      id: 'user:0',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'queued earlier in this turn',
    });
    const anchor = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'anchor prompt',
    });
    const sameTurnAfter = makeItem({
      id: 'assistant:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 2,
      kind: 'assistant_text',
      role: 'assistant',
      summary: 'an answer',
    });
    const pane = await buildPane(thread, [sameTurnBefore, anchor, sameTurnAfter]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => resend.promise);

    const view = render(ChatView, { props: { pane } });
    const rows = () => Array.from(
      view.container.querySelectorAll<HTMLElement>('[data-row-index]'),
    ).map((row) => row.classList.contains('chat-row-pending-cut'));

    await openMessageEditor(view, 1);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(rows()).toEqual([false, false, true]));

    resend.resolve();
    await waitFor(() => expect(rows()).toEqual([false, false, false]));
  });

  it('clears the dimming when the flow ends in failure too', async () => {
    const thread = seedThread();
    const anchor = userItem('user:1', 1, 'anchor prompt');
    const later = userItem('user:2', 2, 'later prompt');
    const pane = await buildPane(thread, [anchor, later]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => resend.promise);

    const view = render(ChatView, { props: { pane } });
    const rows = () => Array.from(
      view.container.querySelectorAll<HTMLElement>('[data-row-index]'),
    ).map((row) => row.classList.contains('chat-row-pending-cut'));

    await openMessageEditor(view, 0);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(rows()).toEqual([false, true]));

    // A guard refusal: the anchor survives, the editor comes back, and
    // nothing may stay dimmed — nothing is being destroyed.
    resend.reject(new Error('revert and resend: cannot revert while a turn is in progress'));
    await waitFor(() => expect(rows()).toEqual([false, false]));
    expect(view.getByTestId('user-message-editor')).toBeInTheDocument();
  });
});

describe('edit-and-resend flow — one execute per thread', () => {
  // The backend serializes sagas on the thread's action lock, so a second
  // concurrent RPC is refused rather than run. What the lock cannot
  // protect is the FRONTEND's outcome classification: both flows consume
  // reverted-event markers and both rebuild a composer from live state,
  // so two panes editing one thread at once would attribute each other's
  // events. The registry is what stops the second one starting.

  it('refuses a second pane\'s submit while one is executing, and allows it after', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const paneA = await buildPane(thread, [item], 'pane-a');
    const paneB = await buildPane(thread, [item], 'pane-b');
    const count = setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    const resendMock = setBindingMock('RevertConversationAndResendMessage', () => resend.promise);

    const viewA = render(ChatView, { props: { pane: paneA } });
    const viewB = render(ChatView, { props: { pane: paneB } });
    const a = paneQueries(viewA.container);
    const b = paneQueries(viewB.container);

    await a.openEditor();
    await fireEvent.click(a.send());
    await waitFor(() => expect(resendMock).toHaveBeenCalledTimes(1));
    const countCallsAfterFirstSubmit = count.mock.calls.length;

    await b.openEditor();
    const before = errorToastCount();
    await fireEvent.click(b.send());
    await tick();

    // Refused before anything went on the wire — not even the preflight
    // count — and the second editor is still sitting there with its text.
    expect(count.mock.calls.length).toBe(countCallsAfterFirstSubmit);
    expect(resendMock).toHaveBeenCalledTimes(1);
    expect(errorToastCount()).toBe(before + 1);
    expect(b.editorPresent()).toBe(true);
    expect(b.send()).not.toBeDisabled();

    // The lane is released on settle, so the second pane can go now.
    resend.resolve();
    await waitFor(() => expect(a.editorPresent()).toBe(false));
    await fireEvent.click(b.send());
    await waitFor(() => expect(resendMock).toHaveBeenCalledTimes(2));
  });
});

describe('edit-and-resend flow — transport-class failure', () => {
  // The marker-based classification is only valid while the socket
  // survived the RPC. When it did not, the frontend cannot know whether
  // the saga committed — and for a timed-out call the saga can still
  // COMPLETE afterwards, so it cannot know whether a committed saga's
  // resend succeeded either. So it guesses nothing and throws nothing
  // away: the editor comes back holding the edit, and the saga's own
  // outcome resolves it — the anchor row surviving the gap replay means
  // nothing committed, its removal means something did.

  it('hands the editor back with the edit intact, keeps the uploads, and reloads on reconnect', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => resend.promise);

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await typeInEditor(editor, 'Update TWO of the lines');

    transport.connected = deferred<void>();
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});
    const saveDraft = setBindingMock('SaveDraft', async () => {});
    const getDraft = setBindingMock('GetDraft', async (threadId: string) => ({
      threadId,
      content: '',
      attachmentIds: [],
      terminalChips: [],
      sourceProposedPlan: null,
      updatedAt: 0,
    }));
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(view.getByTestId('user-message-edit-send')).toBeDisabled());

    const before = errorToastCount();
    resend.reject(new DisconnectedError());

    // Nothing is lost: the editor is back, sendable, with the user's text.
    await waitFor(() => expect(view.getByTestId('user-message-edit-send')).not.toBeDisabled());
    expect(within(view.getByTestId('user-message-editor')).getByLabelText('Message Input'))
      .toHaveValue('Update TWO of the lines');
    await new Promise((resolve) => setTimeout(resolve, 20));

    // A resend that DID land references the session's attachment ids, and
    // its own message would already be in the transcript — so neither the
    // records nor the composer may be rewritten on a guess.
    expect(deleteAttachment).not.toHaveBeenCalled();
    expect(saveDraft.mock.calls.filter((call) => call[0] === thread.id)).toHaveLength(0);
    expect(getDraft).not.toHaveBeenCalled();
    expect(errorToastCount()).toBe(before + 1);
    expect(getToasts().some((t) => /connection lost while resending/i.test(t.message))).toBe(true);

    // Reconnect: the backend's own row is the only party that knows what
    // happened, so the composer is refreshed from it.
    transport.connected!.resolve();
    await waitFor(() => {
      expect(getDraft.mock.calls.some((call) => call[0] === thread.id)).toBe(true);
    });

    // Even with the anchor on screen, the flow's uploads stay held on a
    // later discard: the anchor is a STALE witness until the resync
    // replays whatever the socket lost, and there is no replay-complete
    // signal to wait on. An orphaned record is invisible; deleting one a
    // landed resend references corrupts a visible message — so an
    // outcome-unknown flow never reclaims.
    await fireEvent.click(view.getByTestId('user-message-edit-cancel'));
    await view.findByText('Discard changes?');
    await fireEvent.click(view.getByText('Discard'));
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(deleteAttachment).not.toHaveBeenCalled();
  });

  it('lets the anchor removal void the returned editor, without reclaiming its uploads', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    const rows = mockDraftRows(new Map<string, DraftRow>([[thread.id, { content: '' }]]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => {
      // The revert committed and its event arrived; the RPC's ANSWER is
      // what the timeout lost. The saga may still be running server-side,
      // so "committed" says nothing about whether the resend succeeded.
      applyUserMessageReverted({
        threadId: item.threadId,
        userItemId: item.id,
        turnIndex: item.turnIndex,
        keptAnchorTurnItemIds: [],
        draftPendingResend: true,
      });
      rows.set(thread.id, { content: 'crash copy from the backend' });
      return resend.promise;
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await pasteImageIntoEditor(editor);
    await typeInEditor(editor, 'Update TWO of the lines');

    transport.connected = deferred<void>();
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});
    const saveDraft = setBindingMock('SaveDraft', async () => {});
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).not.toBeNull());

    resend.reject(new TransportError('timeout', 'RPC timed out'));
    // The anchor is already gone, so the editor the branch handed back is
    // voided by the anchor-removed invalidation on the very next pass —
    // the executing-stage exemption does not apply to an 'editing' flow.
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    await new Promise((resolve) => setTimeout(resolve, 20));

    // That void must NOT run the ordinary reclaim: the resent message, or
    // the crash-copy draft row, may own those ids now.
    expect(deleteAttachment).not.toHaveBeenCalled();
    // No optimistic paint and no restoreDraftFor either: the edited text
    // may already be in the transcript as a sent message.
    expect(view.getByLabelText('Message Input')).toHaveValue('');
    expect(saveDraft.mock.calls.filter((call) => call[0] === thread.id)).toHaveLength(0);

    // What the composer ends up holding is the backend's answer, whatever
    // it turned out to be.
    transport.connected!.resolve();
    await waitFor(() => {
      expect(view.getByLabelText('Message Input')).toHaveValue('crash copy from the backend');
    });
  });

  it('scopes the outcome-unknown hold to the flow that lost the connection', async () => {
    // The hold is per-FLOW. A fresh edit has sent nothing, so its own
    // uploads are unambiguously its own — even on the same anchor, in the
    // same pane, right after a flow that ended in doubt.
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = deferred<void>();
    setBindingMock('RevertConversationAndResendMessage', () => resend.promise);
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});

    const view = render(ChatView, { props: { pane } });
    const first = await openMessageEditor(view);
    await typeInEditor(first, 'Update TWO of the lines');
    transport.connected = deferred<void>();
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(view.getByTestId('user-message-edit-send')).toBeDisabled());
    resend.reject(new DisconnectedError());
    await waitFor(() => expect(view.getByTestId('user-message-edit-send')).not.toBeDisabled());

    // Leave the doubtful flow the way a user would: discard it. The
    // anchor is alive, so this is the ordinary path.
    await fireEvent.click(view.getByTestId('user-message-edit-cancel'));
    await view.findByText('Discard changes?');
    await fireEvent.click(view.getByText('Discard'));
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    expect(deleteAttachment).not.toHaveBeenCalled(); // nothing was uploaded in it

    const second = await openMessageEditor(view);
    await pasteImageIntoEditor(second);
    await fireEvent.click(view.getByTestId('user-message-edit-cancel'));
    await view.findByText('Discard changes?');
    await fireEvent.click(view.getByText('Discard'));

    // The new session's upload is reclaimed normally: the previous flow's
    // doubt did not leak onto it.
    await waitFor(() => expect(deleteAttachment).toHaveBeenCalledWith(thread.id, 'att-pasted'));
  });
});

describe('edit-and-resend flow — intercepted commands', () => {
  // AO's intercepted commands are consumed by the composer and never
  // sent. There is nothing to replace a message WITH: the app would run
  // the command and the conversation would just lose its tail — and for a
  // name the provider also ships (`/model` on Claude) the resend would
  // additionally execute the CLI's own version of it.

  async function submitEdited(thread: Thread, text: string) {
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, text);
    // The toast store is global and never reset between tests, so the
    // "no toast" assertion below has to be a delta.
    const toastsBefore = errorToastCount();
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await tick();
    return { view, resend, editor, toastsBefore };
  }

  it('refuses a leading intercepted command inline, and clears on the next edit', async () => {
    const { view, resend, editor, toastsBefore } = await submitEdited(seedThread(), '/model opus');

    expect(resend).not.toHaveBeenCalled();
    const error = view.getByTestId('user-message-edit-error');
    expect(error).toHaveTextContent('/model');
    // Inline, next to the control that refused — not a toast.
    expect(errorToastCount()).toBe(toastsBefore);
    expect(view.getByTestId('user-message-editor')).toBeInTheDocument();

    await typeInEditor(editor, 'Update TWO of the lines');
    expect(view.queryByTestId('user-message-edit-error')).toBeNull();
  });

  it('refuses a provider-scoped intercepted command only on that provider', async () => {
    const codex = await submitEdited(seedThread({ provider: 'codex' }), '/review the diff');
    expect(codex.resend).not.toHaveBeenCalled();
    expect(codex.view.getByTestId('user-message-edit-error')).toHaveTextContent('/review');
    codex.view.unmount();

    // `/review` is Codex's; on Claude it is ordinary text and goes.
    const claude = await submitEdited(seedThread(), '/review the diff');
    await waitFor(() => expect(claude.resend).toHaveBeenCalledTimes(1));
    expect(claude.view.queryByTestId('user-message-edit-error')).toBeNull();
  });

  it('sends an AO command word — the backend expands those on the wire', async () => {
    const { view, resend } = await submitEdited(seedThread(), '/workflow start');
    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));
    expect(view.queryByTestId('user-message-edit-error')).toBeNull();
  });

  it('sends an intercepted name that is not at position 0', async () => {
    const { view, resend } = await submitEdited(seedThread(), 'see /model docs');
    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));
    expect(view.queryByTestId('user-message-edit-error')).toBeNull();
  });
});

describe('edit-and-resend flow — landing at the new tail', () => {
  // On success the reader's parked height measured rows the revert just
  // destroyed, so the flow lands them at the thread's new tail with
  // bottom-follow engaged — the one deliberate divergence from a normal
  // send, which never yanks a scrolled-up reader. On any non-success
  // exit, and whenever the pane no longer shows the saga's thread, the
  // scroll is not touched.

  /** Replace MessageTimeline's adapter with a spy the flow can reach. */
  function attachSpyController(pane: ReturnType<typeof createThreadPane>) {
    const stickToLatest = vi.fn();
    pane.attachScrollController(
      stubScrollController({
        preserveScrollAnchor: async (_anchor, action) => {
          await action();
        },
        stickToLatest,
      }),
    );
    return stickToLatest;
  }

  it('sticks to the tail once after a successful saga', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Edited text');
    const stickToLatest = attachSpyController(pane);

    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    expect(stickToLatest).toHaveBeenCalledTimes(1);
  });

  it('leaves the scroll alone when the saga is refused', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    setBindingMock('RevertConversationAndResendMessage', async () => {
      throw new Error('a newer turn started');
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Edited text');
    const stickToLatest = attachSpyController(pane);

    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    // Not committed (no marker): the flow hands the editor back.
    await waitFor(() => expect(view.getByTestId('user-message-editor')).toBeInTheDocument());
    expect(stickToLatest).not.toHaveBeenCalled();
  });

  it('leaves the scroll alone when the pane switched threads mid-RPC', async () => {
    const thread = seedThread();
    const other = seedThread({ id: 'thread-2', title: 'Other' });
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const rpc = deferred<void>();
    const resend = setBindingMock('RevertConversationAndResendMessage', () => rpc.promise);

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Edited text');
    const stickToLatest = attachSpyController(pane);

    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    rpc.resolve();
    await tick();
    await tick();
    // The saga succeeded, but this pane is looking at another thread now.
    expect(stickToLatest).not.toHaveBeenCalled();
  });
});
