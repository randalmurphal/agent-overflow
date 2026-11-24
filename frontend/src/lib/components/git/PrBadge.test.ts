import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import PrBadge from './PrBadge.svelte';
import type { GitStatus } from '../../types/git';
import { setBindingMock } from '../../../test/mocks/bindings-app';

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
  let originalOpen: typeof window.open;

  beforeEach(() => {
    originalOpen = window.open;
    window.open = vi.fn();
  });

  afterEach(() => {
    window.open = originalOpen;
  });

  it.each([
    ['renders nothing when there is no open PR url', status({ openPrUrl: '' })],
    ['renders nothing for a null status', null],
    ['renders nothing when the url is not a valid http(s) url (defensive)', status({ openPrUrl: 'javascript:alert(1)', openPrNumber: 1 })],
  ])('%s', (_name, observed) => {
    const { queryByTestId } = render(PrBadge, { props: { status: observed } });
    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
  });

  it.each([
    ['renders PR #<n> with the GitHub url for a github forge', 'github', 'https://github.com/o/r/pull/123', 123, 'PR #123'],
    ['renders MR !<n> with the GitLab sigil for a gitlab forge', 'gitlab', 'https://gitlab.com/o/r/-/merge_requests/45', 45, 'MR !45'],
    ['falls back to the bare noun when no PR number is present', 'github', 'https://github.com/o/r/pull/9', undefined, 'PR'],
  ] as const)('%s', (_name, forge, url, number, label) => {
    const { getByTestId } = render(PrBadge, {
      props: { status: status({ forge, openPrUrl: url, openPrNumber: number }) },
    });
    const link = getByTestId('chat-header-pr-badge');
    expect(link.textContent?.trim()).toBe(label);
    expect(link.getAttribute('href')).toBe(url);
  });

  it('plain click opens the review pane and suppresses the external link', () => {
    let opened = 0;
    const { getByTestId } = render(PrBadge, {
      props: {
        status: status({ forge: 'gitlab', openPrUrl: 'https://gitlab.com/o/r/-/merge_requests/45', openPrNumber: 45 }),
        onOpenReview: () => {
          opened += 1;
          return true;
        },
      },
    });
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    getByTestId('chat-header-pr-badge').dispatchEvent(event);
    expect(opened).toBe(1);
    expect(event.defaultPrevented).toBe(true);
  });

  it('mod+click opens the forge page in the system browser, not the companion browser', () => {
    // The app-wide delegate turns an unclaimed mod+click into a companion
    // browser page; the badge claims the gesture first so the SYSTEM
    // browser opens (handleExternalURL directly, default prevented so the
    // delegate never sees it).
    let opened = 0;
    const openExternal = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const { getByTestId } = render(PrBadge, {
      props: {
        status: status({ forge: 'gitlab', openPrUrl: 'https://gitlab.com/o/r/-/merge_requests/45', openPrNumber: 45 }),
        onOpenReview: () => {
          opened += 1;
          return true;
        },
      },
    });
    const event = new MouseEvent('click', { bubbles: true, cancelable: true, ctrlKey: true });
    getByTestId('chat-header-pr-badge').dispatchEvent(event);
    expect(opened).toBe(0);
    expect(event.defaultPrevented).toBe(true);
    const viaBinding = (openExternal as ReturnType<typeof vi.fn>).mock.calls;
    const viaWindow = (window.open as ReturnType<typeof vi.fn>).mock.calls;
    expect([...viaBinding, ...viaWindow].map((call) => call[0])).toEqual([
      'https://gitlab.com/o/r/-/merge_requests/45',
    ]);
  });

  it('hover text explains both gestures when a review target exists', () => {
    const { getByTestId } = render(PrBadge, {
      props: {
        status: status({ forge: 'gitlab', openPrUrl: 'https://gitlab.com/o/r/-/merge_requests/45', openPrNumber: 45 }),
        onOpenReview: () => true,
      },
    });
    const title = getByTestId('chat-header-pr-badge').getAttribute('title') ?? '';
    expect(title).toContain('review pane');
    expect(title).toMatch(/(Ctrl|⌘)\+click opens the merge request in your browser/);
  });

  it('falls through to the external link when review reports unhandled', () => {
    const { getByTestId } = render(PrBadge, {
      props: {
        status: status({ forge: 'gitlab', openPrUrl: 'https://gitlab.com/o/r/-/merge_requests/45', openPrNumber: 45 }),
        onOpenReview: () => false,
      },
    });
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    getByTestId('chat-header-pr-badge').dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});
