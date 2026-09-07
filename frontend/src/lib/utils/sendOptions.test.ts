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
      revisionSourceDiffReview: { threadId: 'thread-1', scope: 'workspace', sourceKey: 'fnv1a:abcd:10' },
      revisionSourceDiffCommentIds: ['comment-1', 'comment-2'],
    });
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
