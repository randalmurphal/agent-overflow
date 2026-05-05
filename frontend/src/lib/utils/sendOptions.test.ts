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
      revisionSourceDiffReview: { threadId: 'thread-1', scope: 'workspace', sourceKey: 'fnv1a:abcd:10' },
      revisionSourceDiffCommentIds: ['comment-1', 'comment-2'],
    });
  });
});
