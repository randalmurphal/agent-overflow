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

  it('shows +0 -0 when no status has been observed yet', () => {
    const { getByTestId } = render(WorkspaceDiffBadge, {
      props: { ...baseProps, status: null },
    });
    const counts = getByTestId('workspace-diff-counts');
    expect(counts.textContent).toContain('+0');
    expect(counts.textContent).toContain('-0');
  });

  it('shows +0 -0 when the workspace is not a git repo', () => {
    const { getByTestId } = render(WorkspaceDiffBadge, {
      props: { ...baseProps, status: status({ isRepo: false }) },
    });
    const counts = getByTestId('workspace-diff-counts');
    expect(counts.textContent).toContain('+0');
    expect(counts.textContent).toContain('-0');
  });

  it('renders +insertions and -deletions for a dirty repo', () => {
    const { getByTestId } = render(WorkspaceDiffBadge, {
      props: { ...baseProps, status: status({ insertions: 12, deletions: 3 }) },
    });
    const counts = getByTestId('workspace-diff-counts');
    expect(counts.textContent).toContain('+12');
    expect(counts.textContent).toContain('-3');
  });

  it('renders +0 -0 for a clean repo (visible even with no changes)', () => {
    const { getByTestId } = render(WorkspaceDiffBadge, {
      props: { ...baseProps, status: status({ insertions: 0, deletions: 0 }) },
    });
    const counts = getByTestId('workspace-diff-counts');
    expect(counts.textContent).toContain('+0');
    expect(counts.textContent).toContain('-0');
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
