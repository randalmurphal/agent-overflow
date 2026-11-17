// Controls that write to a thread row wait out an emptied draft's in-flight
// delete, then act on what it leaves: the placeholder when it deleted the
// thread (no RPC naming the deleted id), the kept thread otherwise.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { resetPanesForTest } from './panes.svelte';
import { withEmptyDraftCleanup } from './emptyDraftCleanup';
import {
  applyThreadContextWindow,
  applyThreadFastMode,
  applyThreadModelSelection,
  applyThreadReasoningEffort,
} from './threadModelControls';
import { cycleAgentMode } from '../components/composer/toolbar/agentModeCycle';
import { renameThreadTitle } from './paneTitleRename';
import { moveDraftProject } from './draftProjectMove';
import {
  archiveThreadAction,
  deleteThreadAction,
  deleteThreadsAction,
  forkThreadAction,
  markThreadUnreadAction,
  nextAssistantThreadAction,
  pinThreadAction,
  renameThreadAction,
  setThreadPinGroupAction,
  unpinThreadAction,
  type ThreadActionCtx,
} from '../components/sidebar/threadRowActions';
import { makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Project, Thread } from '../types/models';

const DRAFT = 'emptied-draft';

function paneOnEmptiedDraft(): ThreadPane {
  const pane = createThreadPane({ paneId: 'writer-gate' });
  pane.replaceThread(makeThread({ id: DRAFT, isDraft: true, mode: 'chat', projectPath: '/repo', workspacePath: '/repo' }));
  return pane;
}

/** An in-flight cleanup whose answer the test gives; deletion returns the pane to a placeholder. */
function cleanupInFlight(pane: ThreadPane, deleted: boolean) {
  let answer!: () => void;
  const answered = new Promise<void>((resolve) => { answer = resolve; });
  const settled = withEmptyDraftCleanup(DRAFT, async () => { await answered; return deleted; }, async (removed) => {
    if (removed) pane.dematerializeEmptyDraftThread();
  });
  return { answer, settled };
}

function ctxFor(): ThreadActionCtx {
  return { thread: makeThread({ id: DRAFT }), isActive: true, clearPane: vi.fn(), switchPane: vi.fn(async () => {}), reportError: vi.fn() };
}

const rowRpc = {
  UpdateThreadModelSelection: async () => makeThread({ id: DRAFT }),
  UpdateThreadReasoningEffort: async () => makeThread({ id: DRAFT }),
  UpdateThreadFastMode: async () => makeThread({ id: DRAFT }),
  UpdateThreadContextWindow: async () => makeThread({ id: DRAFT }),
  UpdateThreadMode: async () => makeThread({ id: DRAFT, mode: 'plan' }),
  GetThread: async () => makeThread({ id: DRAFT }),
  RenameThread: async () => {},
  ArchiveThread: async () => {},
  ForkThread: async () => makeThread({ id: 'fork' }),
  StopSession: async () => {},
  DeleteThreads: async () => ({}),
  StartNextAssistantThread: async () => makeThread({ id: 'successor' }),
  MarkThreadUnread: async () => {},
  PinThread: async () => makeThread({ id: DRAFT }),
  UnpinThread: async () => makeThread({ id: DRAFT }),
  SetThreadPinGroup: async () => makeThread({ id: DRAFT }),
  GetDraft: async () => ({ threadId: DRAFT, content: '', attachmentIds: [], terminalChips: [], updatedAt: 0 }),
  MoveDraftToThread: async () => ({}),
};
type RowRpc = keyof typeof rowRpc;

const writers: Array<[string, RowRpc, (pane: ThreadPane) => Promise<unknown>]> = [
  ['model selection', 'UpdateThreadModelSelection', (pane) => applyThreadModelSelection(pane, 'claude', 'claude-opus-4-1')],
  ['reasoning effort', 'UpdateThreadReasoningEffort', (pane) => applyThreadReasoningEffort(pane, 'high')],
  ['fast mode', 'UpdateThreadFastMode', (pane) => applyThreadFastMode(pane, true)],
  ['context window', 'UpdateThreadContextWindow', (pane) => applyThreadContextWindow(pane, 1_000_000)],
  ['agent mode', 'UpdateThreadMode', (pane) => cycleAgentMode(pane)],
  ['title rename', 'RenameThread', () => renameThreadTitle(DRAFT, 'Renamed')],
  ['sidebar rename', 'RenameThread', () => renameThreadAction(ctxFor(), 'Renamed')],
  ['sidebar archive', 'ArchiveThread', () => archiveThreadAction(ctxFor())],
  ['sidebar fork', 'ForkThread', () => forkThreadAction(ctxFor())],
  ['sidebar delete', 'DeleteThreads', () => deleteThreadAction(ctxFor())],
  ['sidebar delete of a selection', 'DeleteThreads', () => deleteThreadsAction([makeThread({ id: DRAFT })], vi.fn())],
  ['next thread, same assistant', 'StartNextAssistantThread', () => nextAssistantThreadAction(ctxFor())],
  ['sidebar mark unread', 'MarkThreadUnread', () => markThreadUnreadAction(ctxFor())],
  ['sidebar pin', 'PinThread', () => pinThreadAction(ctxFor())],
  ['sidebar unpin', 'UnpinThread', () => unpinThreadAction(ctxFor())],
  ['sidebar pin group', 'SetThreadPinGroup', () => setThreadPinGroupAction(ctxFor(), 1)],
  ['project move', 'GetDraft', (pane) => moveDraftProject(pane, { id: 'project-2', name: 'Other', path: '/other' } as Project, async () => true)],
];

beforeEach(() => {
  installThreadPaneTestEnv();
  resetPanesForTest();
  for (const [name, impl] of Object.entries(rowRpc)) setBindingMock(name as RowRpc, impl as never);
  setBindingMock('UpdateNewThreadDefaults', async (input: Partial<Thread>) => ({ ...input }));
});

describe('row writers during an empty-draft delete', () => {
  it.each(writers)('%s waits for the delete and never names the deleted thread', async (_name, rpc, write) => {
    const pane = paneOnEmptiedDraft();
    const call = setBindingMock(rpc, rowRpc[rpc] as never);
    const cleanup = cleanupInFlight(pane, true);
    const written = write(pane);
    await new Promise((resolve) => setTimeout(resolve, 5));
    expect(call).not.toHaveBeenCalled();
    cleanup.answer();
    await cleanup.settled;
    await written;
    expect(call.mock.calls.filter((args) => args.flat().includes(DRAFT))).toEqual([]);
    expect(pane.hasDraftPlaceholder).toBe(true);
  });

  it('applies to the kept thread once the delete is refused', async () => {
    const pane = paneOnEmptiedDraft();
    const call = setBindingMock('UpdateThreadModelSelection', rowRpc.UpdateThreadModelSelection as never);
    const cleanup = cleanupInFlight(pane, false);
    const written = applyThreadModelSelection(pane, 'claude', 'claude-opus-4-1');
    await new Promise((resolve) => setTimeout(resolve, 5));
    expect(call).not.toHaveBeenCalled();
    cleanup.answer();
    expect(await written).toEqual({ ok: true });
    expect(call).toHaveBeenCalledWith(DRAFT, 'claude', 'claude-opus-4-1');
  });

  it('a model picked during the delete lands on the placeholder that replaces the thread', async () => {
    const pane = paneOnEmptiedDraft();
    const cleanup = cleanupInFlight(pane, true);
    const written = applyThreadModelSelection(pane, 'claude', 'claude-opus-4-1');
    cleanup.answer();
    expect(await written).toEqual({ ok: true });
    expect(pane.thread?.model).toBe('claude-opus-4-1');
  });
});
