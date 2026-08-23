// ChatView structural sanity tests. The old responsive-header behavior
// (inline ModelPicker / BranchToolbar / RuntimeModePicker at wide widths,
// CompactHeaderMenu at narrow widths) is gone — those pickers moved to
// the composer toolbar + below-composer bar in Waves 3a/3b. What's left
// is "does ChatView wire the right children?". This file asserts the
// visible contract that's still meaningful after the rewrite.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import ChatView from './ChatView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { focusPane, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import { getThreads, refreshThreads } from '../../stores/threads.svelte';
import {
  getThreadStatus,
  projectTurnStarted,
  projectTurnCompleted,
  projectThreadItem,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';
import {
  applyUserMessageReverted,
  resetResendRevertMarkersForTest,
} from '../../stores/eventsMessageRevert';
import type { Item, Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installPaneMocks, installThreadSwitchMocks, makeItem } from '../../../test/helpers/chat';
import { resetLayoutMetricsForTest } from '../../stores/layoutMetrics.svelte';
import {
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetEditResendExecutionForTest } from './editResendFlow.svelte';
import { SRC_ROOT } from '../../../test/sourceScan';

const appCss = readFileSync(join(SRC_ROOT, 'app.css'), 'utf8');

beforeAll(() => {
  // Svelte transitions used by children call element.animate; happy-dom
  // doesn't implement it. Keep a minimal shim — the chat directory's
  // tests have relied on this for several waves.
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
  resetResendRevertMarkersForTest();
  // The per-thread execute lane is module-level and released on settle;
  // tests that park the destructive RPC forever never settle, so the
  // reset is what stops one leaking into the next test's submit.
  resetEditResendExecutionForTest();
});

function seedThread(): Thread {
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
  };
}

function mockDrafts(contents: Map<string, string> = new Map()): Map<string, string> {
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId,
    content: contents.get(threadId) ?? '',
    attachmentIds: [],
    terminalChips: [],
    sourceProposedPlan: null,
    updatedAt: 0,
  }));
  return contents;
}

async function buildPane(
  thread: Thread = seedThread(),
  items: Item[] = [],
  paneId = 'pane',
): Promise<ReturnType<typeof createThreadPane>> {
  installPaneMocks(items);
  // Shared installPaneMocks defaults SwitchThread to echo the passed-in
  // id with a synthetic thread; ChatView's tests need the exact `thread`
  // object (including caller overrides) back on switch.
  setBindingMock('SwitchThread', async () => thread);
  // Thread-wide aggregate bindings — PlanSidebar / ActivityRail fetch
  // these on mount. Default to empty; tests that need a populated rail
  // override these before rendering.
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => ({ activeTurnThreads: 0, runningBackgroundTasks: 0 }));
  setBindingMock('CountRunningBackgroundTasks', async () => 0);
  // GitActionsControl calls GetGitStatus on mount; return "not a repo"
  // so the control renders nothing — we don't need a branch chip.
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
  // BranchPicker calls GitListBranches on mount.
  setBindingMock('GitListBranches', async () => []);
  mockDrafts(new Map([[thread.id, '']]));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);

  const pane = createThreadPane({ paneId });
  // ChatView's read-mark + attention-clear effects are now gated on
  // getFocusedPaneId() === pane.paneId. Register + focus the test pane
  // so tests that exercise "user is viewing this thread" behavior have
  // the focus precondition satisfied. Tests that need to assert the
  // background-pane behavior (the user is NOT focused on this pane)
  // can override focus after this returns.
  registerPaneForTest(paneId, pane);
  focusPane(paneId);
  await pane.switchThread(thread);
  return pane;
}

describe('<ChatView>', () => {
  it('renders the chat header with title + always-visible controls', async () => {
    const pane = await buildPane();
    const { getByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(getByTestId('chat-header')).toBeInTheDocument();
    expect(getByTestId('chat-header-title')).toBeInTheDocument();
    expect(getByTestId('review-toggle')).toBeInTheDocument();
    expect(queryByTestId('plan-sidebar-toggle')).toBeNull();
  });

  it('renders the in-card workspace strip', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();
    expect(getByTestId('composer-workspace-strip')).toBeInTheDocument();
  });

  it('hides the fork message action for a claude-tui thread', async () => {
    // Same user-message setup as the fork test below, but on a claude-tui
    // thread. Only the provider capability gate keeps the button off — so
    // dropping the gate brings it back and fails this test.
    const thread: Thread = { ...seedThread(), provider: 'claude-tui' };
    const userItem = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'Update one of the lines',
    });
    const pane = await buildPane(thread, [userItem]);
    mockDrafts(new Map([[thread.id, '']]));

    const { queryByLabelText } = render(ChatView, { props: { pane } });

    expect(queryByLabelText('Fork from this message')).toBeNull();
  });

  it('forks from a user-message action through the chat-level handler', async () => {
    const thread = seedThread();
    const userItem = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'Update one of the lines',
    });
    const pane = await buildPane(thread, [userItem]);
    const forked = {
      ...thread,
      id: 'fork-1',
      projectId: 'project-1',
      title: 'Forked thread',
    };
    mockDrafts(new Map([
      [thread.id, ''],
      [forked.id, userItem.summary],
    ]));
    const fork = setBindingMock('ForkThreadFromMessage', async () => forked);
    setBindingMock('SwitchThread', async () => forked);

    const { getByLabelText } = render(ChatView, { props: { pane } });
    await fireEvent.click(getByLabelText('Fork from this message'));

    await waitFor(() => {
      expect(fork).toHaveBeenCalledWith(thread.id, userItem.id);
      expect(pane.thread?.id).toBe('fork-1');
      expect(getByLabelText('Message Input')).toHaveValue(userItem.summary);
    });
  });

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

  // Structural, not `ReturnType<typeof render>`: the testing-library
  // render result's index signature does not survive being named.
  interface EditorQueries {
    getAllByLabelText(text: string): HTMLElement[];
    getByTestId(id: string): HTMLElement;
  }

  async function openMessageEditor(
    view: EditorQueries,
    index = 0,
  ): Promise<HTMLElement> {
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

  it('opens the in-place editor and closes it silently when nothing was changed', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, '']]));
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);

    await fireEvent.click(view.getByTestId('user-message-edit-cancel'));
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    expect(view.queryByText('Discard changes?')).toBeNull();
    expect(resend).not.toHaveBeenCalled();
  });

  it('confirms before discarding an edited message', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, '']]));

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');

    await fireEvent.click(view.getByTestId('user-message-edit-cancel'));
    await view.findByText('Discard changes?');
    expect(view.queryByTestId('user-message-editor')).toBeInTheDocument();

    await fireEvent.click(view.getByText('Discard'));
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
  });

  it('reverts and resends directly when no background work would be killed', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, 'untouched composer work']]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    await waitFor(() => {
      expect(resend).toHaveBeenCalledWith(
        thread.id,
        item.id,
        {
          content: 'Update TWO of the lines',
          attachmentIds: [],
          killRunningBackgroundTasks: false,
          providerCommand: false,
        },
      );
    });
    // Success ends the flow; the truncate + the resent message are the
    // confirmation, so there is no dialog and no toast.
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    expect(view.queryByText('Revert to this message?')).toBeNull();
    // The composer's own draft was never in this flow's way.
    expect(view.getAllByLabelText('Message Input')[0]).toHaveValue('untouched composer work');
  });

  it('gates the resend behind a kill confirmation when background tasks are running', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, '']]));
    setBindingMock('CountRunningBackgroundTasks', async () => 2);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    await view.findByText('Revert to this message?');
    expect(view.getByText(/kills 2 running background tasks/)).toBeInTheDocument();
    expect(resend).not.toHaveBeenCalled();

    await fireEvent.click(within(view.getByRole('dialog')).getByText('Revert & send'));
    await waitFor(() => {
      expect(resend).toHaveBeenCalledWith(
        thread.id,
        item.id,
        {
          content: 'Update one of the lines',
          attachmentIds: [],
          killRunningBackgroundTasks: true,
          providerCommand: false,
        },
      );
    });
  });

  it('declining the kill confirmation returns to the editor with the edit intact', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, '']]));
    setBindingMock('CountRunningBackgroundTasks', async () => 1);
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await view.findByText('Revert to this message?');

    await fireEvent.click(within(view.getByRole('dialog')).getByText('Cancel'));
    await waitFor(() => expect(view.queryByText('Revert to this message?')).toBeNull());
    expect(resend).not.toHaveBeenCalled();
    expect(within(view.getByTestId('user-message-editor')).getByLabelText('Message Input'))
      .toHaveValue('Update TWO of the lines');
  });

  it('returns to the editor when a guard refuses before anything was committed', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, '']]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    setBindingMock('RevertConversationAndResendMessage', async () => {
      throw new Error('revert and resend: thread is busy');
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    // The anchor row survived, so nothing happened: the editor comes back
    // with the user's text rather than dumping it. Both assertions live
    // inside the waitFor: the input holds the same value while the flow
    // is still executing, so the value alone cannot prove the editor was
    // handed back — the re-enabled Send button is the discriminator.
    await waitFor(() => {
      expect(within(view.getByTestId('user-message-editor')).getByLabelText('Message Input'))
        .toHaveValue('Update TWO of the lines');
      expect(view.getByTestId('user-message-edit-send')).not.toBeDisabled();
    });
  });

  it('hands the message to the composer when the revert committed but the resend failed', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, '']]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    setBindingMock('RevertConversationAndResendMessage', async () => {
      // The backend truncated, then the send failed. Mirror the committed
      // revert the way the WIRE delivers it — through the event handler,
      // which truncates the panes AND records the draftPendingResend
      // marker the failure handler consults — and leave the merged
      // crash-copy draft behind for the composer to pick up.
      applyUserMessageReverted({
        threadId: item.threadId,
        userItemId: item.id,
        turnIndex: item.turnIndex,
        keptAnchorTurnItemIds: [],
        draftPendingResend: true,
      });
      mockDrafts(new Map([[thread.id, 'Update TWO of the lines']]));
      throw new Error('revert and resend: resend failed: provider died');
    });

    const view = render(ChatView, { props: { pane } });
    const editor = await openMessageEditor(view);
    await typeInEditor(editor, 'Update TWO of the lines');
    await fireEvent.click(view.getByTestId('user-message-edit-send'));

    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    await waitFor(() => {
      expect(view.getByLabelText('Message Input')).toHaveValue('Update TWO of the lines');
    });
  });

  it('locks every edit button for the whole flow, starting at preflight', async () => {
    const thread = seedThread();
    const userA = userItem('user:1', 1, 'first prompt');
    const userB = userItem('user:2', 2, 'second prompt');
    const pane = await buildPane(thread, [userA, userB]);
    mockDrafts(new Map([[thread.id, '']]));
    // Deferred count keeps the flow parked in 'preflight' so the test can
    // observe the lock across the window where a busy-id-set-at-execute
    // shape would have swallowed the click.
    let resolveCount: (n: number) => void = () => {};
    setBindingMock(
      'CountRunningBackgroundTasks',
      () => new Promise<number>((resolve) => { resolveCount = resolve; }),
    );
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    const buttons = view.getAllByLabelText('Edit message and resend from here');
    expect(buttons).toHaveLength(2);

    // Opening the editor on one row already locks the other.
    await openMessageEditor(view, 0);
    expect(buttons[1]).toBeDisabled();

    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    expect(buttons[1]).toBeDisabled();
    await fireEvent.click(buttons[1]);

    resolveCount(0);
    await waitFor(() => {
      expect(resend).toHaveBeenCalledWith(thread.id, userA.id, {
        content: 'first prompt',
        attachmentIds: [],
        killRunningBackgroundTasks: false,
        providerCommand: false,
      });
    });
    expect(resend).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(buttons[1]).not.toBeDisabled());
  });

  it('voids the edit flow when the anchor row disappears through another path', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, '']]));
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);

    // Un-send, or a concurrent revert reflected from a second pane.
    pane.removeRevertedItems(item.turnIndex, []);
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    expect(resend).not.toHaveBeenCalled();
  });

  it('keeps the flow alive while executing, and suspends the composer send', async () => {
    const thread = seedThread();
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    // A composer that WOULD be sendable, so the suspension is what
    // disables it rather than an empty draft.
    mockDrafts(new Map([[thread.id, 'composer work']]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    let resolveResend: () => void = () => {};
    setBindingMock(
      'RevertConversationAndResendMessage',
      () => new Promise<void>((resolve) => { resolveResend = resolve; }),
    );

    const view = render(ChatView, { props: { pane } });
    await waitFor(() => expect(view.getByTestId('composer-send')).not.toBeDisabled());
    await openMessageEditor(view);
    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    await waitFor(() => expect(view.getByTestId('composer-send')).toBeDisabled());
    // Disabled, NOT flipped to Stop: there is no turn to interrupt and an
    // interrupt would race the backend's own locked revert.
    expect(view.queryByTestId('composer-interrupt')).toBeNull();

    // The truncation lands mid-RPC — that is our own event, not an
    // invalidation, so the flow must survive it and end on the resolve.
    applyUserMessageReverted({
      threadId: item.threadId,
      userItemId: item.id,
      turnIndex: item.turnIndex,
      keptAnchorTurnItemIds: [],
      draftPendingResend: true,
    });
    await tick();
    expect(view.getByTestId('composer-send')).toBeDisabled();

    resolveResend();
    await waitFor(() => expect(view.getByTestId('composer-send')).not.toBeDisabled());
  });

  it('voids the edit flow on a thread switch', async () => {
    const thread = seedThread();
    const other: Thread = { ...seedThread(), id: 'thread-2', title: 'Other thread' };
    const item = userItem('user:1', 1, 'Update one of the lines');
    const pane = await buildPane(thread, [item]);
    mockDrafts(new Map([[thread.id, ''], [other.id, '']]));
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});
    const resend = setBindingMock('RevertConversationAndResendMessage', async () => undefined);

    const view = render(ChatView, { props: { pane } });
    await openMessageEditor(view);

    installThreadSwitchMocks(other, []);
    await pane.switchThread(other);
    await waitFor(() => expect(view.queryByTestId('user-message-editor')).toBeNull());
    expect(resend).not.toHaveBeenCalled();
    // Nothing was uploaded inside this edit, so nothing is deleted.
    expect(deleteAttachment).not.toHaveBeenCalled();
  });

  it('dims exactly the rows the committed revert will destroy', async () => {
    const thread = seedThread();
    const before = userItem('user:0', 0, 'earlier prompt');
    const anchor = userItem('user:1', 1, 'anchor prompt');
    const sameTurnAfter = makeItem({
      id: 'assistant:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 1,
      kind: 'assistant_text',
      role: 'assistant',
      summary: 'an answer',
    });
    const laterTurn = userItem('user:2', 2, 'later prompt');
    const pane = await buildPane(thread, [before, anchor, sameTurnAfter, laterTurn]);
    mockDrafts(new Map([[thread.id, '']]));
    setBindingMock('CountRunningBackgroundTasks', async () => 0);
    setBindingMock('RevertConversationAndResendMessage', () => new Promise<void>(() => {}));

    const view = render(ChatView, { props: { pane } });
    const rows = () => Array.from(
      view.container.querySelectorAll<HTMLElement>('[data-row-index]'),
    ).map((row) => row.classList.contains('chat-row-pending-cut'));

    // Nothing is dimmed while the editor is merely open: nothing is being
    // destroyed yet.
    await openMessageEditor(view, 1);
    expect(rows()).toEqual([false, false, false, false]);

    await fireEvent.click(view.getByTestId('user-message-edit-send'));
    // Strictly after the anchor in DISPLAY order — the same-turn row that
    // PRECEDES it (here, the anchor itself) is not doomed, which is why
    // the comparison is positional rather than per-turn.
    await waitFor(() => expect(rows()).toEqual([false, false, true, true]));
  });

  it('does not render design preview inside ChatView after explicit toggle', async () => {
    setBindingMock('EnsureDesignWorkdir', async () => {});
    setBindingMock('LatestDesignOptionSet', async () => null);
    const pane = await buildPane({ ...seedThread(), mode: 'design' });
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 1 }]);
    const { getByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(queryByTestId('design-split')).toBeNull();
    expect(queryByTestId('design-split-resizer')).toBeNull();

    await fireEvent.click(getByTestId('design-preview-toggle'));

    expect(pane.showDesignPreviewPanel).toBe(true);
    expect(queryByTestId('design-split')).toBeNull();
    pane.setActiveOptionSet({ setId: 'set-1', optionPaths: ['options/set-1/alpha'] });
    await tick();

    expect(queryByTestId('design-options-panel')).toBeNull();
  });

  it('keeps design clarification controls in the chat column', async () => {
    setBindingMock('EnsureDesignWorkdir', async () => {});
    setBindingMock('LatestDesignOptionSet', async () => null);
    const pane = await buildPane({ ...seedThread(), mode: 'design' });
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 1 }]);
    pane.setPendingClarification({
      requestId: 'clarify-1',
      threadId: pane.threadId ?? 'thread-1',
      intro: 'Pick a direction',
      questions: [{
        id: 'direction',
        prompt: 'Which direction should the agent take?',
        choices: [{ id: 'simple', label: 'Simpler' }],
      }],
    });

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    const overlay = getByTestId('composer-overlay');
    const picker = getByTestId('design-clarification-picker');
    expect(overlay).toContainElement(picker);

    await fireEvent.click(getByTestId('design-preview-toggle'));

    expect(pane.showDesignPreviewPanel).toBe(true);
    expect(overlay).toContainElement(picker);
  });

  it('renders TerminalView instead of the chat surface for terminal-mode threads', async () => {
    // Terminal threads swap the WHOLE surface — same one-for-one branch as
    // discussion mode. No chat header, composer, or timeline mounts; no
    // provider session is started.
    setBindingMock('ListTerminals', async () => []);
    setBindingMock('OpenTerminal', async (threadID: string) => ({
      terminalID: 't1',
      threadID,
      summary: {
        terminalID: 't1', threadID, shell: '/bin/bash', cwd: '/tmp',
        rows: 24, cols: 80, pid: 1, startedAt: 0, running: true,
        exitCode: 0, exitReason: '',
      },
    }));
    setBindingMock('GetTerminalReplay', async () => '');
    const pane = await buildPane({ ...seedThread(), mode: 'terminal' });
    const { getByTestId, queryByTestId, container } = render(ChatView, { props: { pane } });
    // TerminalView mounts through a lazy import, so wait for it to land.
    // The timeout is explicit because what this waits on is MODULE RESOLUTION,
    // not a state transition: on a loaded machine (the full suite running
    // beside a Go test run) the dynamic import alone outruns waitFor's 1s
    // default and the assertion fails on wall clock rather than on behaviour.
    await waitFor(
      () => {
        expect(container.querySelector('[data-ui-surface="terminal"]')).not.toBeNull();
      },
      { timeout: 10_000 },
    );
    expect(getByTestId('terminal-pane-close')).toBeInTheDocument();
    // The chat machinery must be absent — proves the branch replaces, not overlays.
    expect(queryByTestId('chat-header')).toBeNull();
    expect(queryByTestId('composer-overlay')).toBeNull();
  });

  it('does not blur or dim the timeline for pending composer prompts', async () => {
    const pane = await buildPane(seedThread());
    pane.addApproval({
      requestId: 'approval-1',
      threadId: pane.threadId ?? 'thread-1',
      kind: 'command',
      toolName: 'Bash',
      title: 'Approve command',
      description: 'Allow bash?',
      input: { command: 'pwd' },
    });

    const { getByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(getByTestId('composer-pending-approval')).toBeInTheDocument();
    expect(queryByTestId('pending-prompt-scrim')).toBeNull();
  });

  it('does not render interaction-mode / runtime-mode / branch pickers in the header', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ChatView, { props: { pane } });
    await tick();
    // These IDs belonged to the old header chrome; they must be gone
    // from ChatView entirely (the mode cycle button is the composer
    // toolbar's concern now, and the branch picker lives below the
    // composer).
    expect(queryByTestId('interaction-mode-badge')).toBeNull();
    expect(queryByTestId('runtime-mode-trigger')).toBeNull();
    expect(queryByTestId('chat-header-compact')).toBeNull();
    expect(queryByTestId('compact-header-menu-trigger')).toBeNull();
  });

  it('renders a minimal placeholder when no thread is selected', async () => {
    const pane = createThreadPane();
    const { queryByTestId, getByText } = render(ChatView, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header')).toBeNull();
    expect(queryByTestId('chat-empty')).not.toBeNull();
    expect(getByText('Select a thread or create a new one to get started.')).toBeInTheDocument();
  });

  it('lets the app shell own the ground behind transparent chat states', async () => {
    const activePane = await buildPane();
    const active = render(ChatView, { props: { pane: activePane } });
    await tick();

    expect(active.getByTestId('chat-timeline-surface')).toHaveClass('bg-transparent');
    active.unmount();

    const emptyPane = createThreadPane();
    const empty = render(ChatView, { props: { pane: emptyPane } });
    await tick();

    expect(empty.getByTestId('chat-empty')).toHaveClass('bg-transparent');
    expect(appCss).toMatch(/\.app-shell\s*\{[^}]*background:\s*var\(--surface-0\)/);
  });

  it('does not reintroduce a global blended app overlay', () => {
    expect(appCss).not.toMatch(/body::before/);
    expect(appCss).not.toMatch(/body::after/);
    expect(appCss).not.toMatch(/mix-blend-mode/);
    expect(appCss).not.toMatch(/repeating-linear-gradient/);
  });

  it('marks the active thread read locally and coalesces persisted writes when completed turns arrive', async () => {
    vi.useFakeTimers();
    try {
      const thread = { ...seedThread(), latestTurnCompletedAt: 1_000 };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane } });
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
      expect(getThreads()[0]?.lastReadAt).toBe(1_000);
      // The pane attention-dot overlay reads lastReadAt from pane.thread;
      // the sidebar reads it from the global threads registry. Keeping
      // both in sync is what stops the pane dot from showing a stale
      // "Completed" green pip after the user is already looking at the
      // thread.
      expect(pane.thread?.lastReadAt).toBe(1_000);

      vi.setSystemTime(1_010);
      pane.replaceThread({ ...pane.thread!, updatedAt: 1_010, latestTurnCompletedAt: 1_010 });
      await tick();

      expect(getThreads()[0]?.lastReadAt).toBe(1_010);
      expect(pane.thread?.lastReadAt).toBe(1_010);
      expect(markRead).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(100);

      expect(markRead).toHaveBeenCalledTimes(2);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
    } finally {
      vi.useRealTimers();
    }
  });

  it('clears interrupted read state locally when the interrupted thread is opened', async () => {
    vi.useFakeTimers();
    try {
      const thread = { ...seedThread(), hasIncompleteTurn: true };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane } });
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
      expect(getThreads()[0]?.lastReadAt).toBe(1_000);
      expect(getThreads()[0]?.hasIncompleteTurn).toBe(false);
      expect(pane.thread?.hasIncompleteTurn).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('clamps the local read marker to the latest completed turn', async () => {
    vi.useFakeTimers();
    try {
      const latestTurnCompletedAt = 2_000;
      const thread = { ...seedThread(), latestTurnCompletedAt };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane } });
      await tick();

      expect(getThreads()[0]?.lastReadAt).toBe(latestTurnCompletedAt);
    } finally {
      vi.useRealTimers();
    }
  });

  it('leaves an unfocused pane unread when a turn completes on its thread', async () => {
    vi.useFakeTimers();
    try {
      // Two panes: 'main' is focused (default after resetPanesForTest),
      // 'background' is mounted but not focused. A turn completes on
      // the background pane's thread; the attention dot must stay
      // because the user hasn't actually seen it yet.
      const focusedThread = { ...seedThread(), id: 'thread-focused', latestTurnCompletedAt: 500 };
      const backgroundThread = { ...seedThread(), id: 'thread-background', latestTurnCompletedAt: 1_000 };
      setBindingMock('ListThreads', async () => [focusedThread, backgroundThread]);
      await refreshThreads();

      // Register a 'main' pane so we can actually focus it; focusPane
      // is a no-op if the target isn't in the registry.
      const mainPane = createThreadPane({ paneId: 'main' });
      registerPaneForTest('main', mainPane);
      const backgroundPane = await buildPane(backgroundThread, [], 'background');
      // buildPane focuses the pane it just built; flip focus back to
      // 'main' so the background pane is genuinely unfocused. Set the
      // markRead spy AFTER buildPane because buildPane installs its own
      // no-op MarkThreadRead mock.
      focusPane('main');
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane: backgroundPane } });
      await tick();

      // No auto read-mark fires because the pane isn't focused.
      expect(markRead).not.toHaveBeenCalled();
      expect(backgroundPane.thread?.lastReadAt).toBeUndefined();
      expect(getThreads().find((t) => t.id === 'thread-background')?.lastReadAt).toBeUndefined();

      // A new turn completes on the background pane's thread.
      vi.setSystemTime(2_000);
      backgroundPane.replaceThread({
        ...backgroundPane.thread!,
        latestTurnCompletedAt: 2_000,
        updatedAt: 2_000,
      });
      await tick();

      // Still no read-mark — the user hasn't focused the pane yet.
      expect(markRead).not.toHaveBeenCalled();
      expect(backgroundPane.thread?.lastReadAt).toBeUndefined();

      // User focuses the background pane. The read-mark fires now.
      focusPane('background');
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-background');
      expect(backgroundPane.thread?.lastReadAt).toBe(2_000);
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not rewrite read state when the active thread is already read', async () => {
    const thread = { ...seedThread(), latestTurnCompletedAt: 1_000, lastReadAt: 1_500 };
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);
    const markRead = setBindingMock('MarkThreadRead', async () => {});

    render(ChatView, { props: { pane } });
    await tick();

    expect(getThreads()[0]?.lastReadAt).toBe(1_500);
    expect(markRead).not.toHaveBeenCalled();
  });

  it('re-marks read when a stale thread replace reverts the read marker', async () => {
    vi.useFakeTimers();
    try {
      const thread = { ...seedThread(), latestTurnCompletedAt: 1_000, lastReadAt: 1_500 };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(2_000);
      render(ChatView, { props: { pane } });
      await tick();
      expect(markRead).not.toHaveBeenCalled();

      // A wholesale replace from a stale backend snapshot drags
      // lastReadAt behind the completion again. The completion state is
      // unchanged, so a dedup marker keyed only on completions would
      // skip this revert forever and the "Completed" pill would stick
      // on the focused pane.
      pane.replaceThread({ ...pane.thread!, lastReadAt: 500 });
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
      expect(pane.thread?.lastReadAt).toBe(2_000);
      expect(getThreads()[0]?.lastReadAt).toBe(2_000);
    } finally {
      vi.useRealTimers();
    }
  });

  it('marks read from the settled turn when the thread row completion is stale', async () => {
    vi.useFakeTimers();
    try {
      const thread = { ...seedThread(), latestTurnCompletedAt: 1_000, lastReadAt: 1_500 };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(2_500);
      render(ChatView, { props: { pane } });
      await tick();
      expect(markRead).not.toHaveBeenCalled();

      // Transport-gap recovery: the final turn_completed fell into the
      // gap, so pane.thread.latestTurnCompletedAt is stale while
      // refreshFromBackend rehydrated the settled turn with the real
      // completion. The read target must follow the newest completion
      // knowledge, not prefer the defined-but-stale thread row value.
      pane.settleTurn({
        turnId: 'turn-missed',
        turnIndex: 3,
        startedAt: 1_900,
        completedAt: 2_000,
        stopReason: 'end_turn',
        assistantMessageId: null,
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
      expect(pane.thread?.lastReadAt).toBe(2_500);
      expect(getThreads()[0]?.lastReadAt).toBe(2_500);
    } finally {
      vi.useRealTimers();
    }
  });

  it('clears a stale sidebar error status once the thread is open', async () => {
    const thread = seedThread();
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);
    projectThreadItem(makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      status: 'completed',
    }));

    render(ChatView, { props: { pane } });
    await tick();

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('clears a stale sidebar interrupted status once the thread is open', async () => {
    const thread = seedThread();
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);
    projectTurnStarted('thread-1', 'turn-1', 0, 0);
    projectTurnCompleted('thread-1', 'turn-1', { aborted: true });

    render(ChatView, { props: { pane } });
    await tick();

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('flushes a pending composer draft when the chat view unmounts', async () => {
    const pane = await buildPane();
    const saveDraft = setBindingMock('SaveDraft', async () => {});
    const { getByLabelText, unmount } = render(ChatView, { props: { pane } });
    await tick();

    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'pending draft' } });

    unmount();

    await waitFor(() => {
      expect(saveDraft).toHaveBeenCalledWith('thread-1', 'pending draft', [], [], null);
    });
  });

  it('clicking a background tray row does NOT scroll the timeline (rows are informational)', async () => {
    // Phase 5 of the background-tasks plan removed click-to-scroll on
    // tray rows: they are now purely informational, with per-row Stop
    // buttons and a header-level Stop-all button as the only
    // affordances. A plain click on the row body must NOT publish a
    // scroll request on the pane or invoke scrollIntoView on the
    // timeline item.
    const scrollSpy = vi.fn();
    const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;
    HTMLElement.prototype.scrollIntoView = scrollSpy as typeof HTMLElement.prototype.scrollIntoView;
    try {
      const launch: Item = {
        id: 'launch-a',
        threadId: 'thread-1',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        summary: 'Bash: sleep 30',
        isBackground: true,
        toolName: 'Bash',
        createdAt: Date.now() - 1_000,
        updatedAt: Date.now() - 1_000,
      };
      const pane = await buildPane();
      setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
      pane.upsertItem(launch);

      const { getByTestId } = render(ChatView, { props: { pane } });
      await tick();
      await tick();

      // Background section defaults to collapsed in production — expand
      // it via the rail toggle before reaching for the row.
      await fireEvent.click(getByTestId('activity-rail-background-toggle'));
      await tick();

      const row = getByTestId('background-task-tray-row');
      expect(row.getAttribute('data-row-id')).toBe('launch-a');
      expect(row.tagName).not.toBe('BUTTON');

      await fireEvent.click(row);
      await tick();
      await tick();

      expect(scrollSpy).not.toHaveBeenCalled();
    } finally {
      HTMLElement.prototype.scrollIntoView = originalScrollIntoView;
    }
  });
});
