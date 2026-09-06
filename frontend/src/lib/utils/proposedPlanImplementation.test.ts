import { beforeEach, expect, it, vi } from 'vitest';
import { implementProposedPlanInNewThread } from './proposedPlanImplementation';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { makeThread } from '../../test/helpers/chat';
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
