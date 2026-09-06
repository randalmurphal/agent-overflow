// The `draft:updated` fan-out: which frames make a composer re-read its row,
// and — more importantly — which ones must not.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { noteThread } from '../transport/entityIndex';
import { applyDraftUpdated, resyncDraftsForBackend } from './eventsDraftRows';
import {
  registerComposerDraft,
  resetComposerDraftRegistryForTest,
} from './composerDraftRegistry.svelte';
import {
  rememberDraftSnapshot,
  resetComposerDraftSnapshotStateForTest,
} from './composerDraftSnapshots';
import type { ComposerDraftStore } from './composerDraft.svelte';
import { resetPanesForTest } from './panes.svelte';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import { getConnectionId } from '../transport/clientIdentity';

function stubDraft(): { draft: ComposerDraftStore; reloadFromBackend: ReturnType<typeof vi.fn> } {
  const reloadFromBackend = vi.fn(async () => {});
  return {
    reloadFromBackend,
    draft: { reloadFromBackend } as unknown as ComposerDraftStore,
  };
}

async function paneWithDraft(threadId: string, paneKey = 'main') {
  const pane = await buildPane(makeThread({ id: threadId }), [], paneKey);
  const { draft, reloadFromBackend } = stubDraft();
  registerComposerDraft(pane.paneId, draft);
  return { pane, reloadFromBackend };
}

function frameFrom(threadId: string, connectionId?: string) {
  return { threadId, updatedAt: 1_700_000_000_000, deviceId: 'device-other', connectionId };
}

describe('applyDraftUpdated', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetComposerDraftRegistryForTest();
    resetComposerDraftSnapshotStateForTest();
  });

  it('recovers only the reconnecting computer drafts, retaining local edits', async () => {
    const first = await paneWithDraft('thread-a', 'first');
    const second = await paneWithDraft('thread-b', 'second');
    noteThread('thread-a', '');
    noteThread('thread-b', 'gpu');
    resyncDraftsForBackend('');
    expect(first.reloadFromBackend).toHaveBeenCalledWith('thread-a');
    expect(second.reloadFromBackend).not.toHaveBeenCalled();
    rememberDraftSnapshot('thread-b', { content: 'unsaved work', attachments: [], terminalChips: [], sourceProposedPlan: null });
    resyncDraftsForBackend('gpu');
    expect(second.reloadFromBackend).not.toHaveBeenCalled();
  });

  it('re-reads the row when another client wrote the draft', async () => {
    const { reloadFromBackend } = await paneWithDraft('thread-a');

    applyDraftUpdated(frameFrom('thread-a', 'conn-desktop'));

    expect(reloadFromBackend).toHaveBeenCalledWith('thread-a');
  });

  // Every save this client makes comes back as a frame. Re-reading on it
  // would replace the composer's live text with a round-tripped copy of
  // itself, mid-keystroke.
  it('ignores the echo of a write this client made', async () => {
    const { reloadFromBackend } = await paneWithDraft('thread-a');

    applyDraftUpdated(frameFrom('thread-a', getConnectionId()));

    expect(reloadFromBackend).not.toHaveBeenCalled();
  });

  // Suppression is keyed on the connection, not the device: two tabs of one
  // browser share a device id, and keying on that would leave each sitting
  // on the other's stale text forever.
  it('does not suppress a sibling tab that shares this device', async () => {
    const { reloadFromBackend } = await paneWithDraft('thread-a');

    applyDraftUpdated({
      threadId: 'thread-a',
      updatedAt: 1,
      deviceId: 'this-very-device',
      connectionId: 'conn-other-tab',
    });

    expect(reloadFromBackend).toHaveBeenCalledWith('thread-a');
  });

  // The remote write is not the last write — this client's pending save is,
  // and it lands on the next debounce tick. Adopting the remote text here
  // would delete characters out from under someone still typing them.
  it('leaves the composer alone while this client holds unsaved work', async () => {
    const { reloadFromBackend } = await paneWithDraft('thread-a');
    rememberDraftSnapshot('thread-a', {
      content: 'half a sentence',
      attachments: [],
      terminalChips: [],
      sourceProposedPlan: null,
    });

    applyDraftUpdated(frameFrom('thread-a', 'conn-desktop'));

    expect(reloadFromBackend).not.toHaveBeenCalled();
  });

  it('ignores a frame for a thread no pane is showing', async () => {
    const { reloadFromBackend } = await paneWithDraft('thread-a');

    applyDraftUpdated(frameFrom('thread-b', 'conn-desktop'));

    expect(reloadFromBackend).not.toHaveBeenCalled();
  });

  // A backend write — a saga restoring a draft, a queue dispatch consuming
  // one — has no screen to credit, and every client applies it.
  it('applies an unattributed frame', async () => {
    const { reloadFromBackend } = await paneWithDraft('thread-a');

    applyDraftUpdated({ threadId: 'thread-a', updatedAt: 1 });

    expect(reloadFromBackend).toHaveBeenCalledWith('thread-a');
  });

  it('converges every pane showing the thread', async () => {
    const first = await paneWithDraft('thread-a');
    const second = await paneWithDraft('thread-a', 'second');

    applyDraftUpdated(frameFrom('thread-a', 'conn-desktop'));

    expect(first.reloadFromBackend).toHaveBeenCalledWith('thread-a');
    expect(second.reloadFromBackend).toHaveBeenCalledWith('thread-a');
  });

  it('ignores a malformed or empty frame', async () => {
    const { reloadFromBackend } = await paneWithDraft('thread-a');

    applyDraftUpdated(undefined);
    applyDraftUpdated({ threadId: '', updatedAt: 1 });

    expect(reloadFromBackend).not.toHaveBeenCalled();
  });
});
