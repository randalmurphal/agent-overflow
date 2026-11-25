import { describe, expect, it } from 'vitest';
import { ticketBranchName, titleSlug } from './ticketBranch';

describe('ticketBranchName', () => {
  it('names the branch by the default template, the key as spelled and the title made a branch name', () => {
    expect(ticketBranchName('', 'WEB-1', 'Retry cursor returns no rows!')).toBe('WEB-1-retry-cursor-returns-no-rows');
    expect(ticketBranchName('feature/{key}', 'WEB-1', 'Retry')).toBe('feature/WEB-1');
    expect(ticketBranchName('{key}-{title}', 'WEB-1', '')).toBe('WEB-1');
  });

  it('cuts a long title at a word within forty characters', () => {
    const slug = titleSlug('The cursor of the sync loop returns no rows after a retry of the read');
    expect(slug).toBe('the-cursor-of-the-sync-loop-returns-no');
    expect(slug.length).toBeLessThanOrEqual(40);
    expect(titleSlug('a'.repeat(50))).toBe('a'.repeat(40));
  });
});
