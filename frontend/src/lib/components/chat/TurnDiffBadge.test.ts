import { describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import TurnDiffBadge from './TurnDiffBadge.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function seedThread(id = 'thread-1'): Thread {
  return {
    id,
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    interactionMode: 'default',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function buildPane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(seedThread());
  return pane;
}

describe('<TurnDiffBadge>', () => {
  it('renders the insertion/deletion/file-count line', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(TurnDiffBadge, {
      props: {
        pane,
        turnIndex: 2,
        summary: { insertions: 42, deletions: 17, fileCount: 3 },
      },
    });
    const badge = getByTestId('turn-diff-badge');
    expect(badge.textContent ?? '').toMatch(/\+42/);
    expect(badge.textContent ?? '').toMatch(/−17/);
    expect(badge.textContent ?? '').toMatch(/3 files/);
  });

  it('pluralizes "file" correctly for single file', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(TurnDiffBadge, {
      props: {
        pane,
        turnIndex: 0,
        summary: { insertions: 1, deletions: 0, fileCount: 1 },
      },
    });
    expect(getByTestId('turn-diff-badge').textContent ?? '').toMatch(/1 file(\s|$)/);
  });

  it('omits the deletions segment when zero', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(TurnDiffBadge, {
      props: {
        pane,
        turnIndex: 0,
        summary: { insertions: 5, deletions: 0, fileCount: 1 },
      },
    });
    const text = getByTestId('turn-diff-badge').textContent ?? '';
    expect(text).toMatch(/\+5/);
    expect(text).not.toMatch(/−/);
  });

  it('omits the insertions segment when zero', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(TurnDiffBadge, {
      props: {
        pane,
        turnIndex: 0,
        summary: { insertions: 0, deletions: 4, fileCount: 1 },
      },
    });
    const text = getByTestId('turn-diff-badge').textContent ?? '';
    expect(text).toMatch(/−4/);
    expect(text).not.toMatch(/\+/);
  });

  it('opens the diff panel on the badge turn when clicked', async () => {
    const pane = await buildPane();
    expect(pane.diffPanel.open).toBe(false);
    const { getByTestId } = render(TurnDiffBadge, {
      props: {
        pane,
        turnIndex: 4,
        summary: { insertions: 1, deletions: 1, fileCount: 1 },
      },
    });
    await fireEvent.click(getByTestId('turn-diff-badge'));
    expect(pane.diffPanel.open).toBe(true);
    expect(pane.diffPanel.source).toBe('turn');
    expect(pane.diffPanel.selectedTurnIndex).toBe(4);
  });

  it('carries a descriptive aria-label for screen readers', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(TurnDiffBadge, {
      props: {
        pane,
        turnIndex: 2,
        summary: { insertions: 10, deletions: 3, fileCount: 2 },
      },
    });
    const badge = getByTestId('turn-diff-badge');
    // turn indices are 0-based in the data; users see them 1-based.
    expect(badge.getAttribute('aria-label') ?? '').toMatch(/turn 3/i);
    expect(badge.getAttribute('aria-label') ?? '').toMatch(/10 insertions/);
    expect(badge.getAttribute('aria-label') ?? '').toMatch(/3 deletions/);
    expect(badge.getAttribute('aria-label') ?? '').toMatch(/2 files/);
  });
});
