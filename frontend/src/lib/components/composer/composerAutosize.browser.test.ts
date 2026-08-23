// The composer textarea sizes itself through `field-sizing: content`; the
// JS autosize in ComposerInputSurface is the fallback for an engine without
// it and must stay out of the way here. happy-dom cannot prove either half:
// it reports zero geometry and answers `CSS.supports` with true for every
// query, so the unit suite (Composer.test.ts, "autosizes multiline input")
// pins the fallback and this file pins the native path in real Chromium.
//
// Why it matters: the fallback cost every keystroke two forced layouts
// (height:auto → scrollHeight, then the frame's own), and an inline `height`
// left behind by it would override the CSS sizing for good.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import ComposerInputSurface from './ComposerInputSurface.svelte';
import { createComposerDraftStore, resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';

const MAX_HEIGHT_PX = 200;

function installMocks(): void {
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('SearchWorkspaceFiles', async () => ({ files: [], truncated: false, root: '/tmp/workspace' }));
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: false, commands: [] }));
  setBindingMock('GetCodexSkills', async () => ({ cwd: '/tmp/workspace', skills: [], errors: [] }));
  setBindingMock('GetClaudeSkills', async () => []);
  setBindingMock('SetUIState', async () => {});
  setBindingMock('AppendUIRenderTraceBatch', async () => {});
}

async function mountSurface() {
  const pane = await buildPane();
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread('thread-1');
  const props = {
    pane,
    draft,
    value: '',
    disabled: false,
    placeholder: 'Message',
    oninput: vi.fn(),
    onSubmitEnter: vi.fn(),
  };
  const rendered = render(ComposerInputSurface, { props });
  const textarea = rendered.getByLabelText('Message Input') as HTMLTextAreaElement;
  await tick();
  async function setValue(value: string): Promise<void> {
    await rendered.rerender({ ...props, value });
    await tick();
  }
  return { textarea, setValue };
}

describe('composer textarea autosize (native)', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    resetComposerDraftSnapshotsForTest();
    installMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('grows with its content, clamps at the composer maximum, and never carries an inline height', async () => {
    const { textarea, setValue } = await mountSurface();
    expect(getComputedStyle(textarea).getPropertyValue('field-sizing')).toBe('content');
    const oneLine = textarea.clientHeight;
    expect(oneLine).toBeGreaterThan(0);
    expect(textarea.style.height).toBe('');

    await setValue(Array.from({ length: 6 }, (_, i) => `line ${i + 1}`).join('\n'));
    const sixLines = textarea.clientHeight;
    expect(sixLines).toBeGreaterThan(oneLine);
    expect(sixLines).toBeLessThan(MAX_HEIGHT_PX);
    expect(textarea.style.height).toBe('');

    await setValue(Array.from({ length: 60 }, (_, i) => `line ${i + 1}`).join('\n'));
    expect(textarea.getBoundingClientRect().height).toBe(MAX_HEIGHT_PX);
    expect(textarea.scrollHeight).toBeGreaterThan(textarea.clientHeight);
    expect(textarea.style.height).toBe('');

    await setValue('');
    expect(textarea.clientHeight).toBe(oneLine);
    expect(textarea.style.height).toBe('');
  });
});
