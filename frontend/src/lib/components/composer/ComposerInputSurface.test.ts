// The editing core on its own, exercised through the props the composer
// never varies. `Composer.test.ts` is the integration proof for the default
// configuration; this file covers the opt-outs a second host (the in-place
// message editor) will use, so none of them is first exercised in
// production.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ComposerInputSurface from './ComposerInputSurface.svelte';
import type { ComposerInputSurfaceHandle } from './composerInputSurface';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
  type ComposerDraftStore,
} from '../../stores/composerDraft.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import type { Attachment } from '../../types/attachment';
import type { ThreadPane } from '../../stores/thread.svelte';

function makeAttachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 128,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
    kind: 'image',
  };
}

function installMocks() {
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
  setBindingMock('GetAttachmentData', async () => 'iVBORw0KGgo=');
  setBindingMock('DeleteAttachment', async () => {});
  setBindingMock('UploadAttachment', async (_threadId: string, filename: string) => ({
    ...makeAttachment('att-uploaded'),
    filename,
  }));
  setBindingMock('SearchWorkspaceFiles', async () => ({
    files: [],
    truncated: false,
    root: '/tmp/workspace',
  }));
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: false, commands: [] }));
  setBindingMock('GetCodexSkills', async () => ({ cwd: '/tmp/workspace', skills: [], errors: [] }));
  setBindingMock('GetClaudeSkills', async () => []);
}

interface MountOptions {
  draft?: ComposerDraftStore;
  value?: string;
  disabled?: boolean;
  showDraftRows?: boolean;
  editsDraft?: boolean;
  blockAttachment?: (event: DragEvent | ClipboardEvent, notify: boolean) => boolean;
  shouldDeleteAttachmentRecord?: (id: string) => boolean;
  onKeydown?: (event: KeyboardEvent) => boolean;
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

function makeDragEvent(type: string): DragEvent {
  const event = new Event(type, { bubbles: true, cancelable: true }) as DragEvent;
  Object.defineProperty(event, 'dataTransfer', {
    value: { types: ['Files'], files: [] },
  });
  return event;
}

async function mountSurface(options: MountOptions = {}) {
  const pane: ThreadPane = await buildPane();
  const draft = options.draft ?? await (async () => {
    const store = createComposerDraftStore({ debounceMs: 0 });
    await store.setThread('thread-1');
    return store;
  })();

  // The host owns the textarea's value. This mirrors Composer's routing:
  // the draft when the surface edits it, a host-local answer when it does
  // not.
  const editsDraft = options.editsDraft ?? true;
  let hostValue = options.value ?? draft.content;
  const oninput = vi.fn((value: string, info: { appliedToDraft: boolean }) => {
    if (!editsDraft) {
      hostValue = value;
      return;
    }
    if (!info.appliedToDraft) draft.setContent(value);
  });
  const onSubmitEnter = vi.fn();

  const props = {
    pane,
    draft,
    value: hostValue,
    disabled: options.disabled ?? false,
    placeholder: 'Message',
    oninput,
    onSubmitEnter,
    onKeydown: options.onKeydown,
    showDraftRows: options.showDraftRows,
    editsDraft: options.editsDraft,
    blockAttachment: options.blockAttachment,
    shouldDeleteAttachmentRecord: options.shouldDeleteAttachmentRecord,
  };

  const rendered = render(ComposerInputSurface, { props });
  const textarea = rendered.getByLabelText('Message Input') as HTMLTextAreaElement;
  await tick();

  async function setProps(next: Partial<typeof props>): Promise<void> {
    Object.assign(props, next);
    await rendered.rerender(props);
    await tick();
  }

  /** Re-render with whatever the host now considers the value. */
  async function syncValue(): Promise<void> {
    await setProps({ value: editsDraft ? draft.content : hostValue });
  }

  // fireEvent.input alone leaves selectionStart at 0 in happy-dom, and the
  // completion triggers read the caret — so type like a human does.
  async function typeInto(value: string): Promise<void> {
    await fireEvent.input(textarea, { target: { value } });
    textarea.setSelectionRange(value.length, value.length);
    await fireEvent.select(textarea);
    await tick();
    await syncValue();
  }

  return {
    ...rendered,
    pane,
    draft,
    textarea,
    oninput,
    onSubmitEnter,
    typeInto,
    setProps,
    syncValue,
    handle: rendered.component as unknown as ComposerInputSurfaceHandle,
  };
}

describe('<ComposerInputSurface>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetComposerDraftSnapshotsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    installMocks();
  });

  it('reports the typed value to its host and leaves the draft write to it', async () => {
    const { typeInto, oninput, draft } = await mountSurface();

    await typeInto('hello');

    expect(oninput).toHaveBeenCalledWith('hello', { appliedToDraft: false });
    expect(draft.content).toBe('hello');
  });

  it('applies an image-placeholder deletion itself and tells the host not to redo it', async () => {
    const { typeInto, oninput, draft, syncValue } = await mountSurface();
    draft.setContentAndAttachments('look [Image #1]', [makeAttachment('att-1')]);
    await syncValue();

    // Typing into the placeholder drops the attachment with it — one
    // reconciled write, not a raw value the host would clobber it with.
    await typeInto('look [Image #');

    expect(oninput).toHaveBeenLastCalledWith('look [Image #', { appliedToDraft: true });
    expect(draft.attachments).toHaveLength(0);
  });

  it('leaves placeholders alone when the textarea is not editing the draft', async () => {
    const { typeInto, oninput, draft, syncValue } = await mountSurface({ editsDraft: false });
    draft.setContentAndAttachments('look [Image #1]', [makeAttachment('att-1')]);
    await syncValue();

    await typeInto('a different answer');

    expect(oninput).toHaveBeenLastCalledWith('a different answer', { appliedToDraft: false });
    expect(draft.content).toBe('look [Image #1]');
    expect(draft.attachments).toHaveLength(1);
  });

  // ---- slash commands ----

  it('opens the command menu and paints command words by default', async () => {
    const { typeInto, queryByTestId } = await mountSurface();

    await typeInto('/w');
    await waitFor(() => expect(queryByTestId('slash-popover')).not.toBeNull());

    await typeInto('/workflow ship it');
    await waitFor(() => expect(queryByTestId('composer-command-highlight')).not.toBeNull());
  });

  // ---- keyboard ----

  it('submits on plain Enter and yields the modified forms', async () => {
    const { textarea, typeInto, onSubmitEnter } = await mountSurface();
    await typeInto('ready');

    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true });
    expect(onSubmitEnter).not.toHaveBeenCalled();

    await fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSubmitEnter).toHaveBeenCalledTimes(1);
  });

  it('does not submit on the Enter that picks a command from the menu', async () => {
    const { textarea, typeInto, onSubmitEnter, queryByTestId, draft } = await mountSurface();

    await typeInto('/wo');
    await waitFor(() => expect(queryByTestId('slash-popover')).not.toBeNull());
    await fireEvent.keyDown(textarea, { key: 'Enter' });
    await tick();

    expect(onSubmitEnter).not.toHaveBeenCalled();
    expect(draft.content).toBe('/workflow ');
  });

  it('swallows plain Tab and yields Shift+Tab to the global chord', async () => {
    const { textarea } = await mountSurface();

    const tab = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true });
    textarea.dispatchEvent(tab);
    expect(tab.defaultPrevented).toBe(true);

    const shiftTab = new KeyboardEvent('keydown', {
      key: 'Tab',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    textarea.dispatchEvent(shiftTab);
    expect(shiftTab.defaultPrevented).toBe(false);
  });

  it('lets the host claim a keystroke before either popover sees it', async () => {
    const onKeydown = vi.fn((event: KeyboardEvent) => event.key === 'Enter');
    const { textarea, onSubmitEnter, typeInto, queryByTestId } = await mountSurface({ onKeydown });

    await typeInto('/w');
    await waitFor(() => expect(queryByTestId('slash-popover')).not.toBeNull());
    await fireEvent.keyDown(textarea, { key: 'Enter' });

    expect(onKeydown).toHaveBeenCalled();
    expect(onSubmitEnter).not.toHaveBeenCalled();
    // The menu never saw it either: still open, nothing completed.
    expect(queryByTestId('slash-popover')).not.toBeNull();
  });

  // ---- attachments ----

  it('deletes the backing record when an attachment is removed, by default', async () => {
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});
    const { draft, getByLabelText, syncValue } = await mountSurface();
    draft.setContentAndAttachments('', [makeAttachment('att-1')]);
    await syncValue();

    await fireEvent.click(getByLabelText('Remove att-1.png'));
    await tick();

    expect(draft.attachments).toHaveLength(0);
    await waitFor(() => expect(deleteAttachment).toHaveBeenCalledWith('thread-1', 'att-1'));
  });

  it('shouldDeleteAttachmentRecord=false drops it from the draft but keeps the record', async () => {
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});
    const { draft, getByLabelText, syncValue } = await mountSurface({
      shouldDeleteAttachmentRecord: () => false,
    });
    draft.setContentAndAttachments('', [makeAttachment('att-1')]);
    await syncValue();

    await fireEvent.click(getByLabelText('Remove att-1.png'));
    await tick();

    expect(draft.attachments).toHaveLength(0);
    expect(deleteAttachment).not.toHaveBeenCalled();
  });

  it('uploads a pasted image when the host has no objection', async () => {
    const upload = setBindingMock('UploadAttachment', async () => makeAttachment('att-pasted'));
    const { textarea, draft } = await mountSurface();

    textarea.dispatchEvent(makeClipboardPaste([new File(['x'], 'shot.png', { type: 'image/png' })]));

    await waitFor(() => expect(upload).toHaveBeenCalled());
    await waitFor(() => expect(draft.attachments.map((a) => a.id)).toEqual(['att-pasted']));
  });

  it('refuses every attachment path the host blocks', async () => {
    const upload = setBindingMock('UploadAttachment', async () => makeAttachment('att-pasted'));
    const blockAttachment = vi.fn(() => true);
    const { textarea, handle, queryByTestId } = await mountSurface({ blockAttachment });

    textarea.dispatchEvent(makeClipboardPaste([new File(['x'], 'shot.png', { type: 'image/png' })]));
    await tick();
    expect(upload).not.toHaveBeenCalled();

    // The three drag events refuse too — including the passive ones, so no
    // drop hint appears for a drag that could never land.
    handle.handleDragEnter(makeDragEvent('dragenter'));
    handle.handleDragOver(makeDragEvent('dragover'));
    handle.handleDrop(makeDragEvent('drop'));
    await tick();

    expect(queryByTestId('composer-attachment-row')).toBeNull();
    expect(blockAttachment.mock.calls.map((call) => (call as unknown as [unknown, boolean])[1]))
      .toEqual([true, false, false, true]);
  });

  // ---- rows ----

  it('showDraftRows=false hides the staged rows and keeps chip expansion across the gap', async () => {
    const { draft, container, queryByTestId, getByRole, setProps, syncValue } = await mountSurface();
    draft.addTerminalChip({
      id: 'chip-1',
      label: 'sh',
      preview: '$ ls',
      content: '$ ls\nREADME',
      createdAt: 0,
    });
    draft.setContentAndAttachments('', [makeAttachment('att-1')]);
    await syncValue();

    expect(queryByTestId('terminal-chip-row')).not.toBeNull();
    expect(queryByTestId('composer-attachment-row')).not.toBeNull();

    // Expand the chip, then let a prompt take the card over and hand it back.
    await fireEvent.click(getByRole('button', { name: /ls/ }));
    await tick();
    expect(container.querySelector('#chip-body-chip-1')).not.toBeNull();

    await setProps({ showDraftRows: false });
    expect(queryByTestId('terminal-chip-row')).toBeNull();
    expect(queryByTestId('composer-attachment-row')).toBeNull();

    await setProps({ showDraftRows: true });
    expect(container.querySelector('#chip-body-chip-1')).not.toBeNull();
  });

  // ---- handle ----

  it('exposes focus, height and upload state to its host', async () => {
    const { handle, textarea, draft, syncValue } = await mountSurface();
    draft.setContent('resumed text');
    await syncValue();

    expect(handle.inputMounted()).toBe(true);
    expect(handle.uploading()).toBe(false);

    handle.focusInputAtEnd();
    expect(document.activeElement).toBe(textarea);
    expect(textarea.selectionStart).toBe('resumed text'.length);

    textarea.style.height = '120px';
    handle.resetInputHeight();
    expect(textarea.style.height).toBe('auto');
  });

  it('reads and restores the caret without taking focus', async () => {
    // The in-place editor carries the caret across a virtualizer remount.
    // Restoring it must NOT focus: the reader may have moved on, and a
    // remounting row has no business pulling focus back to itself.
    const { handle, textarea, draft, syncValue } = await mountSurface();
    draft.setContent('rewritten prompt');
    await syncValue();

    textarea.setSelectionRange(3, 7);
    expect(handle.inputSelection()).toEqual({ start: 3, end: 7 });

    textarea.blur();
    textarea.setSelectionRange(0, 0);
    handle.restoreInputSelection({ start: 3, end: 7 });

    expect(textarea.selectionStart).toBe(3);
    expect(textarea.selectionEnd).toBe(7);
    expect(document.activeElement).not.toBe(textarea);
  });

  // recreateInput schedules its swap into an idle slot (setTimeout fallback
  // where requestIdleCallback is missing, as in jsdom); this waits for it.
  const flushIdleSwap = () => new Promise((resolve) => setTimeout(resolve, 1));

  it('recreateInput swaps the textarea element and restores focus only if it was held', async () => {
    // The swap is the release mechanism for Blink's per-character edit-command
    // retention (one command per typed character, kept for the ELEMENT's
    // lifetime). Element identity changing is the whole point — assert it —
    // and focus policy is the visible half: restored when the composer held
    // it, left alone when the send came from a button.
    const { handle, getByLabelText } = await mountSurface();
    const first = getByLabelText('Message Input') as HTMLTextAreaElement;

    first.focus();
    expect(document.activeElement).toBe(first);
    handle.recreateInput();
    // Deferred: the frame the send renders still shows the OLD element,
    // focused — the swap must not land inside the send task.
    expect(getByLabelText('Message Input')).toBe(first);
    expect(document.activeElement).toBe(first);
    await flushIdleSwap();
    const second = getByLabelText('Message Input') as HTMLTextAreaElement;
    expect(second).not.toBe(first);
    expect(first.isConnected).toBe(false);
    expect(document.activeElement).toBe(second);

    second.blur();
    handle.recreateInput();
    await flushIdleSwap();
    const third = getByLabelText('Message Input') as HTMLTextAreaElement;
    expect(third).not.toBe(second);
    expect(document.activeElement).not.toBe(third);
  });

  it('recreateInput skips the swap mid-IME-composition', async () => {
    const { handle, getByLabelText } = await mountSurface();
    const first = getByLabelText('Message Input') as HTMLTextAreaElement;
    await fireEvent.compositionStart(first);
    handle.recreateInput();
    await flushIdleSwap();
    expect(getByLabelText('Message Input')).toBe(first);
    await fireEvent.compositionEnd(first);
    handle.recreateInput();
    await flushIdleSwap();
    expect(getByLabelText('Message Input')).not.toBe(first);
  });

  it('recreateInput skips the swap when typing resumed before the idle slot', async () => {
    // A user who starts the next message during the idle window has live
    // text (and possibly a mention popup) anchored to the current element;
    // the release waits for the next send instead of yanking it.
    const { handle, getByLabelText } = await mountSurface();
    const first = getByLabelText('Message Input') as HTMLTextAreaElement;
    handle.recreateInput();
    await fireEvent.input(first, { target: { value: 'resumed typing' } });
    await flushIdleSwap();
    expect(getByLabelText('Message Input')).toBe(first);
  });

  // ---- local draft store ----

  it('drives a persistence: "none" store without any draft RPC', async () => {
    // Let the debounced writes the earlier tests' PERSISTING stores queued
    // land first, so the spies below can assert "never called at all"
    // rather than something weaker.
    await new Promise((resolve) => setTimeout(resolve, 10));
    const save = setBindingMock('SaveDraft', async () => {});
    const clear = setBindingMock('ClearDraft', async () => {});
    const get = setBindingMock('GetDraft', async () => ({
      threadId: 'thread-1',
      content: '',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 0,
    }));

    const local = createComposerDraftStore({ debounceMs: 0, persistence: 'none' });
    local.seedLocalSnapshot('thread-1', {
      content: 'the message being edited',
      attachments: [],
      terminalChips: [],
      sourceProposedPlan: null,
    });

    const { textarea, typeInto } = await mountSurface({ draft: local });
    expect(textarea.value).toBe('the message being edited');

    await typeInto('the message, edited again');
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(local.content).toBe('the message, edited again');
    expect(local.hasPendingSave).toBe(false);
    expect(save).not.toHaveBeenCalled();
    expect(clear).not.toHaveBeenCalled();
    expect(get).not.toHaveBeenCalled();
  });
});
