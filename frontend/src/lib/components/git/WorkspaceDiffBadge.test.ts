import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import WorkspaceDiffBadge from './WorkspaceDiffBadge.svelte';
import type { GitStatus } from '../../types/git';

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'main',
    isDefaultBranch: true,
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

const baseProps = { pressed: false, chord: 'Ctrl+Shift+G', onActivate: () => {} };

describe('<WorkspaceDiffBadge>', () => {
  it('always renders the toggle button (testid + tooltip) regardless of status', () => {
    const { getByTestId } = render(WorkspaceDiffBadge, {
      props: { ...baseProps, status: null },
    });
    const btn = getByTestId('review-toggle');
    expect(btn).toBeTruthy();
    expect(btn.getAttribute('title')).toBe('Toggle Review Pane (Ctrl+Shift+G)');
  });

  it.each([
    ['shows +0 -0 when no status has been observed yet', null, '+0', '-0'],
    ['shows +0 -0 when the workspace is not a git repo', status({ isRepo: false }), '+0', '-0'],
    ['renders +insertions and -deletions for a dirty repo', status({ insertions: 12, deletions: 3 }), '+12', '-3'],
    ['renders +0 -0 for a clean repo (visible even with no changes)', status({ insertions: 0, deletions: 0 }), '+0', '-0'],
  ])('%s', (_name, observed, insertions, deletions) => {
    const { getByTestId } = render(WorkspaceDiffBadge, { props: { ...baseProps, status: observed } });
    const counts = getByTestId('workspace-diff-counts');
    expect(counts.textContent).toContain(insertions);
    expect(counts.textContent).toContain(deletions);
  });

  it('reflects the pressed (panel-open) state via aria-pressed', () => {
    const { getByTestId } = render(WorkspaceDiffBadge, {
      props: { ...baseProps, status: status(), pressed: true },
    });
    expect(getByTestId('review-toggle').getAttribute('aria-pressed')).toBe('true');
  });

  it('invokes onActivate when clicked', async () => {
    const onActivate = vi.fn();
    const { getByTestId } = render(WorkspaceDiffBadge, {
      props: { ...baseProps, status: status(), onActivate },
    });
    await fireEvent.click(getByTestId('review-toggle'));
    expect(onActivate).toHaveBeenCalledTimes(1);
  });
});
