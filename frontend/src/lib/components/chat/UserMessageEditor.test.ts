// The in-place message editor on its own: the bubble swap, the
// keyboard/cancel semantics, the send gate, and the attachment-record
// policy that makes a cancelled edit leave the transcript pristine.
// ChatView.test.ts is the integration proof for the saga behind Send.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import UserMessage from './UserMessage.svelte';
import UserMessageEditor from './UserMessageEditor.svelte';
import type { UserMessageEditSession, UserMessageEditStage } from './userMessageActions';
import { createUserMessageEditUiState } from './userMessageEditUi.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
} from '../../stores/composerDraft.svelte';
import type { ComposerDraftSnapshot } from '../../stores/composerDraftSnapshots';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import {
  projectTurnCompleted,
  projectTurnStarted,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';
import { restoredDraftSnapshotFromUserItem } from '../../utils/userMessageDraftSnapshot';
import type { Attachment } from '../../types/attachment';
import type { Item } from '../../types/models';
import type { ThreadPane } from '../../stores/thread.svelte';

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
  setBindingMock('GetAttachmentThumbnail', async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }));
  setBindingMock('GetAttachmentData', async () => 'iVBORw0KGgo=');
  setBindingMock('DeleteAttachment', async () => {});
  setBindingMock('SearchWorkspaceFiles', async () => ({ files: [], truncated: false, root: '/tmp' }));
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: false, commands: [] }));
  setBindingMock('GetCodexSkills', async () => ({ cwd: '/tmp', skills: [], errors: [] }));
  setBindingMock('GetClaudeSkills', async () => []);
}

function makeAttachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 64,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
}

function makeUserItem(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'user:1',
    threadId: 'thread-1',
    turnIndex: 1,
    itemIndex: 0,
    kind: 'user_text',
    role: 'user',
    summary: 'original prompt',
    ...overrides,
  });
}

// Return type is inferred so the mocks keep their call signatures.
function makeSession(item: Item, stage: UserMessageEditStage = 'editing') {
  const seeded: ComposerDraftSnapshot = restoredDraftSnapshotFromUserItem(item);
  const draft = createComposerDraftStore({ persistence: 'none' });
  draft.seedLocalSnapshot(item.threadId, seeded);
  const onCancel = vi.fn(() => {});
  const onSubmit = vi.fn((_payload: {
    message: string;
    attachmentIds: string[];
    providerCommand: boolean;
  }) => {});
  // The real factory, so the row's writes are reactive exactly as they are
  // in production and the starting state cannot drift from ChatView's.
  const ui = createUserMessageEditUiState();
  const session: UserMessageEditSession = {
    itemId: item.id,
    draft,
    seeded,
    sessionUploadedIds: new Set<string>(),
    ui,
    stage,
    onCancel,
    onSubmit,
  };
  return { seeded, onCancel, onSubmit, session, ui };
}

async function mountEditor(
  item: Item = makeUserItem(),
  stage: UserMessageEditStage = 'editing',
) {
  const pane: ThreadPane = await buildPane(makeThread({ id: item.threadId }));
  const handles = makeSession(item, stage);
  const props = {
    pane,
    session: handles.session,
    onCancel: handles.onCancel,
  };
  const rendered = render(UserMessageEditor, { props });
  await tick();
  const textarea = rendered.getByLabelText('Message Input') as HTMLTextAreaElement;

  async function type(value: string): Promise<void> {
    await fireEvent.input(textarea, { target: { value } });
    textarea.setSelectionRange(value.length, value.length);
    await fireEvent.select(textarea);
    await tick();
    await rendered.rerender(props);
    await tick();
  }

  return { ...rendered, ...handles, pane, textarea, type, props };
}

describe('<UserMessageEditor>', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetBindingMocks();
    resetComposerDraftSnapshotsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    resetThreadStatuses();
    installMocks();
  });

  it('replaces the message body with an editor seeded from the message', async () => {
    const pane: ThreadPane = await buildPane(makeThread({ id: 'thread-1' }));
    const item = makeUserItem({
      summary: 'original prompt\n\n![shot](attachment://att-1)',
      meta: JSON.stringify({
        attachments: [{ id: 'att-1', filename: 'shot.png', mimeType: 'image/png', size: 64 }],
      }),
    });
    const { session } = makeSession(item);

    const { getByTestId, queryByTestId } = render(UserMessage, {
      props: { pane, item, actions: { onEditMessage: vi.fn(), editSession: session } },
    });
    await tick();

    expect(getByTestId('user-message-editor')).toBeInTheDocument();
    // The read-only rendering is gone: the editor's own attachment row
    // shows the draft's copy, so a second grid would be a stale duplicate.
    expect(queryByTestId('user-message-summary')).toBeNull();
    expect(queryByTestId('user-message-attachments')).toBeNull();
    expect(getByTestId('composer-attachment-row')).toBeInTheDocument();
  });

  it('submits the composed message with its attachment ids', async () => {
    const item = makeUserItem();
    const { onSubmit, session, type, getByTestId, props, rerender } = await mountEditor(item);
    await type('rewritten prompt');
    // Uploads land on the draft (the surface owns that path); adding after
    // the text mirrors a paste into a message that already has content.
    session.draft.addAttachment(makeAttachment('att-9'));
    await rerender(props);
    await tick();

    await fireEvent.click(getByTestId('user-message-edit-send'));

    expect(onSubmit).toHaveBeenCalledTimes(1);
    const payload = onSubmit.mock.calls[0][0];
    expect(payload.message).toContain('rewritten prompt');
    expect(payload.attachmentIds).toEqual(['att-9']);
    expect(payload.providerCommand).toBe(false);
  });

  it('submits on plain Enter', async () => {
    const { onSubmit, textarea, type } = await mountEditor();
    await type('rewritten prompt');

    await fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it('refuses to send an empty message or one during an active turn', async () => {
    const { getByTestId, type } = await mountEditor();

    await type('   ');
    expect(getByTestId('user-message-edit-send')).toBeDisabled();

    await type('rewritten prompt');
    expect(getByTestId('user-message-edit-send')).not.toBeDisabled();

    projectTurnStarted('thread-1', 'turn-1', 1, 1000);
    await tick();
    expect(getByTestId('user-message-edit-send')).toBeDisabled();

    projectTurnCompleted('thread-1', 'turn-1');
    await tick();
    expect(getByTestId('user-message-edit-send')).not.toBeDisabled();
  });

  it('cancels instantly and silently when nothing was changed', async () => {
    const { getByTestId, onCancel, queryByText } = await mountEditor();

    await fireEvent.click(getByTestId('user-message-edit-cancel'));

    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(queryByText('Discard changes?')).toBeNull();
  });

  it('confirms before discarding an edited message', async () => {
    const { getByTestId, getByText, findByText, onCancel, type } = await mountEditor();
    await type('rewritten prompt');

    await fireEvent.click(getByTestId('user-message-edit-cancel'));
    await findByText('Discard changes?');
    expect(onCancel).not.toHaveBeenCalled();

    await fireEvent.click(getByText('Discard'));
    await waitFor(() => expect(onCancel).toHaveBeenCalledTimes(1));
  });

  it('cancels on Escape, and yields Escape to an open completion popover', async () => {
    const { textarea, onCancel, type, queryByTestId } = await mountEditor();

    // With the `/` menu open, Escape belongs to the menu: the editor must
    // not close out from under a user dismissing a popover.
    await type('/w');
    await waitFor(() => expect(queryByTestId('slash-popover')).not.toBeNull());
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    expect(onCancel).not.toHaveBeenCalled();
    await waitFor(() => expect(queryByTestId('slash-popover')).toBeNull());

    // Nothing open now, and the text is back to the seed, so Escape is an
    // instant, silent cancel.
    await type('original prompt');
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('deletes only the records of attachments this session uploaded', async () => {
    const item = makeUserItem({
      summary: 'original prompt\n\n![shot](attachment://att-seeded)',
      meta: JSON.stringify({
        attachments: [{ id: 'att-seeded', filename: 'shot.png', mimeType: 'image/png', size: 64 }],
      }),
    });
    const deleteAttachment = setBindingMock('DeleteAttachment', async () => {});
    const { session, getAllByLabelText, props, rerender } = await mountEditor(item);

    // An upload during the session registers as ours.
    session.draft.addAttachment(makeAttachment('att-new'));
    await rerender(props);
    await tick();
    expect(session.sessionUploadedIds.has('att-new')).toBe(true);
    expect(session.sessionUploadedIds.has('att-seeded')).toBe(false);

    // Removing the SEEDED attachment must not delete its record — the
    // original message still references it until the revert commits.
    const removes = getAllByLabelText(/^Remove /);
    await fireEvent.click(removes[0]);
    await tick();
    expect(deleteAttachment).not.toHaveBeenCalledWith('att-seeded');
  });

  it('disables both controls and spins the primary while the saga runs', async () => {
    const { getByTestId } = await mountEditor(makeUserItem(), 'executing');

    expect(getByTestId('user-message-edit-cancel')).toBeDisabled();
    const send = getByTestId('user-message-edit-send');
    expect(send).toBeDisabled();
    expect(send.getAttribute('aria-busy')).toBe('true');
  });

  // The discard confirm is gated on a VALUE comparison against the seed,
  // not on whether the user touched anything — so the attachment set is
  // as much a change as the text is, and undoing a change is not a
  // change. The three tests below pin all three corners of that, because
  // the cheap implementation (a "touched" flag) passes the first two and
  // fails the third.

  it('treats an added attachment as a change worth confirming', async () => {
    const { session, props, rerender, getByTestId, findByText, onCancel } = await mountEditor();

    session.draft.addAttachment(makeAttachment('att-9'));
    await rerender(props);
    await tick();

    await fireEvent.click(getByTestId('user-message-edit-cancel'));
    await findByText('Discard changes?');
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('treats removing a seeded attachment as a change worth confirming', async () => {
    const item = makeUserItem({
      summary: 'original prompt\n\n![shot](attachment://att-seeded)',
      meta: JSON.stringify({
        attachments: [{ id: 'att-seeded', filename: 'shot.png', mimeType: 'image/png', size: 64 }],
      }),
    });
    const {
      getAllByLabelText, getByTestId, findByText, onCancel, props, rerender,
    } = await mountEditor(item);

    await fireEvent.click(getAllByLabelText(/^Remove /)[0]);
    await rerender(props);
    await tick();

    await fireEvent.click(getByTestId('user-message-edit-cancel'));
    await findByText('Discard changes?');
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('goes clean again when the text is typed back to the seed', async () => {
    const { getByTestId, onCancel, queryByText, type } = await mountEditor();

    await type('rewritten prompt');
    await type('original prompt');

    // Nothing to lose, so nothing to confirm: the dialog's promise is
    // about the DIFFERENCE from the message, not about keystroke history.
    await fireEvent.click(getByTestId('user-message-edit-cancel'));
    expect(queryByText('Discard changes?')).toBeNull();
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('keeps the edit and its dirty state across a virtualizer remount', async () => {
    // The anchor row is virtualized: scrolling it out and back
    // destroys and rebuilds this component. Everything durable lives on
    // the session object ChatView owns, so a remount must find the same
    // text — and must still know the edit is unsaved.
    const item = makeUserItem();
    const mounted = await mountEditor(item);
    await mounted.type('rewritten prompt');
    mounted.unmount();

    const remounted = render(UserMessageEditor, { props: mounted.props });
    await tick();
    expect(remounted.getByLabelText('Message Input')).toHaveValue('rewritten prompt');

    await fireEvent.click(remounted.getByTestId('user-message-edit-cancel'));
    await remounted.findByText('Discard changes?');
    expect(mounted.onCancel).not.toHaveBeenCalled();
  });

  it('focuses the input when the reader opens it, and never on a remount', async () => {
    // Opening the editor is a focus request. A remount is the virtualizer
    // rebuilding a row that scrolled back into view — grabbing focus there
    // would yank the caret out of wherever the reader had moved it.
    const mounted = await mountEditor();
    expect(document.activeElement).toBe(mounted.textarea);
    expect(mounted.ui.focusPending).toBe(false);

    mounted.unmount();
    const remounted = render(UserMessageEditor, { props: mounted.props });
    await tick();

    expect(document.activeElement).not.toBe(remounted.getByLabelText('Message Input'));
  });

  it('puts the caret back after a remount without taking focus', async () => {
    const mounted = await mountEditor();
    await mounted.type('rewritten prompt');
    mounted.textarea.setSelectionRange(4, 9);
    await fireEvent.select(mounted.textarea);
    mounted.unmount();

    const remounted = render(UserMessageEditor, { props: mounted.props });
    await tick();
    const textarea = remounted.getByLabelText('Message Input') as HTMLTextAreaElement;

    expect(textarea.selectionStart).toBe(4);
    expect(textarea.selectionEnd).toBe(9);
    expect(document.activeElement).not.toBe(textarea);
  });

  it('re-uses the pane\'s attachment thumbnails across a remount', async () => {
    // The editor's attachment row shows the same images the message bubble
    // already decoded. Without the pane-owned cache every windowing
    // remount revokes them and re-fetches + re-decodes each one.
    const item = makeUserItem({
      summary: 'original prompt\n\n![shot](attachment://att-seeded)',
      meta: JSON.stringify({
        attachments: [{ id: 'att-seeded', filename: 'shot.png', mimeType: 'image/png', size: 64 }],
      }),
    });
    const thumbnail = setBindingMock(
      'GetAttachmentThumbnail',
      async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }),
    );
    const mounted = await mountEditor(item);
    await waitFor(() => expect(thumbnail).toHaveBeenCalledTimes(1));

    mounted.unmount();
    const remounted = render(UserMessageEditor, { props: mounted.props });
    await tick();
    await waitFor(() => expect(remounted.getByTestId('composer-attachment-row')).toBeInTheDocument());

    expect(thumbnail).toHaveBeenCalledTimes(1);
  });

  it('keeps the discard confirm open across a remount', async () => {
    // The dialog asks about work the user is about to lose; a row that
    // scrolls out and back must not answer it for them.
    const mounted = await mountEditor();
    await mounted.type('rewritten prompt');
    await fireEvent.click(mounted.getByTestId('user-message-edit-cancel'));
    await mounted.findByText('Discard changes?');
    mounted.unmount();

    const remounted = render(UserMessageEditor, { props: mounted.props });
    await tick();

    await remounted.findByText('Discard changes?');
    expect(mounted.onCancel).not.toHaveBeenCalled();
  });

  it('lets a second Escape through to the editor once the popover has taken the first', async () => {
    // The ordering is the whole point: the surface hands the host its
    // first look at the keydown, so the editor listens on the way OUT
    // and yields to `defaultPrevented`. One Escape must never do both.
    const { textarea, onCancel, type, queryByTestId, findByText } = await mountEditor();

    await type('/w');
    await waitFor(() => expect(queryByTestId('slash-popover')).not.toBeNull());

    await fireEvent.keyDown(textarea, { key: 'Escape' });
    await waitFor(() => expect(queryByTestId('slash-popover')).toBeNull());
    expect(onCancel).not.toHaveBeenCalled();

    // Second Escape, no retyping: it reaches the editor, and because the
    // text still differs from the seed it asks rather than discarding.
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    await findByText('Discard changes?');
    expect(onCancel).not.toHaveBeenCalled();
  });
});
