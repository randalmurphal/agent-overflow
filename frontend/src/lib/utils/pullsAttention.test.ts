import { describe, expect, it } from 'vitest';
import { pullGroupLabel, pullStateRank, pullStateText, pullStateTone } from './pullsAttention';

describe('pullStateText', () => {
  it('words a turn by what happened', () => {
    expect(pullStateText('your_turn', { kind: 'review_requested', commits: 0, replies: 0 })).toBe('Review requested');
    expect(pullStateText('your_turn', { kind: '', commits: 0, replies: 0 })).toBe('Review requested');
    expect(pullStateText('your_turn', null)).toBe('Review requested');
    expect(pullStateText('your_turn', { kind: 'pushed', commits: 3, replies: 0 })).toBe('+3 commits since your review');
    expect(pullStateText('your_turn', { kind: 'pushed', commits: 1, replies: 0 })).toBe('+1 commit since your review');
    expect(pullStateText('your_turn', { kind: 'pushed', commits: 0, replies: 0 })).toBe('Pushed since your review');
    expect(pullStateText('your_turn', { kind: 'replied', commits: 0, replies: 2 })).toBe('2 replies since your review');
    expect(pullStateText('your_turn', { kind: 'replied', commits: 0, replies: 1 })).toBe('1 reply since your review');
    expect(pullStateText('your_turn', { kind: 'replied', commits: 0, replies: 0 })).toBe('Updated since your review');
  });

  it('names every other state, approved by whether the viewer approved or only reviewed', () => {
    expect(pullStateText('pipeline_failed', null)).toBe('Pipeline failed');
    expect(pullStateText('ready', null)).toBe('Ready to merge');
    expect(pullStateText('changes_requested', null)).toBe('Changes requested');
    expect(pullStateText('waiting_on_author', null)).toBe('Waiting on author');
    expect(pullStateText('approved', { kind: 'approved', commits: 0, replies: 0 })).toBe('Approved by you');
    expect(pullStateText('approved', { kind: 'reviewed', commits: 0, replies: 0 })).toBe('Reviewed by you');
    expect(pullStateText('draft', null)).toBe('Draft');
    expect(pullStateText('open', null)).toBe('Open');
    expect(pullStateText('mystery', null)).toBe('Open');
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
