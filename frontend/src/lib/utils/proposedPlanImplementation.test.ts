import { beforeEach, describe, expect, it, vi } from 'vitest';
import { implementProposedPlanInNewThread } from './proposedPlanImplementation';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { createThreadPane } from '../stores/thread.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { noteThread, forgetThread } from '../transport/entityIndex';
import { takePinnedBackend } from '../transport/backends';
import { setSelectedBackend } from '../stores/selectedBackend.svelte';
import { clearWorktreeIntent, setThreadEnvMode } from '../stores/worktreeIntent.svelte';
import { applyThreadUpdated } from '../stores/eventsThreadRows';

beforeEach(installThreadPaneTestEnv);

it('keeps a plan’s new thread on its source computer after focus changes during payload loading', async () => {
  const pane = createThreadPane();
  const thread = makeThread({ id: 'source-plan', projectId: 'remote-project' });
  pane.replaceThread(thread);
  noteThread(thread.id, 'remote-mac');
  let resolvePayload!: (value: { data: string }) => void;
  setBindingMock('GetPayloadData', () => new Promise((resolve) => { resolvePayload = resolve; }));
  let destination: string | null | undefined;
  setBindingMock('CreateThread', async () => {
    destination = takePinnedBackend();
    // The routing regression ends at dispatch; no fixture-created thread
    // should continue into workspace or provider setup.
    throw new Error('test creation refused');
  });
  const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
  try {
    const creating = implementProposedPlanInNewThread(pane, { threadId: thread.id, itemId: 'plan', payloadId: 'payload' });
    setSelectedBackend('other-computer');
    resolvePayload({ data: '## Plan\nImplement this change.' });
    expect(await creating).toBe(false);
    expect(destination).toBe('remote-mac');
  } finally { quiet.mockRestore(); forgetThread(thread.id); pane.clear(); setSelectedBackend(''); }
});

// A failed seed removes the worktree the new thread was given. Any other
// thread on it moved to the project root; the removal answers with those
// rows, and they are applied before anything reads its workspace again.
it('applies the rows moved by the rollback of a failed seed', async () => {
  const source = createThreadPane();
  source.replaceThread(makeThread({ id: 'plan-source', projectId: 'project-1', workspacePath: '/repo', worktreePath: '', branch: 'main' }));
  setThreadEnvMode(source.thread!, 'new-worktree');
  const onChildWorktree = makeThread({ id: 'plan-sibling', projectId: 'project-1', workspacePath: '/repo-wt/child', worktreePath: '/repo-wt/child', branch: 'child' });
  const sibling = await buildPane(onChildWorktree);
  setBindingMock('GetPayloadData', async () => ({ data: '## Plan\nImplement this change.' }));
  setBindingMock('CreateThread', async () => makeThread({
    id: 'plan-child', projectId: 'project-1', workspacePath: '/repo', worktreePath: '', branch: 'main',
  }));
  setBindingMock('AttachThreadWorktree', async () => makeThread({
    id: 'plan-child', projectId: 'project-1', workspacePath: '/repo-wt/child', worktreePath: '/repo-wt/child', branch: 'child',
  }));
  setBindingMock('SaveDraft', async () => { throw new Error('draft write refused'); });
  const removed: string[] = [];
  setBindingMock('RemoveOtherWorktree', async (_ws: unknown, path: string) => {
    removed.push(path);
    return {
      workspace: { workspacePath: '/repo', worktreePath: '', branch: 'main' },
      reattached: [{ ...onChildWorktree, workspacePath: '/repo', worktreePath: '', branch: 'main' }],
    };
  });
  setBindingMock('DeleteThread', async () => {});
  const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
  try {
    expect(await implementProposedPlanInNewThread(source, { threadId: 'plan-source', itemId: 'plan', payloadId: 'payload' })).toBe(false);
    expect(removed).toEqual(['/repo-wt/child']);
    expect(sibling.thread?.workspacePath).toBe('/repo');
    expect(sibling.thread?.worktreePath).toBe('');
    expect(sibling.thread?.branch).toBe('main');
  } finally { quiet.mockRestore(); clearWorktreeIntent('plan-source'); source.clear(); sibling.clear(); }
});

// A new thread started from a source in a worktree inherits that checkout.
// A failed seed rolls back only a worktree cut for the new thread.
describe('rollback of a failed seed from a source in a worktree', () => {
  function seedFailing(childWorktree: string) {
    const source = createThreadPane();
    source.replaceThread(makeThread({ id: 'plan-wt-source', projectId: 'project-1', workspacePath: '/repo-wt/src', worktreePath: '/repo-wt/src', branch: 'src' }));
    setBindingMock('GetPayloadData', async () => ({ data: '## Plan\nImplement this change.' }));
    setBindingMock('CreateThread', async () => makeThread({
      id: 'plan-wt-child', projectId: 'project-1', workspacePath: '/repo-wt/src', worktreePath: '/repo-wt/src', branch: 'src',
    }));
    setBindingMock('AttachThreadWorktree', async () => makeThread({
      id: 'plan-wt-child', projectId: 'project-1', workspacePath: childWorktree, worktreePath: childWorktree, branch: 'src-child',
    }));
    setBindingMock('SaveDraft', async () => { throw new Error('draft write refused'); });
    const removed: string[] = [];
    const removeOther = setBindingMock('RemoveOtherWorktree', async (_ws: unknown, path: string) => {
      removed.push(path);
      return { workspace: { workspacePath: '/repo', worktreePath: '', branch: 'main' }, reattached: [] };
    });
    // A rollback names the path it cut; resolving the child's current
    // attachment could name the source's worktree.
    const removeAttached = setBindingMock('GitRemoveWorktree', async () => {
      throw new Error('rollback resolved a mutable attachment');
    });
    const deleted: string[] = [];
    setBindingMock('DeleteThread', async (threadId: string) => { deleted.push(threadId); });
    return { source, removed, deleted, removeOther, removeAttached };
  }

  it('leaves the inherited worktree of the source thread alone', async () => {
    const { source, removed, deleted } = seedFailing('/repo-wt/src');
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      expect(await implementProposedPlanInNewThread(source, { threadId: 'plan-wt-source', itemId: 'plan', payloadId: 'payload' })).toBe(false);
      expect(removed).toEqual([]);
      expect(deleted).toEqual(['plan-wt-child']);
    } finally { quiet.mockRestore(); source.clear(); }
  });

  it('removes the worktree materialization cut for the new thread', async () => {
    const { source, removed, deleted } = seedFailing('/repo-wt/child');
    setThreadEnvMode(source.thread!, 'new-worktree');
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      expect(await implementProposedPlanInNewThread(source, { threadId: 'plan-wt-source', itemId: 'plan', payloadId: 'payload' })).toBe(false);
      expect(removed).toEqual(['/repo-wt/child']);
      expect(deleted).toEqual(['plan-wt-child']);
    } finally { quiet.mockRestore(); clearWorktreeIntent('plan-wt-source'); source.clear(); }
  });

  // Another client moves the child back to the source's worktree after the
  // cut answered and before the seed fails. The rollback still removes only
  // the worktree it cut, never the one the child sits on now.
  it('removes the cut worktree when the child was moved back before the rollback', async () => {
    const { source, removed, removeAttached } = seedFailing('/repo-wt/child');
    setThreadEnvMode(source.thread!, 'new-worktree');
    setBindingMock('SaveDraft', async () => {
      applyThreadUpdated({ action: 'full', thread: makeThread({
        id: 'plan-wt-child', projectId: 'project-1', workspacePath: '/repo-wt/src', worktreePath: '/repo-wt/src', branch: 'src',
      }) });
      throw new Error('draft write refused');
    });
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      expect(await implementProposedPlanInNewThread(source, { threadId: 'plan-wt-source', itemId: 'plan', payloadId: 'payload' })).toBe(false);
      expect(removed).toEqual(['/repo-wt/child']);
      expect(removeAttached).not.toHaveBeenCalled();
    } finally { quiet.mockRestore(); clearWorktreeIntent('plan-wt-source'); source.clear(); }
  });

  it('removes the cut worktree on the computer the new thread was created on', async () => {
    const { source, removed } = seedFailing('/repo-wt/child');
    setThreadEnvMode(source.thread!, 'new-worktree');
    noteThread('plan-wt-source', 'remote-mac');
    const targets: Array<string | null> = [];
    setBindingMock('RemoveOtherWorktree', async (_ws: unknown, path: string) => {
      targets.push(takePinnedBackend());
      removed.push(path);
      return { workspace: { workspacePath: '/repo', worktreePath: '', branch: 'main' }, reattached: [] };
    });
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      expect(await implementProposedPlanInNewThread(source, { threadId: 'plan-wt-source', itemId: 'plan', payloadId: 'payload' })).toBe(false);
      expect(removed).toEqual(['/repo-wt/child']);
      expect(targets).toEqual(['remote-mac']);
    } finally { quiet.mockRestore(); forgetThread('plan-wt-source'); clearWorktreeIntent('plan-wt-source'); source.clear(); }
  });
});
