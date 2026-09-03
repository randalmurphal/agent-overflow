import { beforeEach, describe, expect, it } from 'vitest';
import type { Attachment } from '../types/attachment';
import {
  cloneDraftSnapshot,
  draftSnapshotMatchesPersistedState,
  forgetDraftSnapshot,
  forgetDraftSnapshotIfMatches,
  getRememberedDraftSnapshot,
  rememberDraftSnapshot,
  resetComposerDraftSnapshotStateForTest,
  trackActiveDraftSave,
  waitForActiveDraftSaves,
  type ComposerDraftSnapshot,
} from './composerDraftSnapshots';

function attachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 10,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
    kind: 'image',
  };
}

function snapshot(overrides: Partial<ComposerDraftSnapshot> = {}): ComposerDraftSnapshot {
  return {
    content: 'hello',
    attachments: [attachment('att-1')],
    terminalChips: [{
      id: 'chip-1',
      label: 'sh',
      preview: '$ ls',
      content: '$ ls\nREADME',
      createdAt: 1,
    }],
    sourceProposedPlan: {
      threadId: 'source-thread',
      itemId: 'plan-1',
      payloadId: 'payload-1',
      title: 'Plan',
    },
    ...overrides,
  };
}

describe('composerDraftSnapshots', () => {
  beforeEach(() => {
    resetComposerDraftSnapshotStateForTest();
  });

  it('clones nested snapshot values', () => {
    const original = snapshot();
    const cloned = cloneDraftSnapshot(original);

    expect(cloned).toEqual(original);
    expect(cloned.attachments[0]).not.toBe(original.attachments[0]);
    expect(cloned.terminalChips[0]).not.toBe(original.terminalChips[0]);
    expect(cloned.sourceProposedPlan).not.toBe(original.sourceProposedPlan);
  });

  it('matches snapshots by persisted draft identity fields', () => {
    const first = snapshot();
    const same = snapshot({
      attachments: [{ ...first.attachments[0], filename: 'renamed.png' }],
    });
    const differentChip = snapshot({
      terminalChips: [{ ...first.terminalChips[0], content: 'different' }],
    });

    expect(draftSnapshotMatchesPersistedState(first, same)).toBe(true);
    expect(draftSnapshotMatchesPersistedState(first, differentChip)).toBe(false);
  });

  it('stores cloned snapshots and evicts the oldest entry', () => {
    const stored = snapshot();
    rememberDraftSnapshot('thread-0', stored);
    stored.attachments[0].filename = 'mutated.png';

    expect(getRememberedDraftSnapshot('thread-0')?.attachments[0].filename).toBe('att-1.png');

    for (let i = 1; i <= 100; i += 1) {
      rememberDraftSnapshot(`thread-${i}`, snapshot({ content: `draft-${i}` }));
    }

    expect(getRememberedDraftSnapshot('thread-0')).toBeUndefined();
    expect(getRememberedDraftSnapshot('thread-100')?.content).toBe('draft-100');
  });

  it('forgets a remembered snapshot', () => {
    rememberDraftSnapshot('thread-1', snapshot());
    forgetDraftSnapshot('thread-1');
    expect(getRememberedDraftSnapshot('thread-1')).toBeUndefined();
  });

  it('forgets a remembered snapshot only when it still matches the saved state', () => {
    const saved = snapshot({ content: 'saved draft' });
    const newer = snapshot({ content: 'newer unsaved draft' });

    rememberDraftSnapshot('thread-1', newer);
    forgetDraftSnapshotIfMatches('thread-1', saved);
    expect(getRememberedDraftSnapshot('thread-1')?.content).toBe('newer unsaved draft');

    forgetDraftSnapshotIfMatches('thread-1', newer);
    expect(getRememberedDraftSnapshot('thread-1')).toBeUndefined();
  });

  it('waits until tracked saves settle', async () => {
    let release!: () => void;
    const save = new Promise<void>((resolve) => {
      release = resolve;
    });
    trackActiveDraftSave('thread-1', save);

    let settled = false;
    const waiting = waitForActiveDraftSaves('thread-1').then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);

    release();
    await waiting;
    expect(settled).toBe(true);
  });
});
