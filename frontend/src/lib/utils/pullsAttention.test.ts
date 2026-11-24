import { describe, expect, it } from 'vitest';
import { pullGroupLabel, pullStateRank, pullStateText, pullStateTone, type PullReason } from './pullsAttention';

describe('pullStateText', () => {
  // A turn words what happened; every other state is named, approved by
  // whether the viewer approved or only reviewed.
  it.each<[string, PullReason | null, string]>([
    ['your_turn', { kind: 'review_requested', commits: 0, replies: 0 }, 'Review requested'],
    ['your_turn', { kind: '', commits: 0, replies: 0 }, 'Review requested'],
    ['your_turn', null, 'Review requested'],
    ['your_turn', { kind: 'pushed', commits: 3, replies: 0 }, '+3 commits since your review'],
    ['your_turn', { kind: 'pushed', commits: 1, replies: 0 }, '+1 commit since your review'],
    ['your_turn', { kind: 'pushed', commits: 0, replies: 0 }, 'Pushed since your review'],
    ['your_turn', { kind: 'replied', commits: 0, replies: 2 }, '2 replies since your review'],
    ['your_turn', { kind: 'replied', commits: 0, replies: 1 }, '1 reply since your review'],
    ['your_turn', { kind: 'replied', commits: 0, replies: 0 }, 'Author replied'],
    ['pipeline_failed', null, 'Pipeline failed'],
    ['ready', null, 'Ready to merge'],
    ['changes_requested', null, 'Changes requested'],
    ['waiting_on_author', null, 'Waiting on author'],
    ['approved', { kind: 'approved', commits: 0, replies: 0 }, 'Approved by you'],
    ['approved', { kind: 'reviewed', commits: 0, replies: 0 }, 'Reviewed by you'],
    ['draft', null, 'Draft'],
    ['open', null, 'Open'],
    ['mystery', null, 'Open'],
  ])('%s with %j reads %s', (state, reason, want) => {
    expect(pullStateText(state, reason)).toBe(want);
  });
});

describe('pullStateTone', () => {
  it('colours a turn, a failed pipeline, ready and changes requested; the rest are muted', () => {
    expect(pullStateTone('your_turn')).toBe('accent');
    expect(pullStateTone('pipeline_failed')).toBe('error');
    expect(pullStateTone('ready')).toBe('success');
    expect(pullStateTone('changes_requested')).toBe('warning');
    for (const state of ['waiting_on_author', 'approved', 'draft', 'open', 'mystery']) expect(pullStateTone(state)).toBe('muted');
  });
});

describe('pullStateRank', () => {
  it("follows the backend's order and puts an unknown state last", () => {
    expect(pullStateRank('your_turn')).toBeLessThan(pullStateRank('pipeline_failed'));
    expect(pullStateRank('approved')).toBeLessThan(pullStateRank('draft'));
    expect(pullStateRank('open')).toBeLessThan(pullStateRank('mystery'));
  });
});

describe('pullGroupLabel', () => {
  it('names the two groups', () => {
    expect(pullGroupLabel('needs_you')).toBe('Needs you');
    expect(pullGroupLabel('waiting')).toBe('Waiting on others');
  });
});
