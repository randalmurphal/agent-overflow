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
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import ComposerInputSurface from './ComposerInputSurface.svelte';
import ComposerControlledInputHarness from './ComposerControlledInputHarness.svelte';
import { createComposerDraftStore, resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import type { Attachment } from '../../types/attachment';

const MAX_HEIGHT_PX = 200;

function makeAttachment(threadId = 'thread-1'): Attachment {
  return {
    id: 'pasted-image',
    threadId,
    filename: 'pasted.png',
    mimeType: 'image/png',
    size: 3,
    relativePath: `${threadId}/pasted.png`,
    createdAt: 1,
  };
}

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
  setBindingMock('DeleteAttachment', async () => {});
  setBindingMock('GetAttachmentThumbnail', async () => ({
    data: 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
    mimeType: 'image/png',
  }));
  setBindingMock('SearchWorkspaceFiles', async () => ({ files: [], truncated: false, root: '/tmp/workspace' }));
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: false, commands: [] }));
  setBindingMock('GetCodexSkills', async () => ({ cwd: '/tmp/workspace', skills: [], errors: [] }));
  setBindingMock('GetClaudeSkills', async () => []);
  setBindingMock('SetUIState', async () => {});
  setBindingMock('AppendUIRenderTraceBatch', async () => {});
}

function makeClipboardPaste(files: File[]): ClipboardEvent {
  const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
  Object.defineProperty(event, 'clipboardData', {
    value: {
      items: files.map((file) => ({
        kind: 'file',
        type: file.type,
        getAsFile: () => file,
      })),
    },
  });
  return event;
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

  it('keeps the internal scroll position when a pasted image updates the controlled draft', async () => {
    const content = Array.from({ length: 80 }, (_, index) => `line ${index + 1}`).join('\n');
    setBindingMock('UploadAttachment', async (threadId: string) => makeAttachment(threadId));
    const pane = await buildPane();
    const draft = createComposerDraftStore({ debounceMs: 0 });
    await draft.setThread('thread-1');
    draft.setContent(content);
    const rendered = render(ComposerControlledInputHarness, { props: { pane, draft } });
    const textarea = rendered.getByLabelText('Message Input') as HTMLTextAreaElement;
    await tick();

    const insertion = content.indexOf('line 55');
    textarea.focus();
    textarea.setSelectionRange(insertion, insertion);
    textarea.scrollTop = textarea.scrollHeight * 0.6;
    const before = textarea.scrollTop;
    expect(before).toBeGreaterThan(0);

    textarea.dispatchEvent(makeClipboardPaste([
      new File(['png'], 'pasted.png', { type: 'image/png' }),
    ]));

    await waitFor(() => expect(draft.attachments).toHaveLength(1));
    await waitFor(() => expect(rendered.getByAltText('pasted.png')).toBeInTheDocument());
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    expect(draft.content).toContain('[Image #1]');
    expect(textarea.selectionStart).toBe(insertion + '[Image #1] '.length);
    expect(textarea.selectionEnd).toBe(textarea.selectionStart);
    expect(Math.abs(textarea.scrollTop - before)).toBeLessThanOrEqual(1);
  });

  it('keeps the internal scroll position when removing an image placeholder', async () => {
    const plain = Array.from({ length: 80 }, (_, index) => `line ${index + 1}`).join('\n');
    const insertion = plain.indexOf('line 55');
    const content = `${plain.slice(0, insertion)}[Image #1] ${plain.slice(insertion)}`;
    const pane = await buildPane();
    const draft = createComposerDraftStore({ debounceMs: 0 });
    await draft.setThread('thread-1');
    draft.setContentAndAttachments(content, [makeAttachment()]);
    const rendered = render(ComposerControlledInputHarness, { props: { pane, draft } });
    const textarea = rendered.getByLabelText('Message Input') as HTMLTextAreaElement;
    await waitFor(() => expect(rendered.getByAltText('pasted.png')).toBeInTheDocument());

    textarea.focus();
    textarea.setSelectionRange(insertion + '[Image #1] '.length, insertion + '[Image #1] '.length);
    textarea.scrollTop = textarea.scrollHeight * 0.6;
    const before = textarea.scrollTop;
    expect(before).toBeGreaterThan(0);

    await rendered.getByLabelText('Remove pasted.png').click();

    await waitFor(() => expect(draft.attachments).toHaveLength(0));
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    expect(draft.content).not.toContain('[Image #1]');
    expect(Math.abs(textarea.scrollTop - before)).toBeLessThanOrEqual(1);
  });
});
