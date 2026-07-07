import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import Composer from './Composer.svelte';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
} from '../../stores/composerDraft.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { buildPane, makeItem, makeThread as makeTestThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { Attachment } from '../../types/attachment';
import {
  resetProposedPlanCacheForTests,
  upsertProposedPlanForTests,
} from '../../stores/proposedPlans.svelte';
import {
  getQueueForThread,
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
} from '../../stores/sendQueue.svelte';
import {
  projectSendResolved,
  projectSendStarted,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';
import {
  enterCreateBranchMode,
  resetForTest as resetWorktreeIntent,
  setNewBranchBase,
  setNewBranchName,
  setThreadEnvMode,
  worktreeIntentForThread,
} from '../../stores/worktreeIntent.svelte';
import { getThreadById, removeThread } from '../../stores/threads.svelte';
import type { Project } from '../../types/models';
import * as composerKeyboard from './composerKeyboard';
import {
  notifyTerminalFocus,
  resetTerminalFocusForTest,
} from '../terminal/terminalStore.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';

function installDraftMocks() {
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('DeleteEmptyDraftThread', async () => false);
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('GetAttachmentData', async () => 'iVBORw0KGgo=');
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('ListThreadProposedPlans', async () => []);
  setBindingMock('ListProposedPlanComments', async () => []);
  setBindingMock('SearchWorkspaceFiles', async () => ({
    files: [],
    truncated: false,
    root: '/tmp/workspace',
  }));
}

async function buildDraft(threadId: string | null = 'thread-1') {
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread(threadId);
  return draft;
}

function makeAttachment(id: string, filename = `${id}.png`): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename,
    mimeType: 'image/png',
    size: 128,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
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

function makeFileDrop(files: File[]): DragEvent {
  const event = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
  Object.defineProperty(event, 'dataTransfer', {
    value: {
      files,
      types: ['Files'],
    },
  });
  return event;
}

function makeTextPaste(text: string): ClipboardEvent {
  const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
  Object.defineProperty(event, 'clipboardData', {
    value: {
      items: [{ kind: 'string', type: 'text/plain' }],
      getData: () => text,
    },
  });
  return event;
}

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'project-placeholder',
    path: '/tmp/placeholder',
    name: 'Placeholder Project',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('<Composer>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetComposerDraftSnapshotsForTest();
    resetProposedPlanCacheForTests();
    resetWorktreeIntent();
    resetSendQueueForTest();
    resetThreadStatuses();
    resetTerminalFocusForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    installDraftMocks();
    setBindingMock('SendMessageWithOptions', async () => makeTestThread({ runtimeMode: 'full-access' }));
    setBindingMock('InterruptTurn', async () => {});
    setBindingMock('DeleteAttachment', async () => {});
    setBindingMock('UploadAttachment', async (
      _threadId: string,
      filename: string,
      _mimeType: string,
    ) => makeAttachment(`att-${filename}`, filename));
    // Default RegisterQueueItem mock simulates the backend round-trip:
    // returns a wire item AND seeds the local queue store, mirroring
    // the production flow where the backend stores the item and emits
    // provider:queue_state_changed which seeds the store via the events
    // handler. Tests that need a different backend behaviour (e.g.
    // rejection) can override with their own setBindingMock.
    let defaultQueueSeq = 0;
    setBindingMock('RegisterQueueItem', async (
      threadId: string,
      message: string,
      opts: {
        attachmentIds?: string[];
        sourceProposedPlan?: unknown;
        revisionSourceProposedPlan?: unknown;
        revisionSourceCommentIds?: string[];
      } = {},
    ) => {
      defaultQueueSeq += 1;
      const wire = {
        id: `q-${defaultQueueSeq}`,
        threadId,
        message,
        attachmentIds: opts.attachmentIds ? [...opts.attachmentIds] : [],
        sourceProposedPlan: opts.sourceProposedPlan ?? null,
        revisionSourceProposedPlan: opts.revisionSourceProposedPlan ?? null,
        revisionSourceCommentIds: opts.revisionSourceCommentIds
          ? [...opts.revisionSourceCommentIds]
          : undefined,
        enqueuedAt: defaultQueueSeq,
      };
      const current = getQueueForThread(threadId);
      replaceQueueForThread(threadId, [
        ...current,
        {
          id: wire.id,
          threadId: wire.threadId,
          message: wire.message,
          attachmentIds: wire.attachmentIds,
          sourceProposedPlan: wire.sourceProposedPlan as never,
          revisionSourceProposedPlan: wire.revisionSourceProposedPlan as never,
          revisionSourceCommentIds: wire.revisionSourceCommentIds,
          enqueuedAt: wire.enqueuedAt,
        },
      ]);
      return wire;
    });
  });

  it('disables input when no thread is selected', async () => {
    const pane = createThreadPane();
    const draft = await buildDraft(null);

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });

    expect((getByLabelText('Message Input') as HTMLTextAreaElement).disabled).toBe(true);
    expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(true);
  });

  it('materializes a placeholder and sends through the same draft store', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-send' });
    pane.startDraftPlaceholder(makeProject(), 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-send',
      projectId: 'project-placeholder',
      workspacePath: '/tmp/placeholder',
      projectPath: '/tmp/placeholder',
    });
    const create = setBindingMock('CreateThread', async () => created);
    const save = setBindingMock('SaveDraft', async () => {});
    const clear = setBindingMock('ClearDraft', async () => {});
    const send = setBindingMock('SendMessageWithOptions', async () => created);

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'first send' } });
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => expect(send).toHaveBeenCalledWith('materialized-send', 'first send', {
      attachmentIds: [],
    }));
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      projectId: 'project-placeholder',
      provider: 'codex',
      mode: 'chat',
      workspaceOverride: '/tmp/placeholder',
    }));
    expect(save).not.toHaveBeenCalled();
    await waitFor(() => expect(clear).toHaveBeenCalledWith('materialized-send'));
    expect(draft.threadId).toBe('materialized-send');
    expect(draft.content).toBe('');
  });

  it('uploads pasted images after materializing a placeholder', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-upload' });
    pane.startDraftPlaceholder(makeProject(), 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-upload',
      projectId: 'project-placeholder',
      workspacePath: '/tmp/placeholder',
      projectPath: '/tmp/placeholder',
    });
    setBindingMock('CreateThread', async () => created);
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({
      ...makeAttachment('uploaded', filename),
      threadId,
      mimeType,
    }));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    const file = new File(['png'], 'first.png', { type: 'image/png' });

    await fireEvent(textarea, makeClipboardPaste([file]));

    await waitFor(() => expect(upload).toHaveBeenCalledWith(
      'materialized-upload',
      'first.png',
      'image/png',
      expect.any(String),
    ));
    expect(draft.threadId).toBe('materialized-upload');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['uploaded']);
  });

  it('cleans up a materialized placeholder after rejecting the first pasted image', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-rejected-upload' });
    pane.startDraftPlaceholder(makeProject(), 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-rejected-upload',
      projectId: 'project-placeholder',
      workspacePath: '/tmp/placeholder',
      projectPath: '/tmp/placeholder',
      isDraft: true,
    });
    const create = setBindingMock('CreateThread', async () => created);
    const upload = setBindingMock('UploadAttachment', async () =>
      makeAttachment('should-not-upload'));
    const deleteEmpty = setBindingMock('DeleteEmptyDraftThread', async () => true);

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent(textarea, makeClipboardPaste([
      new File(['bmp'], 'scan.bmp', { type: 'image/bmp' }),
    ]));

    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(upload).not.toHaveBeenCalled();
    await waitFor(() => expect(deleteEmpty).toHaveBeenCalledWith(created.id));
    await waitFor(() => {
      expect(pane.threadId).toBeNull();
      expect(pane.hasDraftPlaceholder).toBe(true);
      expect(draft.threadId).toBeNull();
      expect(draft.attachments).toEqual([]);
    });
  });

  it('uploads dropped images after materializing a placeholder', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-drop-upload' });
    pane.startDraftPlaceholder(makeProject(), 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-drop-upload',
      projectId: 'project-placeholder',
      workspacePath: '/tmp/placeholder',
      projectPath: '/tmp/placeholder',
      isDraft: true,
    });
    setBindingMock('CreateThread', async () => created);
    const save = setBindingMock('SaveDraft', async () => {});
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({
      ...makeAttachment('dropped', filename),
      threadId,
      mimeType,
    }));

    const { getByTestId } = render(Composer, { props: { pane, draft } });

    await fireEvent(getByTestId('composer-root'), makeFileDrop([
      new File(['png'], 'dropped.png', { type: 'image/png' }),
    ]));

    await waitFor(() => expect(upload).toHaveBeenCalledWith(
      created.id,
      'dropped.png',
      'image/png',
      expect.any(String),
    ));
    expect(draft.content).toBe('[Image #1]');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['dropped']);
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(created.id, '[Image #1]', ['dropped'], [], null);
    });
  });

  it('uploads a pasted image on a claude thread (control for the claude-tui gate)', async () => {
    const pane = await buildPane(makeTestThread({ provider: 'claude' }));
    const draft = await buildDraft();
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({ ...makeAttachment('uploaded', filename), threadId, mimeType }));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent(textarea, makeClipboardPaste([
      new File(['png'], 'shot.png', { type: 'image/png' }),
    ]));

    await waitFor(() => expect(upload).toHaveBeenCalledWith(
      'thread-1',
      'shot.png',
      'image/png',
      expect.any(String),
    ));
  });

  it('uploads a pasted image on a claude-tui thread', async () => {
    // claude-tui ingests images by injecting the file path into the real TUI
    // composer, so it accepts composer attachments like any other provider.
    // Same setup as the claude control above; only the provider differs, so this
    // fails if the claude-tui attachment capability regresses to false.
    const pane = await buildPane(makeTestThread({ provider: 'claude-tui' }));
    const draft = await buildDraft();
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({ ...makeAttachment('uploaded', filename), threadId, mimeType }));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent(textarea, makeClipboardPaste([
      new File(['png'], 'shot.png', { type: 'image/png' }),
    ]));

    await waitFor(() => expect(upload).toHaveBeenCalledWith(
      'thread-1',
      'shot.png',
      'image/png',
      expect.any(String),
    ));
  });

  it('lets a plain text paste through on a claude-tui thread', async () => {
    // The attachment gate must only intercept image payloads — a plain text
    // paste has to reach the textarea untouched. Regression for the bug where a
    // capability-gated provider preventDefault'd every paste, text included.
    const pane = await buildPane(makeTestThread({ provider: 'claude-tui' }));
    const draft = await buildDraft();
    const upload = setBindingMock('UploadAttachment', async () =>
      makeAttachment('should-not-upload'));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    const paste = makeTextPaste('just some words');
    await fireEvent(textarea, paste);

    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(upload).not.toHaveBeenCalled();
    expect(draft.attachments).toEqual([]);
    // Not consumed: the textarea performs its native text insertion.
    expect(paste.defaultPrevented).toBe(false);
  });

  it('materializes once and preserves ordered placeholders for multiple first-pasted images', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-multi-upload' });
    pane.startDraftPlaceholder(makeProject(), 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-multi-upload',
      projectId: 'project-placeholder',
      workspacePath: '/tmp/placeholder',
      projectPath: '/tmp/placeholder',
      isDraft: true,
    });
    const create = setBindingMock('CreateThread', async () => created);
    const save = setBindingMock('SaveDraft', async () => {});
    let nextId = 1;
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({
      ...makeAttachment(`att-${nextId++}`, filename),
      threadId,
      mimeType,
    }));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent(textarea, makeClipboardPaste([
      new File(['png-one'], 'one.png', { type: 'image/png' }),
      new File(['png-two'], 'two.png', { type: 'image/png' }),
    ]));

    await waitFor(() => expect(upload).toHaveBeenCalledTimes(2));
    expect(create).toHaveBeenCalledTimes(1);
    expect(upload.mock.calls.map((call) => call[0])).toEqual([created.id, created.id]);
    expect(draft.content).toBe('[Image #1] [Image #2]');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['att-1', 'att-2']);
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(created.id, '[Image #1] [Image #2]', ['att-1', 'att-2'], [], null);
    });
  });

  it('does not delete a just-materialized placeholder while the first pasted image is uploading', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-upload-race' });
    pane.startDraftPlaceholder(makeProject(), 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-upload-race',
      projectId: 'project-placeholder',
      workspacePath: '/tmp/placeholder',
      projectPath: '/tmp/placeholder',
      isDraft: true,
    });
    setBindingMock('CreateThread', async () => created);
    const save = setBindingMock('SaveDraft', async () => {});
    const deleteEmpty = setBindingMock('DeleteEmptyDraftThread', async () => true);
    let releaseUpload: (() => void) | undefined;
    const uploadStarted = new Promise<void>((resolve) => {
      setBindingMock('UploadAttachment', async (
        threadId: string,
        filename: string,
        mimeType: string,
      ) => {
        resolve();
        await new Promise<void>((finish) => {
          releaseUpload = finish;
        });
        return {
          ...makeAttachment('uploaded', filename),
          threadId,
          mimeType,
        };
      });
    });

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent(textarea, makeClipboardPaste([
      new File(['png'], 'first.png', { type: 'image/png' }),
    ]));
    await uploadStarted;
    await tick();
    expect(deleteEmpty).not.toHaveBeenCalled();

    releaseUpload?.();

    await waitFor(() => {
      expect(draft.threadId).toBe(created.id);
      expect(draft.content).toBe('[Image #1]');
      expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['uploaded']);
    });
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(created.id, '[Image #1]', ['uploaded'], [], null);
    });
    expect(deleteEmpty).not.toHaveBeenCalled();
    expect(pane.threadId).toBe(created.id);
  });

  it('does not attach a delayed placeholder upload after the pane leaves that draft', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-upload-switch' });
    const firstProject = makeProject({ id: 'project-upload-switch-first', path: '/tmp/upload-switch-first', name: 'First' });
    const secondProject = makeProject({ id: 'project-upload-switch-second', path: '/tmp/upload-switch-second', name: 'Second' });
    pane.startDraftPlaceholder(firstProject, 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-upload-switch',
      projectId: firstProject.id,
      workspacePath: firstProject.path,
      projectPath: firstProject.path,
      isDraft: true,
    });
    setBindingMock('CreateThread', async () => created);
    let releaseUpload: (() => void) | undefined;
    const uploadStarted = new Promise<void>((resolve) => {
      setBindingMock('UploadAttachment', async (
        threadId: string,
        filename: string,
        mimeType: string,
      ) => {
        resolve();
        await new Promise<void>((finish) => {
          releaseUpload = finish;
        });
        return {
          ...makeAttachment('uploaded-after-switch', filename),
          threadId,
          mimeType,
        };
      });
    });

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent(textarea, makeClipboardPaste([
      new File(['png'], 'first.png', { type: 'image/png' }),
    ]));
    await uploadStarted;

    pane.startDraftPlaceholder(secondProject, 'chat');
    await draft.setThread(null);
    releaseUpload?.();
    await tick();

    expect(pane.draftPlaceholder?.projectId).toBe(secondProject.id);
    expect(draft.threadId).toBeNull();
    expect(draft.content).toBe('');
    expect(draft.attachments).toEqual([]);
  });

  it('uploads pasted images to a replacement thread when empty cleanup is already pending', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-upload-pending-cleanup' });
    const project = makeProject({ id: 'project-upload-pending-cleanup' });
    pane.startDraftPlaceholder(project, 'chat');
    const draft = await buildDraft(null);
    const first = makeTestThread({
      id: 'materialized-upload-pending-cleanup',
      projectId: project.id,
      workspacePath: project.path,
      projectPath: project.path,
      isDraft: true,
    });
    const replacement = makeTestThread({
      id: 'materialized-upload-pending-cleanup-replacement',
      projectId: project.id,
      workspacePath: project.path,
      projectPath: project.path,
      isDraft: true,
    });
    let createCount = 0;
    const create = setBindingMock('CreateThread', async () => {
      createCount += 1;
      return createCount === 1 ? first : replacement;
    });
    const save = setBindingMock('SaveDraft', async () => {});
    const deleteGate = deferred<boolean>();
    const deleteEmpty = setBindingMock('DeleteEmptyDraftThread', async () => deleteGate.promise);
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({
      ...makeAttachment('uploaded', filename),
      threadId,
      mimeType,
    }));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'draft text' } });
    await waitFor(() => expect(draft.threadId).toBe(first.id));
    await fireEvent.input(textarea, { target: { value: '' } });
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(first.id, '', [], [], null);
    });
    await waitFor(() => expect(deleteEmpty).toHaveBeenCalledWith(first.id));

    await fireEvent(textarea, makeClipboardPaste([
      new File(['png'], 'first.png', { type: 'image/png' }),
    ]));

    await waitFor(() => {
      expect(create).toHaveBeenCalledTimes(2);
      expect(upload).toHaveBeenCalledWith(
        replacement.id,
        'first.png',
        'image/png',
        expect.any(String),
      );
    });
    deleteGate.resolve(true);

    await waitFor(() => {
      expect(pane.threadId).toBe(replacement.id);
      expect(draft.threadId).toBe(replacement.id);
      expect(draft.content).toBe('[Image #1]');
      expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['uploaded']);
    });
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(replacement.id, '[Image #1]', ['uploaded'], [], null);
    });
  });

  it('deletes the attachment before cleaning up after the only first-pasted image is removed', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-remove-only-image' });
    pane.startDraftPlaceholder(makeProject(), 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-remove-only-image',
      projectId: 'project-placeholder',
      workspacePath: '/tmp/placeholder',
      projectPath: '/tmp/placeholder',
      isDraft: true,
    });
    setBindingMock('CreateThread', async () => created);
    const save = setBindingMock('SaveDraft', async () => {});
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({
      ...makeAttachment('uploaded', filename),
      threadId,
      mimeType,
    }));
    let releaseDelete!: () => void;
    const deleteStarted = new Promise<void>((resolve) => {
      setBindingMock('DeleteAttachment', async () => {
        resolve();
        await new Promise<void>((finish) => {
          releaseDelete = finish;
        });
      });
    });
    const deleteEmpty = setBindingMock('DeleteEmptyDraftThread', async () => true);

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent(textarea, makeClipboardPaste([
      new File(['png'], 'first.png', { type: 'image/png' }),
    ]));
    await waitFor(() => expect(upload).toHaveBeenCalledWith(
      created.id,
      'first.png',
      'image/png',
      expect.any(String),
    ));

    await fireEvent.click(getByLabelText('Remove first.png'));
    await deleteStarted;
    await tick();
    expect(deleteEmpty).not.toHaveBeenCalled();

    releaseDelete();

    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(created.id, '', [], [], null);
    });
    await waitFor(() => expect(deleteEmpty).toHaveBeenCalledWith(created.id));
    await waitFor(() => {
      expect(pane.threadId).toBeNull();
      expect(pane.hasDraftPlaceholder).toBe(true);
      expect(draft.threadId).toBeNull();
      expect(draft.attachments).toEqual([]);
    });
  });

  it('does not commit stale placeholder materialization after the pane changes drafts', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-race' });
    const firstProject = makeProject({ id: 'project-first', path: '/tmp/first', name: 'First' });
    const secondProject = makeProject({ id: 'project-second', path: '/tmp/second', name: 'Second' });
    pane.startDraftPlaceholder(firstProject, 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-stale',
      projectId: firstProject.id,
      workspacePath: firstProject.path,
      projectPath: firstProject.path,
    });
    const createGate = deferred<typeof created>();
    setBindingMock('CreateThread', () => createGate.promise);

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'start first' } });
    pane.startDraftPlaceholder(secondProject, 'design');
    createGate.resolve(created);

    await tick();
    await tick();

    expect(pane.draftPlaceholder?.projectId).toBe(secondProject.id);
    expect(pane.threadId).toBeNull();
    expect(pane.thread?.id).not.toBe('materialized-stale');
    // The stale create resolution must NOT prepend the materialized
    // row — the pane already swapped to a different placeholder.
    expect(getThreadById('materialized-stale')).toBeUndefined();
  });

  it('deletes an emptied materialized draft and returns the pane to a placeholder', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-empty-cleanup' });
    const project = makeProject({ id: 'project-empty-cleanup' });
    pane.startDraftPlaceholder(project, 'chat');
    const draft = await buildDraft(null);
    const created = makeTestThread({
      id: 'materialized-empty-cleanup',
      projectId: project.id,
      workspacePath: project.path,
      projectPath: project.path,
      isDraft: true,
    });
    removeThread(created.id);
    setBindingMock('CreateThread', async () => created);
    const save = setBindingMock('SaveDraft', async () => {});
    const deleteEmpty = setBindingMock('DeleteEmptyDraftThread', async () => true);

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'draft text' } });
    await waitFor(() => expect(draft.threadId).toBe(created.id));
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(created.id, 'draft text', [], [], null);
    });
    expect(getThreadById(created.id)?.isDraft).toBe(true);

    await fireEvent.input(textarea, { target: { value: '' } });
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(created.id, '', [], [], null);
    });
    await waitFor(() => {
      expect(deleteEmpty).toHaveBeenCalledWith(created.id);
    });
    await waitFor(() => {
      expect(pane.threadId).toBeNull();
      expect(pane.hasDraftPlaceholder).toBe(true);
      expect(draft.threadId).toBeNull();
    });
    expect(pane.thread?.id.startsWith('draft:')).toBe(true);
    expect(getThreadById(created.id)).toBeUndefined();
  });

  it('rematerializes and preserves content when typing resumes during empty cleanup', async () => {
    const pane = createThreadPane({ paneId: 'placeholder-empty-cleanup-race' });
    const project = makeProject({ id: 'project-empty-cleanup-race' });
    pane.startDraftPlaceholder(project, 'chat');
    const draft = await buildDraft(null);
    const first = makeTestThread({
      id: 'materialized-empty-race',
      projectId: project.id,
      workspacePath: project.path,
      projectPath: project.path,
      isDraft: true,
    });
    const replacement = makeTestThread({
      id: 'materialized-empty-race-replacement',
      projectId: project.id,
      workspacePath: project.path,
      projectPath: project.path,
      isDraft: true,
    });
    let createCount = 0;
    const create = setBindingMock('CreateThread', async () => {
      createCount += 1;
      return createCount === 1 ? first : replacement;
    });
    const save = setBindingMock('SaveDraft', async () => {});
    const deleteGate = deferred<boolean>();
    const deleteEmpty = setBindingMock('DeleteEmptyDraftThread', async () => deleteGate.promise);

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'draft text' } });
    await waitFor(() => expect(draft.threadId).toBe(first.id));
    await fireEvent.input(textarea, { target: { value: '' } });
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(first.id, '', [], [], null);
    });
    await waitFor(() => expect(deleteEmpty).toHaveBeenCalledWith(first.id));

    await fireEvent.input(textarea, { target: { value: 'typed again' } });
    deleteGate.resolve(true);

    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(pane.threadId).toBe(replacement.id));
    expect(draft.content).toBe('typed again');
    await waitFor(() => {
      expect(save).toHaveBeenCalledWith(replacement.id, 'typed again', [], [], null);
    });
  });

  it('sends the draft and clears it on success', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'hello world' } });
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'hello world', {
        attachmentIds: [],
      });
    });
    expect(draft.content).toBe('');
  });

  it('sends draft plan comments with a typed refinement by default', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: 'Selected section',
      body: 'Tighten this up.',
      createdAt: 1,
      updatedAt: 1,
    }]);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'Please revise.' } });
    await findByText('Send comments');
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith(
        'thread-1',
        'Please revise.',
        expect.objectContaining({
          attachmentIds: [],
          revisionSourceProposedPlan: expect.objectContaining({
            threadId: 'thread-1',
            itemId: 'plan-1',
            payloadId: 'payload-1',
            title: 'Plan',
          }),
          revisionSourceCommentIds: ['comment-1'],
        }),
      );
    });
  });

  it('sends typed plan comments when Enter is pressed', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: 'Selected section',
      body: 'Tighten this up.',
      createdAt: 1,
      updatedAt: 1,
    }]);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, findByText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input');
    await fireEvent.input(textarea, { target: { value: 'Please revise.' } });
    await findByText('Send comments');
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith(
        'thread-1',
        'Please revise.',
        expect.objectContaining({
          revisionSourceCommentIds: ['comment-1'],
        }),
      );
    });
  });

  it('can send a typed plan refinement without draft comments from the send menu', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: 'Selected section',
      body: 'Tighten this up.',
      createdAt: 1,
      updatedAt: 1,
    }]);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'Please revise.' } });
    await findByText('Send comments');
    await fireEvent.click(getByTestId('composer-send-menu'));
    await fireEvent.click(await findByText('Send without comments'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'Please revise.', expect.objectContaining({
        attachmentIds: [],
        revisionSourceProposedPlan: expect.objectContaining({
          threadId: 'thread-1',
          itemId: 'plan-1',
          payloadId: 'payload-1',
          title: 'Plan',
        }),
      }));
    });
  });

  it('labels typed plan feedback without draft comments as refine', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => []);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'Revise this plan.' } });
    await findByText('Refine');
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith(
        'thread-1',
        'Revise this plan.',
        expect.objectContaining({
          attachmentIds: [],
          revisionSourceProposedPlan: expect.objectContaining({ itemId: 'plan-1' }),
        }),
      );
    });
    const sendOptions = send.mock.calls[0]?.[2] as { revisionSourceCommentIds?: string[] };
    expect(sendOptions.revisionSourceCommentIds).toBeUndefined();
  });

  it('sends draft plan comments without requiring typed text', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: 'Selected section',
      body: 'Tighten this up.',
      createdAt: 1,
      updatedAt: 1,
    }]);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Send comments');
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith(
        'thread-1',
        '',
        expect.objectContaining({
          attachmentIds: [],
          revisionSourceProposedPlan: expect.objectContaining({
            threadId: 'thread-1',
            itemId: 'plan-1',
            payloadId: 'payload-1',
            title: 'Plan',
          }),
          revisionSourceCommentIds: ['comment-1'],
        }),
      );
    });
  });

  it('shows implement for the latest plan even when later assistant text exists', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
      makeItem({
        id: 'assistant-after-plan',
        kind: 'assistant_text',
        payloadKind: undefined,
        payloadId: undefined,
        turnIndex: 0,
        itemIndex: 1,
        summary: 'Plan created and ready.',
        updatedAt: 2,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => []);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send'));

    expect(send).toHaveBeenCalledWith('thread-1', 'Implement the plan.', {
      attachmentIds: [],
      sourceProposedPlan: expect.objectContaining({
        itemId: 'plan-1',
        payloadId: 'payload-1',
      }),
    });
  });

  it('prepares a pending worktree before implementing the current plan', async () => {
    const initialThread = makeTestThread({ branch: 'main' });
    const worktreeThread = makeTestThread({
      branch: 'feature/custom',
      workspacePath: '/tmp/wt-feature',
      worktreePath: '/tmp/wt-feature',
    });
    const pane = await buildPane(initialThread, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchBase(pane.thread, 'release');
    setNewBranchName(pane.thread, 'feature/custom');
    setBindingMock('ListProposedPlanComments', async () => []);
    const prepare = setBindingMock('PrepareThreadWorktree', async () => worktreeThread);
    const send = setBindingMock('SendMessageWithOptions', async () => worktreeThread);

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send'));

    expect(prepare).toHaveBeenCalledWith('thread-1', 'release', 'feature/custom', false);
    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'Implement the plan.', {
        attachmentIds: [],
        sourceProposedPlan: expect.objectContaining({
          itemId: 'plan-1',
          payloadId: 'payload-1',
        }),
      });
    });
    expect(prepare.mock.invocationCallOrder[0]).toBeLessThan(send.mock.invocationCallOrder[0]);
    expect(pane.thread?.worktreePath).toBe('/tmp/wt-feature');
  });

  it('does not reread a cleared plan source after implement send resolves', async () => {
    const plan = makeItem({
      id: 'plan-1',
      kind: 'tool_call',
      payloadKind: 'proposed_plan',
      payloadId: 'payload-1',
      payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
      turnIndex: 0,
      itemIndex: 0,
      updatedAt: 1,
    });
    const pane = await buildPane(makeTestThread(), [plan]);
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', ratio: 1 }]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => []);
    const sendGate = deferred<ReturnType<typeof makeTestThread>>();
    const send = setBindingMock('SendMessageWithOptions', async () => sendGate.promise);

    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByTestId, findByText, queryByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send'));
    await waitFor(() => {
      expect(send).toHaveBeenCalled();
    });

    upsertProposedPlanForTests({
      ...plan,
      meta: JSON.stringify({ planImplementedAt: 123 }),
      updatedAt: 2,
    });
    await tick();
    sendGate.resolve(makeTestThread({ runtimeMode: 'full-access' }));

    await waitFor(() => {
      expect(queryByText('Implement')).toBeNull();
    });
    expect(consoleError).not.toHaveBeenCalled();
    consoleError.mockRestore();
  });

  it('returns to the normal send button after implementing the latest plan', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => []);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByTestId, findByText, queryByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(queryByText('Implement')).toBeNull();
    });
    await waitFor(() => {
      expect(send).toHaveBeenCalled();
    });
    projectSendResolved('thread-1');
    await waitFor(() => {
      expect(queryByText('Working')).toBeNull();
    });
    await waitFor(() => {
      expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(true);
    });
  });

  it('does not fall back to an older plan when the latest plan is implemented', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Old plan', preview: '# Old plan' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
      makeItem({
        id: 'plan-2',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-2',
        payloadMeta: JSON.stringify({ title: 'Implemented plan', preview: '# Implemented plan' }),
        meta: JSON.stringify({ planImplementedAt: 123 }),
        turnIndex: 1,
        itemIndex: 0,
        updatedAt: 2,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => []);

    const { getByTestId, queryByText } = render(Composer, { props: { pane, draft } });

    await waitFor(() => {
      expect(queryByText('Implement')).toBeNull();
    });
    expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(true);
  });

  it('seeds a fresh thread with the plan markdown when "Implement in new thread" is chosen', async () => {
    const pane = await buildPane(makeTestThread({ projectId: 'project-1', workspacePath: '/repo' }), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Ship feature', preview: '# Ship feature' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => []);
    setBindingMock('GetPayloadData', async () => ({
      data: '# Ship feature\n\n- step one\n- step two\n',
      kind: 'proposed_plan',
    }));
    const create = setBindingMock(
      'CreateThread',
      async () => makeTestThread({ id: 'thread-2', projectId: 'project-1', workspacePath: '/repo' }),
    );
    const save = setBindingMock('SaveDraft', async () => {});
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ id: 'thread-2', runtimeMode: 'full-access' }));

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send-menu'));
    await fireEvent.click(await findByText('Implement in new thread'));

    await waitFor(() => {
      expect(create).toHaveBeenCalled();
      expect(save).toHaveBeenCalled();
    });

    // CreateThread is called with the inherited workspace + plan-derived title.
    const createArg = create.mock.calls[0]?.[0];
    expect(createArg).toMatchObject({
      projectId: 'project-1',
      workspaceOverride: '/repo',
      title: 'Implement Ship feature',
      mode: 'chat',
    });

    // SaveDraft seeds the wrapped plan prompt + the source-plan link so a
    // later send marks the original plan Accepted.
    expect(save).toHaveBeenCalledWith(
      'thread-2',
      expect.stringContaining('PLEASE IMPLEMENT THIS PLAN:'),
      [],
      [],
      expect.objectContaining({ threadId: 'thread-1', itemId: 'plan-1', payloadId: 'payload-1' }),
    );

    // No turn is started — the user is expected to send when ready.
    expect(send).not.toHaveBeenCalled();
  });

  it('materializes staged worktree intent on the implementation thread only', async () => {
    const sourceThread = makeTestThread({
      branch: 'main',
      projectId: 'project-1',
      workspacePath: '/repo',
    });
    const pane = await buildPane(sourceThread, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Ship feature', preview: '# Ship feature' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchBase(pane.thread, 'release');
    setNewBranchName(pane.thread, 'feature/custom');
    setBindingMock('ListProposedPlanComments', async () => []);
    setBindingMock('GetPayloadData', async () => ({
      data: '# Ship feature\n\n- step one\n- step two\n',
      kind: 'proposed_plan',
    }));
    const childBeforeWorktree = makeTestThread({
      id: 'thread-2',
      branch: 'main',
      projectId: 'project-1',
      workspacePath: '/repo',
    });
    const childAfterWorktree = makeTestThread({
      id: 'thread-2',
      branch: 'feature/custom',
      projectId: 'project-1',
      workspacePath: '/tmp/wt-feature',
      worktreePath: '/tmp/wt-feature',
    });
    const create = setBindingMock('CreateThread', async () => childBeforeWorktree);
    const prepare = setBindingMock('PrepareThreadWorktree', async () => childAfterWorktree);
    const save = setBindingMock('SaveDraft', async () => {});
    const send = setBindingMock('SendMessageWithOptions', async () => childAfterWorktree);

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send-menu'));
    await fireEvent.click(await findByText('Implement in new thread'));

    await waitFor(() => {
      expect(save).toHaveBeenCalled();
    });
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      projectId: 'project-1',
      workspaceOverride: '/repo',
      branch: 'main',
    }));
    expect(prepare).toHaveBeenCalledWith('thread-2', 'release', 'feature/custom', false);
    expect(create.mock.invocationCallOrder[0]).toBeLessThan(prepare.mock.invocationCallOrder[0]);
    expect(prepare.mock.invocationCallOrder[0]).toBeLessThan(save.mock.invocationCallOrder[0]);
    expect(save).toHaveBeenCalledWith(
      'thread-2',
      expect.stringContaining('PLEASE IMPLEMENT THIS PLAN:'),
      [],
      [],
      expect.objectContaining({ threadId: 'thread-1', itemId: 'plan-1', payloadId: 'payload-1' }),
    );
    expect(send).not.toHaveBeenCalled();
    expect(pane.thread?.id).toBe('thread-2');
    expect(pane.thread?.worktreePath).toBe('/tmp/wt-feature');
    expect(worktreeIntentForThread(sourceThread).mode).toBe('local');
  });

  it('cleans up the orphan thread when seeding the implementation draft fails', async () => {
    const pane = await buildPane(makeTestThread({ projectId: 'project-1', workspacePath: '/repo' }), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => []);
    setBindingMock('GetPayloadData', async () => ({ data: '# Plan\n\n- step', kind: 'proposed_plan' }));
    setBindingMock('CreateThread', async () => makeTestThread({ id: 'thread-2', projectId: 'project-1' }));
    setBindingMock('SaveDraft', async () => {
      throw new Error('disk full');
    });
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send-menu'));
    await fireEvent.click(await findByText('Implement in new thread'));

    // The orphan thread row must be torn down so it doesn't accumulate
    // an invisible draft (sidebar carve-out keys on the column we just
    // failed to write).
    await waitFor(() => {
      expect(deleted).toHaveBeenCalledWith('thread-2');
    });
  });

  it('cleans up the child worktree when seeding the implementation draft fails', async () => {
    const sourceThread = makeTestThread({
      branch: 'main',
      projectId: 'project-1',
      workspacePath: '/repo',
    });
    const pane = await buildPane(sourceThread, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchBase(pane.thread, 'release');
    setNewBranchName(pane.thread, 'feature/custom');
    setBindingMock('ListProposedPlanComments', async () => []);
    setBindingMock('GetPayloadData', async () => ({ data: '# Plan\n\n- step', kind: 'proposed_plan' }));
    setBindingMock('CreateThread', async () => makeTestThread({
      id: 'thread-2',
      branch: 'main',
      projectId: 'project-1',
      workspacePath: '/repo',
    }));
    setBindingMock('PrepareThreadWorktree', async () => makeTestThread({
      id: 'thread-2',
      branch: 'feature/custom',
      projectId: 'project-1',
      workspacePath: '/tmp/wt-feature',
      worktreePath: '/tmp/wt-feature',
    }));
    setBindingMock('SaveDraft', async () => {
      throw new Error('disk full');
    });
    const removeWorktree = setBindingMock('GitRemoveWorktree', async () => {});
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send-menu'));
    await fireEvent.click(await findByText('Implement in new thread'));

    await waitFor(() => {
      expect(removeWorktree).toHaveBeenCalledWith('thread-2');
      expect(deleted).toHaveBeenCalledWith('thread-2');
    });
    expect(removeWorktree.mock.invocationCallOrder[0]).toBeLessThan(
      deleted.mock.invocationCallOrder[0],
    );
    expect(pane.thread?.id).toBe('thread-1');
    expect(worktreeIntentForThread(sourceThread).mode).toBe('new-worktree');
  });

  it('cleans up the implementation thread when child worktree preparation fails', async () => {
    const sourceThread = makeTestThread({
      branch: 'main',
      projectId: 'project-1',
      workspacePath: '/repo',
    });
    const pane = await buildPane(sourceThread, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        turnIndex: 0,
        itemIndex: 0,
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchBase(pane.thread, 'release');
    setNewBranchName(pane.thread, 'feature/custom');
    setBindingMock('ListProposedPlanComments', async () => []);
    setBindingMock('GetPayloadData', async () => ({ data: '# Plan\n\n- step', kind: 'proposed_plan' }));
    setBindingMock('CreateThread', async () => makeTestThread({
      id: 'thread-2',
      branch: 'main',
      projectId: 'project-1',
      workspacePath: '/repo',
    }));
    setBindingMock('PrepareThreadWorktree', async () => {
      throw new Error('branch exists');
    });
    const save = setBindingMock('SaveDraft', async () => {});
    const deleted = setBindingMock('DeleteThread', async () => {});

    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    await findByText('Implement');
    await fireEvent.click(getByTestId('composer-send-menu'));
    await fireEvent.click(await findByText('Implement in new thread'));

    await waitFor(() => {
      expect(deleted).toHaveBeenCalledWith('thread-2');
    });
    expect(save).not.toHaveBeenCalled();
    expect(pane.thread?.id).toBe('thread-1');
  });

  it('forwards the draft sourceProposedPlan on send when no in-thread plan is active', async () => {
    const pane = await buildPane(makeTestThread({ id: 'thread-2', projectId: 'project-1' }));
    const draft = await buildDraft('thread-2');
    // Hydrate the draft with a sourceProposedPlan (e.g. the user just
    // landed on a new thread seeded by Implement-in-new-thread).
    setBindingMock('GetDraft', async () => ({
      threadId: 'thread-2',
      content: 'PLEASE IMPLEMENT THIS PLAN:\n# Plan',
      attachmentIds: [],
      terminalChips: [],
      sourceProposedPlan: { threadId: 'src-thread', itemId: 'plan-1', payloadId: 'payload-1' },
      updatedAt: 1,
    }));
    await draft.setThread(null);
    await draft.setThread('thread-2');

    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ id: 'thread-2', runtimeMode: 'full-access' }));

    const { getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalled();
    });
    const call = send.mock.calls[0];
    expect(call?.[2]).toMatchObject({
      sourceProposedPlan: expect.objectContaining({ threadId: 'src-thread', itemId: 'plan-1' }),
    });
    // The draft's source-plan ref is consumed on send so subsequent
    // turns are regular turns.
    expect(draft.sourceProposedPlan).toBeNull();
  });

  it('opens the plan sidebar from the composer toolbar plan button', async () => {
    const plan = makeItem({
      id: 'plan-1',
      kind: 'tool_call',
      payloadKind: 'proposed_plan',
      payloadId: 'payload-1',
      payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
      updatedAt: 1,
    });
    const pane = await buildPane(makeTestThread(), [plan]);
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', ratio: 1 }]);
    const draft = await buildDraft();
    setBindingMock('ListThreadProposedPlans', async () => []);

    const { findByTestId } = render(Composer, { props: { pane, draft } });
    const button = await findByTestId('composer-plan-sidebar-toggle');
    expect(pane.showPlanSidebar).toBe(false);
    await fireEvent.click(button);
    expect(pane.showPlanSidebar).toBe(true);
  });

  it('keeps the plan sidebar button visible for implemented plan history', async () => {
    const plan = makeItem({
      id: 'plan-1',
      kind: 'tool_call',
      payloadKind: 'proposed_plan',
      payloadId: 'payload-1',
      payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
      meta: JSON.stringify({ planImplementedAt: 123 }),
      updatedAt: 1,
    });
    const pane = await buildPane(makeTestThread(), [plan]);
    const draft = await buildDraft();
    setBindingMock('ListThreadProposedPlans', async () => []);

    const { findByTestId } = render(Composer, { props: { pane, draft } });
    const button = await findByTestId('composer-plan-sidebar-toggle');

    expect(button.textContent?.trim()).toBe('Plan');
  });

  it('does not attach runtime mode overrides to send options', async () => {
    const pane = await buildPane(makeTestThread({ runtimeMode: 'auto-accept-edits' }));
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'auto-accept-edits' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'use persisted access' } });
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'use persisted access', {
        attachmentIds: [],
      });
    });
  });

  it('does not synthesize a runtime override from a missing thread value', async () => {
    const pane = await buildPane(makeTestThread({ runtimeMode: undefined }));
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'approval-required' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'use persisted mode' } });
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'use persisted mode', {
        attachmentIds: [],
      });
    });
  });

  it('prepares a pending worktree before sending', async () => {
    const initialThread = makeTestThread({ branch: 'main' });
    const worktreeThread = makeTestThread({
      branch: 'feature/custom',
      workspacePath: '/tmp/wt-feature',
      worktreePath: '/tmp/wt-feature',
    });
    const pane = await buildPane(initialThread);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchBase(pane.thread, 'release');
    setNewBranchName(pane.thread, 'feature/custom');

    const prepare = setBindingMock('PrepareThreadWorktree', async () => worktreeThread);
    const send = setBindingMock('SendMessageWithOptions', async () => worktreeThread);

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'work there' } });
    await fireEvent.click(getByTestId('composer-send'));

    expect(prepare).toHaveBeenCalledWith('thread-1', 'release', 'feature/custom', false);
    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'work there', {
        attachmentIds: [],
      });
    });
    expect(prepare.mock.invocationCallOrder[0]).toBeLessThan(send.mock.invocationCallOrder[0]);
    expect(pane.thread?.worktreePath).toBe('/tmp/wt-feature');
  });

  it('shows worktree preparation status while creating the worktree', async () => {
    const initialThread = makeTestThread({ branch: 'main' });
    const worktreeThread = makeTestThread({
      branch: 'feature/custom',
      workspacePath: '/tmp/wt-feature',
      worktreePath: '/tmp/wt-feature',
    });
    const pane = await buildPane(initialThread);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    enterCreateBranchMode(pane.thread, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchBase(pane.thread, 'release');
    setNewBranchName(pane.thread, 'feature/custom');

    let finishPrepare!: () => void;
    setBindingMock('PrepareThreadWorktree', async () => {
      await new Promise<void>((resolve) => {
        finishPrepare = resolve;
      });
      return worktreeThread;
    });
    setBindingMock('SendMessageWithOptions', async () => worktreeThread);

    const { getByLabelText, getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'work there' } });
    void fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(getByTestId('composer-worktree-preparing').textContent).toContain('Preparing worktree...');
    });

    finishPrepare();

    await waitFor(() => {
      expect(queryByTestId('composer-worktree-preparing')).toBeNull();
    });
  });

  it('keeps the send bound to the original thread if the pane switches while clearing the draft', async () => {
    const threadOne = makeTestThread({ id: 'thread-1', runtimeMode: 'approval-required' });
    const threadTwo = makeTestThread({ id: 'thread-2', runtimeMode: 'full-access' });
    const pane = await buildPane(threadOne);
    const draft = await buildDraft('thread-1');

    let releaseClear!: () => void;
    const clearStarted = vi.fn();
    setBindingMock('ClearDraft', async () => {
      clearStarted();
      await new Promise<void>((resolve) => {
        releaseClear = resolve;
      });
    });
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ id: 'thread-1' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'race send' } });
    void fireEvent.click(getByTestId('composer-send'));
    await waitFor(() => expect(clearStarted).toHaveBeenCalled());

    await pane.switchThread(threadTwo);
    releaseClear();

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'race send', {
        attachmentIds: [],
      });
    });
    expect(pane.thread?.id).toBe('thread-2');
  });

  it('sends image-only drafts with a visible image placeholder and attachment ids', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments('[Image #1]', [makeAttachment('att-1', 'hero.png')]);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', '[Image #1]', {
        attachmentIds: ['att-1'],
      });
    });
  });

  it('pasting images inserts image placeholders at the cursor and sends ordered attachment ids', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    let nextId = 1;
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({
      ...makeAttachment(`att-${nextId++}`, filename),
      threadId,
      mimeType,
    }));
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'please inspect' } });
    textarea.setSelectionRange(textarea.value.length, textarea.value.length);
    await fireEvent(textarea, makeClipboardPaste([
      new File(['png-one'], 'one.png', { type: 'image/png' }),
      new File(['png-two'], 'two.png', { type: 'image/png' }),
    ]));

    await waitFor(() => expect(upload).toHaveBeenCalledTimes(2));
    expect(draft.content).toBe('please inspect [Image #1] [Image #2]');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['att-1', 'att-2']);

    await fireEvent.click(getByTestId('composer-send'));
    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'please inspect [Image #1] [Image #2]', {
        attachmentIds: ['att-1', 'att-2'],
      });
    });
  });

  it('backspace after an image placeholder removes the whole placeholder and attachment', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments('before [Image #1] after', [makeAttachment('att-1', 'hero.png')]);
    const remove = setBindingMock('DeleteAttachment', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    const cursor = 'before [Image #1]'.length;
    textarea.setSelectionRange(cursor, cursor);

    await fireEvent.keyDown(textarea, { key: 'Backspace' });

    expect(draft.content).toBe('before after');
    expect(draft.attachments).toHaveLength(0);
    await waitFor(() => expect(remove).toHaveBeenCalledWith('att-1'));
  });

  it('delete before or inside an image placeholder removes the whole placeholder and attachment', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments('before [Image #1] after', [makeAttachment('att-1', 'hero.png')]);
    const remove = setBindingMock('DeleteAttachment', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    const cursor = 'before [Ima'.length;
    textarea.setSelectionRange(cursor, cursor);

    await fireEvent.keyDown(textarea, { key: 'Delete' });

    expect(draft.content).toBe('before after');
    expect(draft.attachments).toHaveLength(0);
    await waitFor(() => expect(remove).toHaveBeenCalledWith('att-1'));
  });

  it('removing an attachment with the thumbnail X removes its placeholder and renumbers later images', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments(
      '[Image #1] [Image #2] [Image #3]',
      [
        makeAttachment('att-1', 'first.png'),
        makeAttachment('att-2', 'second.png'),
        makeAttachment('att-3', 'third.png'),
      ],
    );
    const remove = setBindingMock('DeleteAttachment', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    await fireEvent.click(getByLabelText('Remove second.png'));

    expect(draft.content).toBe('[Image #1] [Image #2]');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['att-1', 'att-3']);
    await waitFor(() => expect(remove).toHaveBeenCalledWith('att-2'));
  });

  it('shows the interrupt affordance while a turn is active and interrupts on click', async () => {
    const pane = await buildPane();
    // Post-refactor, isTurnActive is wire-pushed — a streaming item no
    // longer flips it on. Driving setActiveTurn directly simulates the
    // `provider:turn_started` event the composer really depends on.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    const draft = await buildDraft();
    const interrupt = setBindingMock('InterruptTurn', async () => {});

    const { getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });

    expect(queryByTestId('composer-send')).toBeNull();
    await fireEvent.click(getByTestId('composer-interrupt'));

    expect(interrupt).toHaveBeenCalledWith('thread-1');
  });

  it('shows the interrupt affordance while a send is waiting for turn start', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    const interrupt = setBindingMock('InterruptTurn', async () => {});
    projectSendStarted('thread-1');

    const { getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });

    expect(queryByTestId('composer-send')).toBeNull();
    await fireEvent.click(getByTestId('composer-interrupt'));

    expect(interrupt).toHaveBeenCalledWith('thread-1');
  });

  it('renders the background tray inside the composer card before the input', async () => {
    const pane = await buildPane();
    setBindingMock('ListLiveBackgroundTasks', async () => [{
      id: 'launch-a',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      summary: 'Bash',
      isBackground: true,
      createdAt: Date.now() - 1_000,
      updatedAt: Date.now() - 1_000,
    }]);
    const draft = await buildDraft();

    const { getByTestId, getByLabelText } = render(Composer, { props: { pane, draft } });
    await tick();
    await tick();

    const root = getByTestId('composer-root');
    const rail = getByTestId('activity-rail');
    const input = getByLabelText('Message Input');

    expect(root.contains(rail)).toBe(true);
    expect(rail.compareDocumentPosition(input) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('reserves the activity row height above the card when idle and yields it to the rail on turn start/end', async () => {
    // The composer is a bottom-anchored overlay whose height drives the
    // timeline's padding-bottom (--composer-height); the ActivityRail's
    // working row mounting/unmounting at turn boundaries is what made the
    // last message jump. The reservation swaps in lockstep with the rail
    // (net composer height unchanged) and lives ABOVE the card so the card
    // itself looks identical whether or not a turn is running.
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });
    await tick();
    await tick();

    // Idle: rail collapsed; the reservation is present and sits OUTSIDE
    // (before) the visible card — not a dead-space lump inside it.
    const reserve = getByTestId('composer-activity-reserve');
    const card = getByTestId('composer-root');
    expect(queryByTestId('activity-rail')).toBeNull();
    expect(card.contains(reserve)).toBe(false);
    expect(reserve.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    // Capture the spacer's row + chip classes before it unmounts; the
    // live rail must carry the identical strings (height-twin by
    // construction — both sides render activityRailClasses.ts).
    const reserveRow = reserve.querySelector(':scope > div');
    const reserveChip = reserveRow?.querySelector(':scope > span');
    expect(reserveRow).not.toBeNull();
    expect(reserveChip).not.toBeNull();
    const reserveRowClass = reserveRow!.className;
    const reserveChipClass = reserveChip!.className;

    // Turn starts: the rail takes the row inside the card, reservation collapses.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    await tick();
    expect(getByTestId('activity-rail')).toBeInTheDocument();
    expect(queryByTestId('composer-activity-reserve')).toBeNull();
    const railRow = getByTestId('activity-rail').querySelector(':scope > div');
    expect(railRow?.className).toBe(reserveRowClass);
    expect(getByTestId('activity-rail-working').className).toBe(reserveChipClass);

    // Turn ends: rail collapses, reservation returns.
    pane.clearActiveTurn();
    await tick();
    expect(queryByTestId('activity-rail')).toBeNull();
    expect(queryByTestId('composer-activity-reserve')).not.toBeNull();
  });

  it('does not double-reserve: a todos-only rail (no active turn) occupies the row and the spacer stays collapsed', async () => {
    // reserveActivityRow mirrors the rail's own visibility predicate for
    // the synchronous cases (working + todos), so a settled-turn rail that
    // still shows todos doesn't stack an extra transparent row above the card.
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    const pane = await buildPane();
    pane.setLiveTodo([{ step: 'one', status: 'inProgress' }]);
    const draft = await buildDraft();

    const { getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });
    await tick();
    await tick();

    expect(getByTestId('activity-rail')).toBeInTheDocument();
    expect(queryByTestId('composer-activity-reserve')).toBeNull();
  });

  it('scopes pointer-events and drag-and-drop to the visible card', async () => {
    // The outer wrapper is pointer-events-none so the transparent
    // moat around the visible card is click-through. Clicks on the
    // scrollbar gutter and on transcript rows visible through the
    // moat hit the timeline behind, instead of being silently
    // absorbed. Drag-and-drop handlers move to the card itself for
    // the same reason — users drop where they see the composer.
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByTestId, queryByText } = render(Composer, { props: { pane, draft } });
    await tick();

    const card = getByTestId('composer-root');
    expect(card.className).toContain('pointer-events-auto');

    const outer = card.parentElement as HTMLElement | null;
    expect(outer).not.toBeNull();
    expect(outer!.className).toContain('pointer-events-none');

    // No drop hint before any drag activity.
    expect(queryByText('Drop an image to attach')).toBeNull();

    const fileDataTransfer = {
      types: ['Files'],
      dropEffect: 'none',
      effectAllowed: 'copy',
      files: [],
      items: [],
      getData: () => '',
      setData: () => {},
      setDragImage: () => {},
    } as unknown as DataTransfer;

    // Dispatch dragenter on the card — the new handler home. The
    // drop hint becomes visible (rendered by ComposerAttachmentRow
    // when uploads.dragActive flips true), proving the handler
    // moved off the outer wrapper.
    await fireEvent.dragEnter(card, { dataTransfer: fileDataTransfer });
    await tick();
    expect(queryByText('Drop an image to attach')).not.toBeNull();
  });

  it('enqueues mid-turn instead of dispatching SendMessage (Enter key)', async () => {
    // Mid-round behaviour matches both reference UIs (Claude Code's
    // commandQueue, Codex's VecDeque<QueuedUserMessage>): the user
    // can keep typing and submit; the message lands in the per-thread
    // queue and drains on the next provider:turn_completed. No more
    // "Cannot send during an active turn" block.
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'queue me' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    await tick();

    expect(send).not.toHaveBeenCalled();
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['queue me']);
    // Composer cleared after enqueue so the user can stack the next
    // message immediately.
    expect(draft.content).toBe('');
  });

  it('does not clear a restored draft when queue restore arrives before RegisterQueueItem resolves', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    let restored = false;
    setBindingMock('GetDraft', async (threadId: string) => ({
      threadId,
      content: restored ? 'restored after session death' : '',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: restored ? 2 : 1,
    }));
    const draft = await buildDraft();
    setBindingMock('RegisterQueueItem', async () => {
      restored = true;
      await draft.reloadFromBackend('thread-1');
      return {
        id: 'q-restored',
        threadId: 'thread-1',
        message: 'queue then restore',
        attachmentIds: [],
        sourceProposedPlan: null,
        revisionSourceProposedPlan: null,
        enqueuedAt: 1,
      };
    });

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'queue then restore' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    await tick();

    expect(draft.content).toBe('restored after session death');
  });

  it('enqueues mid-turn when the Send button is clicked', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'queue via click' } });
    // With a draft typed during an active turn, the SendButton
    // shows Send (not Stop) so the user can queue. Click it.
    await fireEvent.click(getByTestId('composer-send'));
    await tick();

    expect(send).not.toHaveBeenCalled();
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['queue via click']);
  });

  it('captures attachments + plan-revision metadata on the queued item', async () => {
    const pane = await buildPane(makeTestThread(), [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadKind: 'proposed_plan',
        payloadId: 'payload-1',
        payloadMeta: JSON.stringify({ title: 'Plan', preview: '# Plan' }),
        updatedAt: 1,
      }),
    ]);
    const draft = await buildDraft();
    setBindingMock('ListProposedPlanComments', async () => [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: 'Selected section',
      body: 'Tighten this up.',
      createdAt: 1,
      updatedAt: 1,
    }]);
    draft.setContentAndAttachments('Refine the plan', [makeAttachment('att-1', 'hero.png')]);

    const { findByText, getByTestId } = render(Composer, { props: { pane, draft } });
    // Wait for plan + comments to hydrate (label appears while idle).
    await findByText('Send comments');
    // Now flip to active-turn — the SendButton stays in Send variant
    // because canSend is true (draft + comments). Click queues with
    // the plan-revision metadata captured.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    await tick();
    await fireEvent.click(getByTestId('composer-send'));
    await tick();

    const queue = getQueueForThread('thread-1');
    expect(queue).toHaveLength(1);
    const item = queue[0];
    // Image placeholders are appended by composeOutgoingMessage so the
    // provider receives a textual marker for each attachment.
    expect(item.message).toBe('Refine the plan [Image #1]');
    expect([...item.attachmentIds]).toEqual(['att-1']);
    expect(item.revisionSourceProposedPlan).toMatchObject({
      threadId: 'thread-1',
      itemId: 'plan-1',
      payloadId: 'payload-1',
    });
    expect([...(item.revisionSourceCommentIds ?? [])]).toEqual(['comment-1']);
  });

  it('still dispatches SendMessage when no turn is active', async () => {
    const pane = await buildPane();
    // No setActiveTurn — pane is idle.
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'idle send' } });
    await fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => expect(send).toHaveBeenCalledWith('thread-1', 'idle send', { attachmentIds: [] }));
    expect(getQueueForThread('thread-1')).toEqual([]);
  });

  // Runtime mode is persisted on the thread/defaults before send time.
  // Queue items intentionally carry message payload only; the backend
  // dispatch path reads the thread's current runtime mode.

  it('refuses to enqueue when a blocking approval is pending', async () => {
    // canSend gates on `!hasBlockingPrompt`, so a click during a
    // pending approval can't reach the enqueue branch. Pin this so
    // a future loosening of canSend doesn't silently allow the user
    // to stack messages while the agent is blocked on approval —
    // the approval panel renders over the composer for a reason.
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      kind: 'command',
      toolName: 'Bash',
      title: 'Approve command',
      description: 'rm -rf',
      input: { command: 'rm -rf' },
    });
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    // Approval panel hijacks the input — typing into the textarea
    // is intercepted; we drive draft state directly.
    draft.setContent('I want to queue this');
    await fireEvent.keyDown(getByLabelText('Message Input'), { key: 'Enter', shiftKey: false });
    await tick();

    expect(send).not.toHaveBeenCalled();
    expect(getQueueForThread('thread-1')).toEqual([]);
  });

  it('routes Codex mid-turn sends through the unified backend queue', async () => {
    // Phase G6 collapsed the Codex steer fast-path into the same
    // backend-owned queue both providers now share. The dispatcher
    // delivers each queued item via Steer (with Send fallback) for
    // Codex; the Composer is provider-agnostic and never calls
    // SteerMessageWithOptions directly. Pin so a future regression
    // doesn't reintroduce a frontend-side steer dispatch that
    // double-counts against the backend queue.
    const pane = await buildPane(makeTestThread({ provider: 'codex' }));
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ provider: 'codex', runtimeMode: 'full-access' }));
    const steer = setBindingMock('SteerMessageWithOptions', async () =>
      makeTestThread({ provider: 'codex', runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'queue me' } });
    await fireEvent.click(getByTestId('composer-send'));
    await tick();

    expect(send).not.toHaveBeenCalled();
    expect(steer).not.toHaveBeenCalled();
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['queue me']);
    expect(draft.content).toBe('');
  });

  it('keeps the existing enqueue path for Claude mid-turn sends', async () => {
    // Claude has no steer primitive — its per-thread queue + drain
    // on provider:turn_completed remains the single mid-turn path.
    // Pin so Phase D doesn't accidentally start calling Steer for
    // Claude threads.
    const pane = await buildPane(makeTestThread({ provider: 'claude' }));
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    const draft = await buildDraft();
    const steer = setBindingMock('SteerMessageWithOptions', async () =>
      makeTestThread({ provider: 'claude', runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message Input'), { target: { value: 'claude queue' } });
    await fireEvent.click(getByTestId('composer-send'));
    await tick();

    expect(steer).not.toHaveBeenCalled();
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['claude queue']);
  });

  it('autosizes multiline input and clamps at the maximum composer height', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    Object.defineProperty(textarea, 'scrollHeight', {
      configurable: true,
      get: () => 260,
    });

    await fireEvent.input(textarea, { target: { value: 'one\ntwo\nthree\nfour' } });

    expect(textarea.style.height).toBe('200px');
  });

  it('autosizes multiline input below the maximum composer height', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    Object.defineProperty(textarea, 'scrollHeight', {
      configurable: true,
      get: () => 96,
    });

    await fireEvent.input(textarea, { target: { value: 'one\ntwo' } });

    expect(textarea.style.height).toBe('96px');
  });

  it('autosizes restored draft content without waiting for a new input event', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContent('one\ntwo\nthree');

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    Object.defineProperty(textarea, 'scrollHeight', {
      configurable: true,
      get: () => 144,
    });

    await waitFor(() => {
      expect(textarea.style.height).toBe('144px');
    });
  });

  it('restores the draft and surfaces an error when send fails', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    setBindingMock('SendMessageWithOptions', async () => {
      throw new Error('rpc down');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'fails' } });
    await fireEvent.click(getByTestId('composer-send'));
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(draft.content).toBe('fails');
    expect(pane.generalError).toMatch(/Failed to send message/);
    consoleError.mockRestore();
  });

  it('keeps a pending user-input answer separate from the normal draft', async () => {
    const pane = await buildPane();
    pane.addUserInput({
      requestId: 'req-input',
      threadId: 'thread-1',
      toolName: 'AskUserQuestion',
      title: 'Input requested',
      questions: [{ id: 'name', header: 'Name', question: 'Name?' }],
    });
    const draft = await buildDraft();
    draft.setContent('normal prompt stays here');
    const respond = setBindingMock('RespondToUserInput', async () => {});

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    expect(textarea.value).toBe('');
    await fireEvent.input(textarea, { target: { value: 'Randy' } });
    await tick();
    await fireEvent.click(getByTestId('user-input-submit'));

    expect(draft.content).toBe('normal prompt stays here');
    expect(respond).toHaveBeenCalledTimes(1);
    expect(respond.mock.calls[0]).toMatchObject([
      'thread-1',
      {
        requestId: 'req-input',
        decision: 'accept',
        answers: { name: 'Randy' },
      },
    ]);
  });

  it('keeps composer toolbar chrome visible and hides send during pending user input', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText, getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    textarea.focus();

    pane.addUserInput({
      requestId: 'req-input',
      threadId: 'thread-1',
      toolName: 'AskUserQuestion',
      title: 'Input requested',
      questions: [{
        id: 'framework',
        header: 'Framework',
        question: 'Pick one',
        options: [
          { label: 'React', description: '' },
          { label: 'Svelte', description: '' },
        ],
      }],
    });
    await tick();

    expect(getByTestId('composer-toolbar')).toBeInTheDocument();
    expect(getByTestId('composer-model-menu-trigger')).toBeInTheDocument();
    expect(queryByTestId('composer-send')).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(getByTestId('user-input-option-1')));

    await fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(document.activeElement).toBe(getByTestId('user-input-option-2'));
  });

  it('focuses the textarea after draft hydrates with cursor at end of resumed content', async () => {
    setBindingMock('GetDraft', async (threadId: string) => ({
      threadId,
      content: 'resumed text',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 0,
    }));
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await waitFor(() => {
      expect(document.activeElement).toBe(textarea);
      expect(textarea.selectionStart).toBe('resumed text'.length);
      expect(textarea.selectionEnd).toBe('resumed text'.length);
    });
  });

  it('focuses with cursor at offset 0 when the draft is empty', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await waitFor(() => {
      expect(document.activeElement).toBe(textarea);
      expect(textarea.selectionStart).toBe(0);
      expect(textarea.selectionEnd).toBe(0);
    });
  });

  it('does not focus the textarea while the draft is still hydrating', async () => {
    let resolveGetDraft!: (value: unknown) => void;
    const pendingGetDraft = new Promise<unknown>((resolve) => {
      resolveGetDraft = resolve;
    });
    setBindingMock('GetDraft', () => pendingGetDraft);
    const pane = await buildPane();
    const draft = createComposerDraftStore({ debounceMs: 0 });
    // Kick off hydration without awaiting it — the GetDraft mock above
    // is parked until we resolve it below.
    void draft.setThread('thread-1');

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await tick();
    expect(draft.hydrating).toBe(true);
    expect(document.activeElement).not.toBe(textarea);

    resolveGetDraft({
      threadId: 'thread-1',
      content: 'late text',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 0,
    });

    await waitFor(() => {
      expect(document.activeElement).toBe(textarea);
      expect(textarea.selectionStart).toBe('late text'.length);
    });
  });

  it('does not steal focus when textarea is disabled (no active thread)', async () => {
    const pane = createThreadPane();
    const draft = await buildDraft(null);

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    await tick();
    expect(textarea.disabled).toBe(true);
    expect(document.activeElement).not.toBe(textarea);
  });

  // Regression — cold terminal open must not lose focus to the composer.
  // Opening the bottom terminal (⌘`/⌘J) on a placeholder thread materializes
  // it, which re-arms the composer's initial-focus pass. By the time the draft
  // re-hydrates, the xterm may already hold DOM focus; the composer must not
  // steal it back. These two tests are a self-validating pair: the control
  // proves the initial-focus pass fires (and the spy is wired to the
  // component's call) under this exact setup, so the guarded test's
  // "not called" can't be a false pass from the effect simply not running.
  it('control: focuses the textarea on entry when no terminal holds focus', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    const focusSpy = vi.spyOn(composerKeyboard, 'focusTextareaAtEnd');
    try {
      render(Composer, { props: { pane, draft } });
      await waitFor(() => expect(focusSpy).toHaveBeenCalledTimes(1));
    } finally {
      focusSpy.mockRestore();
    }
  });

  it('does not steal focus from a terminal that already owns it on entry', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    // The xterm grabbed DOM focus when the drawer opened, reflected in the
    // pane-keyed terminal-focus registry the guard reads. Register under THIS
    // pane's id so the guard (scoped to `pane.paneId`) sees it.
    notifyTerminalFocus(pane.paneId, true);
    const focusSpy = vi.spyOn(composerKeyboard, 'focusTextareaAtEnd');
    try {
      const { getByLabelText } = render(Composer, { props: { pane, draft } });
      const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
      // Settle to the same steady state the control reaches: the textarea is
      // enabled and the initial-focus effect has run (waitFor polls on a
      // macrotask, after Svelte's effect flush). The control proves the effect
      // calls focusTextareaAtEnd at this point absent a focused terminal.
      await waitFor(() => expect(textarea.disabled).toBe(false));
      await tick();
      expect(focusSpy).not.toHaveBeenCalled();
    } finally {
      focusSpy.mockRestore();
    }
  });

  it('yields Shift+Tab to the global keydown handler without preventDefault', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;

    const event = new KeyboardEvent('keydown', {
      key: 'Tab',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    textarea.dispatchEvent(event);
    await tick();

    expect(event.defaultPrevented).toBe(false);
  });

  it('swallows plain Tab so it does not move focus out of the textarea', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    textarea.focus();
    expect(document.activeElement).toBe(textarea);

    const event = new KeyboardEvent('keydown', {
      key: 'Tab',
      bubbles: true,
      cancelable: true,
    });
    textarea.dispatchEvent(event);
    await tick();

    expect(event.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(textarea);
  });
});
