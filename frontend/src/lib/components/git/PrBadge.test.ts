import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import PrBadge from './PrBadge.svelte';
import type { GitStatus } from '../../types/git';

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'feature',
    isDefaultBranch: false,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    ...overrides,
  };
}

describe('<PrBadge>', () => {
  it('renders nothing when there is no open PR url', () => {
    const { queryByTestId } = render(PrBadge, { props: { status: status({ openPrUrl: '' }) } });
    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
  });

  it('renders nothing for a null status', () => {
    const { queryByTestId } = render(PrBadge, { props: { status: null } });
    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
  });

  it('renders PR #<n> with the GitHub url for a github forge', () => {
    const { getByTestId } = render(PrBadge, {
      props: {
        status: status({
          forge: 'github',
          openPrUrl: 'https://github.com/o/r/pull/123',
          openPrNumber: 123,
        }),
      },
    });
    const link = getByTestId('chat-header-pr-badge');
    expect(link.textContent?.trim()).toBe('PR #123');
    expect(link.getAttribute('href')).toBe('https://github.com/o/r/pull/123');
  });

  it('renders MR !<n> with the GitLab sigil for a gitlab forge', () => {
    const { getByTestId } = render(PrBadge, {
      props: {
        status: status({
          forge: 'gitlab',
          openPrUrl: 'https://gitlab.com/o/r/-/merge_requests/45',
          openPrNumber: 45,
        }),
      },
    });
    const link = getByTestId('chat-header-pr-badge');
    expect(link.textContent?.trim()).toBe('MR !45');
    expect(link.getAttribute('href')).toBe('https://gitlab.com/o/r/-/merge_requests/45');
  });

  it('falls back to the bare noun when no PR number is present', () => {
    const { getByTestId } = render(PrBadge, {
      props: {
        status: status({
          forge: 'github',
          openPrUrl: 'https://github.com/o/r/pull/9',
          openPrNumber: undefined,
        }),
      },
    });
    expect(getByTestId('chat-header-pr-badge').textContent?.replace(/\s+/g, '')).toBe('PR');
  });

  it('renders nothing when the url is not a valid http(s) url (defensive)', () => {
    const { queryByTestId } = render(PrBadge, {
      props: { status: status({ openPrUrl: 'javascript:alert(1)', openPrNumber: 1 }) },
    });
    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
  });
});
