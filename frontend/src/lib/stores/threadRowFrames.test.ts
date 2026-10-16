import { describe, expect, it } from 'vitest';
import type { Thread } from '../types/models';
import {
  forgetThreadRowFrames,
  noteThreadPatchFrame,
  noteThreadRowFrame,
  threadReplyRow,
  threadRowFrameMark,
} from './threadRowFrames';

// The sequence is process-wide and only grows, so a mark taken in one test
// is never passed by a frame another test recorded earlier.
function row(id: string, fields: Partial<Thread> = {}): Thread {
  return { id, title: 'reply title', model: 'reply-model', branch: 'reply-branch', ...fields } as Thread;
}

describe('threadReplyRow', () => {
  it('applies the reply when no frame for the thread arrived after the call started', () => {
    noteThreadRowFrame('t-before');
    const mark = threadRowFrameMark();
    noteThreadRowFrame('t-other');
    const reply = row('t-before');
    expect(threadReplyRow(reply, mark, row('t-before', { title: 'held' }))).toBe(reply);
    expect(threadReplyRow(row('t-never'), mark, undefined)).toEqual(row('t-never'));
  });

  it('skips the reply when a whole-row frame arrived after the call started', () => {
    const mark = threadRowFrameMark();
    noteThreadRowFrame('t-row');
    expect(threadReplyRow(row('t-row'), mark, row('t-row', { title: 'frame' }))).toBeNull();
  });

  it('keeps the held value of each field a later patch named', () => {
    const mark = threadRowFrameMark();
    noteThreadPatchFrame('t-patch', { title: 'patched' });
    const held = row('t-patch', { title: 'patched', model: 'held-model', branch: 'held-branch' });
    expect(threadReplyRow(row('t-patch'), mark, held)).toEqual(
      row('t-patch', { title: 'patched', model: 'reply-model', branch: 'reply-branch' }),
    );
  });

  it('does not count an activity-only patch', () => {
    const mark = threadRowFrameMark();
    noteThreadPatchFrame('t-activity', {});
    const reply = row('t-activity');
    expect(threadReplyRow(reply, mark, row('t-activity', { title: 'held' }))).toBe(reply);
  });

  it('forgets a deleted thread', () => {
    const mark = threadRowFrameMark();
    noteThreadRowFrame('t-deleted');
    forgetThreadRowFrames('t-deleted');
    const reply = row('t-deleted');
    expect(threadReplyRow(reply, mark, undefined)).toBe(reply);
  });
});
