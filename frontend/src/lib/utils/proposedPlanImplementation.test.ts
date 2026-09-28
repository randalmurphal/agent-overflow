import { beforeEach, expect, it, vi } from 'vitest';
import { implementProposedPlanInNewThread } from './proposedPlanImplementation';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { createThreadPane } from '../stores/thread.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { noteThread, forgetThread } from '../transport/entityIndex';
import { takePinnedBackend } from '../transport/backends';
import { setSelectedBackend } from '../stores/selectedBackend.svelte';

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
  const onChildWorktree = makeThread({ id: 'plan-sibling', projectId: 'project-1', workspacePath: '/repo-wt/child', worktreePath: '/repo-wt/child', branch: 'child' });
  const sibling = await buildPane(onChildWorktree);
  setBindingMock('GetPayloadData', async () => ({ data: '## Plan\nImplement this change.' }));
  setBindingMock('CreateThread', async () => makeThread({
    id: 'plan-child', projectId: 'project-1', workspacePath: '/repo-wt/child', worktreePath: '/repo-wt/child', branch: 'child',
  }));
  setBindingMock('SaveDraft', async () => { throw new Error('draft write refused'); });
  const removed: string[] = [];
  setBindingMock('GitRemoveWorktree', async (threadId: string) => {
    removed.push(threadId);
    return {
      workspace: { workspacePath: '/repo', worktreePath: '', branch: 'main' },
      reattached: [{ ...onChildWorktree, workspacePath: '/repo', worktreePath: '', branch: 'main' }],
    };
  });
  setBindingMock('DeleteThread', async () => {});
  const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
  try {
    expect(await implementProposedPlanInNewThread(source, { threadId: 'plan-source', itemId: 'plan', payloadId: 'payload' })).toBe(false);
    expect(removed).toEqual(['plan-child']);
    expect(sibling.thread?.workspacePath).toBe('/repo');
    expect(sibling.thread?.worktreePath).toBe('');
    expect(sibling.thread?.branch).toBe('main');
  } finally { quiet.mockRestore(); source.clear(); sibling.clear(); }
});
