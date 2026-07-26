import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings, updateSetting } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { makeSettings } from '../../../test/helpers/settings';
import { clearThreadScrollSnapshotsForTest } from '../../utils/threadScrollSnapshots';
import type { Item } from '../../types/models';
import MessageTimeline from './MessageTimeline.svelte';

function tool(id: string, index: number, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id,
    itemIndex: index,
    kind: 'tool_call',
    toolName: 'Bash',
    summary: `Bash: ${id}`,
    ...overrides,
  });
}

async function renderRun(items: Item[]) {
  const pane = await buildPane(undefined, items);
  return render(MessageTimeline, { props: { pane } });
}

/**
 * Drains the scroll-to-item flow: the request effect, `loadUntilItem`, and
 * the reveal tick between pointing the run at the item and scrolling to it.
 * The macrotask boundary is what clears the awaits inside it.
 */
async function flushJump(): Promise<void> {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

describe('<ActivityRun>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    clearThreadScrollSnapshotsForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('UpdateSettings', async (patch: unknown) =>
      makeSettings(patch as Parameters<typeof makeSettings>[0]));
    await loadSettings();
  });

  describe('expanded state', () => {
    it('renders its rows inside one capped clip', async () => {
      const { getByTestId, getAllByTestId } = await renderRun([
        tool('t0', 0),
        tool('t1', 1),
        tool('t2', 2),
      ]);

      const clip = getByTestId('activity-run-clip');
      expect(clip.style.maxHeight).toBe('min(50vh, 32rem)');
      // overflow-x hidden is load-bearing: a horizontal bar at run level
      // would consume HEIGHT and shift every row below it.
      expect(clip.className).toContain('overflow-y-auto');
      expect(clip.className).toContain('overflow-x-hidden');
      expect(getAllByTestId('command-output-row')).toHaveLength(3);
      expect(getByTestId('activity-run').getAttribute('data-collapsed')).toBe('false');
    });

    it('mounts only the tail window and offers the rest behind a boundary', async () => {
      await updateSetting('activityRunWindowRows', 10);
      const { getByTestId, getAllByTestId, queryByTestId } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );

      expect(getAllByTestId('command-output-row')).toHaveLength(10);
      expect(getByTestId('activity-run-earlier').textContent).toContain('4 earlier');
      // The window starts on the tail, so nothing is ever hidden below it.
      expect(queryByTestId('activity-run-later')).toBeNull();
    });

    it('mounts an earlier chunk on demand', async () => {
      await updateSetting('activityRunWindowRows', 10);
      const { getByTestId, getAllByTestId, queryByTestId } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );

      await fireEvent.click(getByTestId('activity-run-earlier'));
      await tick();

      expect(getAllByTestId('command-output-row')).toHaveLength(14);
      // Nothing left to reach for, so the boundary retires.
      expect(queryByTestId('activity-run-earlier')).toBeNull();
    });

    it('has no boundary when the whole run fits the window', async () => {
      const { queryByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);

      expect(queryByTestId('activity-run-earlier')).toBeNull();
    });
  });

  describe('collapsing', () => {
    it('collapses from the rail and expands from the chip', async () => {
      const { getByTestId, queryByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);

      await fireEvent.click(getByTestId('activity-run-rail'));
      await tick();

      expect(queryByTestId('activity-run-clip')).toBeNull();
      expect(getByTestId('activity-run').getAttribute('data-collapsed')).toBe('true');
      expect(queryByTestId('command-output-row')).toBeNull();

      await fireEvent.click(getByTestId('activity-run-chip'));
      await tick();

      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
      expect(getByTestId('activity-run').getAttribute('data-collapsed')).toBe('false');
    });

    it('starts collapsed when the setting says so', async () => {
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId, queryByTestId } = await renderRun([tool('t0', 0)]);

      expect(queryByTestId('activity-run-clip')).toBeNull();
      expect(getByTestId('activity-run-chip')).toBeInTheDocument();
    });

    it('keeps the rail in both states so the block stays anchored', async () => {
      const { getByTestId } = await renderRun([tool('t0', 0)]);
      const run = () => getByTestId('activity-run');

      expect(run().getAttribute('data-rail')).toBe('true');
      await fireEvent.click(getByTestId('activity-run-rail'));
      await tick();
      expect(run().getAttribute('data-rail')).toBe('true');
    });
  });

  describe('chip', () => {
    async function collapsedChip(items: Item[]) {
      await updateSetting('activityRunDefault', 'collapsed');
      return renderRun(items);
    }

    it('tallies per tool name', async () => {
      const { getByTestId } = await collapsedChip([
        tool('t0', 0),
        tool('t1', 1),
        tool('t2', 2, { toolName: 'Read', summary: 'Read: a.ts' }),
      ]);

      expect(getByTestId('activity-run-chip-counts').textContent?.trim())
        .toBe('2 Bash, 1 Read');
    });

    it('surfaces a failure it would otherwise hide', async () => {
      const { getByTestId, queryByTestId } = await collapsedChip([
        tool('t0', 0, { status: 'errored' }),
        tool('t1', 1),
      ]);

      expect(queryByTestId('activity-run-chip-failure')).not.toBeNull();
      expect(getByTestId('activity-run-chip-failure').querySelector('[data-state="error"]'))
        .not.toBeNull();
    });

    it('names what is still running', async () => {
      const { getByTestId } = await collapsedChip([
        tool('t0', 0),
        tool('t1', 1, { toolName: 'Grep', status: 'running' }),
      ]);

      expect(getByTestId('activity-run-chip-running').textContent).toContain('Grep');
    });

    it('says nothing about failure or progress on a clean settled run', async () => {
      const { queryByTestId } = await collapsedChip([tool('t0', 0)]);

      expect(queryByTestId('activity-run-chip-failure')).toBeNull();
      expect(queryByTestId('activity-run-chip-running')).toBeNull();
    });

    it('recounts from live items as the run streams', async () => {
      await updateSetting('activityRunDefault', 'collapsed');
      const pane = await buildPane(undefined, [tool('t0', 0, { status: 'running' })]);
      const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

      expect(getByTestId('activity-run-chip-running').textContent).toContain('Bash');

      pane.upsertItem(tool('t0', 0, { status: 'completed', updatedAt: Date.now() + 1 }));
      await tick();

      expect(getByTestId('activity-run-chip-counts').textContent?.trim()).toBe('1 Bash');
      expect(queryByTestId('activity-run-chip-running')).toBeNull();
    });
  });

  describe('jumping to an item inside a run', () => {
    const LONG_RUN = Array.from({ length: 40 }, (_, i) => tool(`t${i}`, i));

    async function jumpTo(itemId: string, items = LONG_RUN) {
      await updateSetting('activityRunWindowRows', 10);
      const pane = await buildPane(undefined, items);
      const view = render(MessageTimeline, { props: { pane } });
      pane.requestScrollToItem(itemId, { flash: true });
      await flushJump();
      return { ...view, pane };
    }

    it('relocates the window around the target, with context either side', async () => {
      const { container, getByTestId } = await jumpTo('t20');

      expect(container.querySelector('[data-item-id="t20"]')).not.toBeNull();
      // Half a window above the hit, so the reader sees what led to it.
      expect(container.querySelector('[data-item-id="t15"]')).not.toBeNull();
      expect(container.querySelector('[data-item-id="t14"]')).toBeNull();
      // Still the same size: a jump moves the window, it does not grow it.
      expect(container.querySelectorAll('[data-testid="command-output-row"]'))
        .toHaveLength(10);
      expect(getByTestId('activity-run-earlier').textContent).toContain('15 earlier');
      expect(getByTestId('activity-run-later').textContent).toContain('15 later');
    });

    it('expands a run whose chip could not show the hit', async () => {
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId, queryByTestId, container } = await jumpTo('t20');

      expect(queryByTestId('activity-run-chip')).toBeNull();
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
      expect(container.querySelector('[data-item-id="t20"]')).not.toBeNull();
    });

    it('leaves the window alone when the target is already mounted', async () => {
      const { getByTestId, queryByTestId } = await jumpTo('t39');

      expect(getByTestId('activity-run-earlier').textContent).toContain('30 earlier');
      expect(queryByTestId('activity-run-later')).toBeNull();
    });

    it('follows new activity again once the reader mounts the rest', async () => {
      const { getByTestId, queryByTestId, container, pane } = await jumpTo('t20');

      await fireEvent.click(getByTestId('activity-run-later'));
      await tick();
      expect(queryByTestId('activity-run-later')).toBeNull();

      pane.upsertItem(tool('t40', 40));
      await tick();

      // Back on the tail: the new row is mounted and nothing is hidden below.
      expect(container.querySelector('[data-item-id="t40"]')).not.toBeNull();
      expect(queryByTestId('activity-run-later')).toBeNull();
    });
  });
});
