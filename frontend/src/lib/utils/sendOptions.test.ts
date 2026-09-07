import { describe, expect, it } from 'vitest';
import { buildSendOptions } from './sendOptions';

describe('buildSendOptions', () => {
  it('carries diff review source and comment ids', () => {
    expect(buildSendOptions({
      attachmentIds: [],
      revisionSourceDiffReview: { threadId: 'thread-1', scope: 'workspace', sourceKey: 'fnv1a:abcd:10' },
      revisionSourceDiffCommentIds: ['comment-1', 'comment-2'],
    })).toEqual({
      attachmentIds: [],
      sendId: expect.any(String),
      reconcileBySendId: true,
      consumeDraft: { content: '', attachmentIds: [], terminalChips: [], sourceProposedPlan: null },
      revisionSourceDiffReview: { threadId: 'thread-1', scope: 'workspace', sourceKey: 'fnv1a:abcd:10' },
      revisionSourceDiffCommentIds: ['comment-1', 'comment-2'],
    });
  });

  it('keeps captured draft consumption separate from revision precedence and later edits', () => {
    const original = { threadId: 'source', itemId: 'original' };
    const revised = { threadId: 'source', itemId: 'revised' };
    const snapshot = { content: 'raw draft', attachments: [], terminalChips: [], sourceProposedPlan: original };
    const options = buildSendOptions({ attachmentIds: [], consumeDraft: snapshot, sourceProposedPlan: original, revisionSourceProposedPlan: revised });
    snapshot.content = 'next draft';
    original.itemId = 'later';
    expect(options.consumeDraft).toEqual({ content: 'raw draft', attachmentIds: [], terminalChips: [], sourceProposedPlan: { threadId: 'source', itemId: 'original' } });
    expect(options.sourceProposedPlan).toBeUndefined();
    expect(options.revisionSourceProposedPlan).toBe(revised);
  });

  // One call is one send. The backend answers a repeat by matching this id
  // against what it already recorded, so two calls sharing one would make
  // two genuinely different messages look like a retry of the first.
  it('mints a fresh, non-empty send id per call', () => {
    const first = buildSendOptions({ attachmentIds: [] });
    const second = buildSendOptions({ attachmentIds: [] });
    expect(first.sendId).not.toBe('');
    expect(first.reconcileBySendId).toBe(true);
    expect(second.reconcileBySendId).toBe(true);
    expect(second.sendId).not.toBe(first.sendId);
  });
});
