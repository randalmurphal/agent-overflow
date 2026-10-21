import { describe, expect, it } from 'vitest';
import { pullGroupLabel, pullRowQuiet, pullStatePill, sinceYouLookedText } from './pullsAttention';

describe('pullStatePill', () => {
  it('names every state with its tone', () => {
    expect(pullStatePill('your_turn')).toEqual({ label: 'Your turn', tone: 'accent' });
    expect(pullStatePill('pipeline_failed')).toEqual({ label: 'Pipeline failed', tone: 'error' });
    expect(pullStatePill('ready')).toEqual({ label: 'Ready to merge', tone: 'success' });
    expect(pullStatePill('changes_requested')).toEqual({ label: 'Changes requested', tone: 'warning' });
    expect(pullStatePill('waiting_on_author')).toEqual({ label: 'Waiting on author', tone: 'muted' });
    expect(pullStatePill('draft')).toEqual({ label: 'Draft', tone: 'muted' });
    expect(pullStatePill('open')).toEqual({ label: 'Open', tone: 'muted' });
  });

  it('reads an unknown state as open rather than throwing', () => {
    expect(pullStatePill('mystery')).toEqual({ label: 'Open', tone: 'muted' });
  });
});

describe('pullGroupLabel', () => {
  it('names the two groups', () => {
    expect(pullGroupLabel('needs_you')).toBe('Needs you');
    expect(pullGroupLabel('waiting')).toBe('Waiting on others');
  });
});

describe('sinceYouLookedText', () => {
  it('is null when the PR was never opened here', () => {
    expect(sinceYouLookedText(null)).toBeNull();
    expect(sinceYouLookedText(undefined)).toBeNull();
  });

  it('is null when nothing moved', () => {
    expect(sinceYouLookedText({ headMoved: false, newComments: 0, newCommits: 0 })).toBeNull();
  });

  it('counts commits where the forge counts them and says pushed where it does not', () => {
    expect(sinceYouLookedText({ headMoved: true, newComments: 0, newCommits: 2 })).toBe('+2 commits since you looked');
    expect(sinceYouLookedText({ headMoved: true, newComments: 0, newCommits: 1 })).toBe('+1 commit since you looked');
    expect(sinceYouLookedText({ headMoved: true, newComments: 0, newCommits: 0 })).toBe('pushed since you looked');
  });

  it('joins commits and comments', () => {
    expect(sinceYouLookedText({ headMoved: true, newComments: 1, newCommits: 2 })).toBe('+2 commits · 1 new comment since you looked');
    expect(sinceYouLookedText({ headMoved: false, newComments: 3, newCommits: 0 })).toBe('3 new comments since you looked');
  });
});

describe('pullRowQuiet', () => {
  it('dims only a seen row with nothing new', () => {
    expect(pullRowQuiet(true, { headMoved: false, newComments: 0, newCommits: 0 })).toBe(true);
    expect(pullRowQuiet(true, { headMoved: true, newComments: 0, newCommits: 0 })).toBe(false);
    expect(pullRowQuiet(false, null)).toBe(false);
  });
});
