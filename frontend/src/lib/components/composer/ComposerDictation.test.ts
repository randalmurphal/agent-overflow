// The composer's dictation lifecycle against a scripted microphone: the mic
// shows only where this connection may dictate, and losing that while
// recording abandons the recording instead of leaving the microphone open
// behind a control that is gone.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import Composer from './Composer.svelte';
import { createComposerDraftStore, resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { mockAttachmentDownload } from '../../../test/mocks/attachmentTransfer';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { idleWorkspaceActivity } from '../../../test/helpers/workspaceLock';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { DictationEnvironment } from '../../dictation/dictationSession.svelte';

const scripted = vi.hoisted(() => ({ env: null as DictationEnvironment | null }));
vi.mock('../../dictation/browserDictation', () => ({
  get browserDictationEnvironment() {
    return scripted.env;
  },
}));

const { dictationFixture } = await import('../../../test/helpers/dictation');

function installComposerMocks(): void {
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId, content: '', attachmentIds: [], terminalChips: [], updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('DeleteEmptyDraftThread', async () => false);
  setBindingMock('ListAttachments', async () => []);
  mockAttachmentDownload();
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  setBindingMock('ListThreadMcpServers', async () => []);
  setBindingMock('ListWorkspaceMcpServers', async () => []);
  setBindingMock('ListThreadProposedPlans', async () => []);
  setBindingMock('ListProposedPlanComments', async () => []);
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: false, commands: [] }));
  setBindingMock('GetCodexSkills', async () => ({ cwd: '/tmp/workspace', skills: [], errors: [] }));
  setBindingMock('GetClaudeSkills', async () => []);
}

async function mountComposer() {
  const pane = await buildPane(makeThread({ provider: 'codex' }));
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread(pane.threadId);
  return render(Composer, { props: { pane, draft } });
}

describe('<Composer> dictation', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetComposerDraftSnapshotsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetToLocalPage();
    installComposerMocks();
  });
  afterEach(() => {
    resetToLocalPage();
    setCompactLayoutForTest(false);
    scripted.env = null;
  });

  it('keeps the mic in the toolbar on the desktop and moves it into the input row on the phone', async () => {
    scripted.env = dictationFixture().env;
    setBindingMock('DictationSources', async () => ({ subscription: true }));
    const { findByTestId, getByLabelText, queryAllByTestId } = await mountComposer();
    const inputRow = () => getByLabelText('Message Input').closest('.flex.items-end');

    const desktopMic = await findByTestId('composer-dictate');
    expect(desktopMic.closest('[data-composer-toolbar]')).not.toBeNull();
    expect(inputRow()?.contains(desktopMic)).toBe(false);

    setCompactLayoutForTest(true);
    await tick();
    const phoneMic = await findByTestId('composer-dictate');
    expect(queryAllByTestId('composer-dictate')).toHaveLength(1);
    expect(phoneMic.closest('[data-composer-toolbar]')).toBeNull();
    expect(phoneMic.parentElement).toBe(inputRow());

    setCompactLayoutForTest(false);
    await tick();
    const backMic = await findByTestId('composer-dictate');
    expect(queryAllByTestId('composer-dictate')).toHaveLength(1);
    expect(backMic.closest('[data-composer-toolbar]')).not.toBeNull();
  });

  it('abandons a recording when the connection loses permission to dictate', async () => {
    const f = dictationFixture();
    scripted.env = f.env;
    setBindingMock('DictationSources', async () => ({ subscription: true }));
    setBindingMock('BeginDictation', async () => 'rec-1');
    const cancel = setBindingMock('CancelDictation', async () => {});

    const { findByTestId, queryByTestId } = await mountComposer();
    const mic = await findByTestId('composer-dictate');
    await fireEvent.click(mic);
    await expect.poll(() => mic.dataset.phase).toBe('recording');

    await pairViewOnly();

    await expect.poll(() => queryByTestId('composer-dictate')).toBeNull();
    expect(f.tracks[0].stop).toHaveBeenCalled();
    expect(cancel).toHaveBeenCalledWith('rec-1');
  });

  it('shows no mic where the engine cannot record', async () => {
    scripted.env = dictationFixture({ format: null }).env;
    const sources = setBindingMock('DictationSources', async () => ({ subscription: true }));
    const { queryByTestId, findByTestId } = await mountComposer();
    await findByTestId('composer-send');
    await expect.poll(() => sources.mock.calls.length).toBeGreaterThan(0);
    expect(queryByTestId('composer-dictate')).toBeNull();
  });
});
