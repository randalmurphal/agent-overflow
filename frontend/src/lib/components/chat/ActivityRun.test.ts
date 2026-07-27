import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings, updateSetting } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { makeSettings } from '../../../test/helpers/settings';
import { clearThreadScrollSnapshotsForTest } from '../../utils/threadScrollSnapshots';
import { ACTIVITY_RUN_CAP_CSS } from '../../utils/activityRunClip';
import type { Item } from '../../types/models';
import MessageTimeline from './MessageTimeline.svelte';

/**
 * Prose closes a run: the next activity row starts a new one, so the run can
 * never grow again. Liveness is what decides whether a COLLAPSED run keeps its
 * clip open, so a test about the header-only state has to say which it means.
 */
function finished(items: Item[]): Item[] {
  return [
    ...items,
    makeItem({
      id: 'done',
      itemIndex: items.length,
      kind: 'assistant_text',
      summary: 'done',
    }),
  ];
}

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
  return { ...render(MessageTimeline, { props: { pane } }), pane };
}

/**
 * happy-dom lays nothing out, so a clip states the geometry the scroll-driven
 * paths read back. Every one of them asks the same three numbers, which is why
 * they travel together: a scrollTop with no scrollHeight beside it describes a
 * surface that cannot scroll, and the triggers would correctly refuse it.
 */
function stampScroll(
  clip: HTMLElement,
  metrics: { scrollTop: number; clientHeight: number; scrollHeight: number },
): void {
  for (const [key, value] of Object.entries(metrics)) {
    Object.defineProperty(clip, key, { value, configurable: true, writable: true });
  }
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
      expect(clip.style.maxHeight).toBe(ACTIVITY_RUN_CAP_CSS);
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

    it('pages the earlier chunk in on scrolling to the top of the window', async () => {
      await updateSetting('activityRunWindowRows', 10);
      const { getByTestId, getAllByTestId, queryByTestId } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );
      const clip = getByTestId('activity-run-clip');

      stampScroll(clip, { scrollTop: 0, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.wheel(clip, { deltaY: -120 });
      await fireEvent.scroll(clip);
      await tick();

      // Browsing back through a long run is one continuous scroll; reaching
      // the top must not put a button between the reader and the next rows.
      expect(getAllByTestId('command-output-row')).toHaveLength(14);
      expect(queryByTestId('activity-run-earlier')).toBeNull();
    });

    it('does not page on a position the run wrote itself', async () => {
      // The run writes its own position on mount, after a prepend, and on a
      // jump — and each write dispatches a `scroll` event. Reading those as
      // the reader arriving at the top is how expanding a run used to page
      // backwards through its history and strand the reader up there: the
      // mount write aims at `scrollHeight`, but nothing inside is measured
      // yet, so it lands in the trigger zone. Intent is the gesture, never the
      // geometry it produced.
      await updateSetting('activityRunWindowRows', 10);
      const { getByTestId, getAllByTestId } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );
      const clip = getByTestId('activity-run-clip');

      stampScroll(clip, { scrollTop: 0, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.scroll(clip);
      await tick();

      expect(getAllByTestId('command-output-row')).toHaveLength(10);
      expect(getByTestId('activity-run-earlier').textContent).toContain('4 earlier');
    });

    it('stops trusting a gesture once the run answers it', async () => {
      // The arming has to be consumed by the paging it caused, or one wheel
      // would license every later self-written position — including the
      // compensation write the paging itself performs.
      await updateSetting('activityRunWindowRows', 10);
      // One chunk past the window still leaves rows hidden above, so there is
      // a second page for an unarmed scroll to wrongly pull in.
      const { getByTestId, getAllByTestId } = await renderRun(
        Array.from({ length: 40 }, (_, i) => tool(`t${i}`, i)),
      );
      const clip = getByTestId('activity-run-clip');

      stampScroll(clip, { scrollTop: 0, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.wheel(clip, { deltaY: -120 });
      await fireEvent.scroll(clip);
      await tick();
      expect(getAllByTestId('command-output-row')).toHaveLength(35);

      // No new gesture: the rest stays behind the boundary.
      await fireEvent.scroll(clip);
      await tick();
      expect(getAllByTestId('command-output-row')).toHaveLength(35);
      expect(getByTestId('activity-run-earlier').textContent).toContain('5 earlier');
    });

    it('leaves the window alone while the reader is mid-scroll', async () => {
      await updateSetting('activityRunWindowRows', 10);
      const { getByTestId, getAllByTestId } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );
      const clip = getByTestId('activity-run-clip');

      stampScroll(clip, { scrollTop: 900, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.scroll(clip);
      await tick();

      expect(getAllByTestId('command-output-row')).toHaveLength(10);
      expect(getByTestId('activity-run-earlier').textContent).toContain('4 earlier');
    });

    it('holds the window a reader never scrolled, whatever the setting', async () => {
      // A window that fits under the cap rests at a scrollTop inside the
      // trigger zone. Paging in there would ignore the setting entirely.
      await updateSetting('activityRunWindowRows', 10);
      const { getByTestId, getAllByTestId } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );
      const clip = getByTestId('activity-run-clip');

      stampScroll(clip, { scrollTop: 0, clientHeight: 300, scrollHeight: 300 });
      await fireEvent.scroll(clip);
      await tick();

      expect(getAllByTestId('command-output-row')).toHaveLength(10);
    });

    it('fades the top edge only while activity is passing under it', async () => {
      const { getByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);
      const clip = getByTestId('activity-run-clip');
      const fade = getByTestId('activity-run-top-fade');

      // Resting at its first row there is nothing above to dissolve, and
      // tinting that row would just make it look dimmer than the rest.
      expect(fade.getAttribute('data-faded')).toBe('false');
      expect(fade.className).toContain('opacity-0');

      stampScroll(clip, { scrollTop: 120, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.scroll(clip);

      expect(fade.getAttribute('data-faded')).toBe('true');
      expect(fade.className).not.toContain('opacity-0');
    });

    it('has no boundary when the whole run fits the window', async () => {
      const { queryByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);

      expect(queryByTestId('activity-run-earlier')).toBeNull();
    });

    it('is live when it holds the timeline tail', async () => {
      const { getByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);

      expect(getByTestId('activity-run').dataset.live).toBe('true');
    });

    it('is not live when prose closed it, even as the last run in the list', async () => {
      // Prose after a run closes it: the next activity row starts a NEW run,
      // so this one can never grow again. A settled turn usually ends
      // `[…, activity_run, assistant_text]`, so scanning back past the prose
      // would hand nearly every thread's last run a spring, an observer set,
      // and intent listeners it can never use.
      const { getByTestId } = await renderRun([
        tool('t0', 0),
        tool('t1', 1),
        makeItem({ id: 'p0', itemIndex: 2, kind: 'assistant_text', summary: 'done' }),
      ]);

      expect(getByTestId('activity-run').dataset.live).toBe('false');
    });
  });

  describe('collapsing', () => {
    it('keeps its header while expanded, and collapses from it', async () => {
      // The header is the run's own control in BOTH directions. It used to
      // render only while collapsed, so expanding a run removed the very thing
      // the reader had just clicked and left the invisible rail strip as the
      // only way back.
      const { getByTestId, queryByTestId } = await renderRun(
        finished([tool('t0', 0), tool('t1', 1)]),
      );
      const header = getByTestId('activity-run-header');
      expect(getByTestId('activity-run').dataset.collapsed).toBe('false');
      // Present while open, and saying so — the chevron and the screen reader
      // read the same attribute.
      expect(header.getAttribute('aria-expanded')).toBe('true');
      expect(header.getAttribute('aria-label')).toContain('Collapse');

      await fireEvent.click(header);
      await tick();

      expect(getByTestId('activity-run').dataset.collapsed).toBe('true');
      expect(queryByTestId('activity-run-clip')).toBeNull();
      // Same element, still there, now offering the other direction.
      expect(getByTestId('activity-run-header')).toBe(header);
      expect(header.getAttribute('aria-expanded')).toBe('false');
      expect(header.getAttribute('aria-label')).toContain('Expand');

      await fireEvent.click(header);
      await tick();

      expect(getByTestId('activity-run').dataset.collapsed).toBe('false');
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
    });

    it('collapses from the rail and expands from the header', async () => {
      // Finished, so collapsing means header ONLY. A run still holding the tail
      // keeps its clip under the header — see "still working" below.
      const { getByTestId, queryByTestId } = await renderRun(
        finished([tool('t0', 0), tool('t1', 1)]),
      );

      await fireEvent.click(getByTestId('activity-run-rail'));
      await tick();

      expect(queryByTestId('activity-run-clip')).toBeNull();
      expect(getByTestId('activity-run').getAttribute('data-collapsed')).toBe('true');
      expect(queryByTestId('command-output-row')).toBeNull();

      await fireEvent.click(getByTestId('activity-run-header'));
      await tick();

      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
      expect(getByTestId('activity-run').getAttribute('data-collapsed')).toBe('false');
    });

    it('collapses from the rail inside the viewport-bottom transaction', async () => {
      // The rail is a height change the reader asked for, so it opens upward
      // over the rows above rather than moving the rows below them. Which way
      // it lands is real geometry (activityRunScroll.browser.test.ts); that the
      // rail routes through the transaction at all is asserted here.
      const { getByTestId, pane } = await renderRun([tool('t0', 0), tool('t1', 1)]);
      const controller = pane.scrollController;
      if (!controller?.preserveViewportBottom) throw new Error('timeline published no controller');
      const held = vi.spyOn(controller, 'preserveViewportBottom');

      await fireEvent.click(getByTestId('activity-run-rail'));
      await tick();

      expect(held).toHaveBeenCalledTimes(1);
      expect(getByTestId('activity-run').getAttribute('data-collapsed')).toBe('true');
    });

    it('keeps no reading position across a collapse', async () => {
      // Collapsing replaced the inside of the run, so there is nothing left to
      // restore and the clip takes the never-scrolled path — its newest row,
      // which is the reason the run is on screen at all. That the write lands
      // at the bottom needs real geometry; happy-dom reports zero for both
      // sides of it, so the landing is asserted in
      // activityRunScroll.browser.test.ts and the state is asserted here.
      const { getByTestId, pane } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );
      const clip = getByTestId('activity-run-clip');
      const id = getByTestId('activity-run').getAttribute('data-run-id') ?? '';
      stampScroll(clip, { scrollTop: 120, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.scroll(clip);
      expect(pane.activityRuns.scrollSnapshot(id)).not.toBeNull();

      await fireEvent.click(getByTestId('activity-run-rail'));
      await tick();

      expect(pane.activityRuns.scrollSnapshot(id)).toBeNull();

      await fireEvent.click(getByTestId('activity-run-header'));
      await tick();

      // And the reopened clip did not record one on the way back in either.
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
      expect(pane.activityRuns.scrollSnapshot(id)).toBeNull();
    });

    it('names the clip it controls, scoped to the pane', async () => {
      const { getByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);

      const target = getByTestId('activity-run-clip').id;
      // Pane-scoped, because run ids are minted per registry: every pane's
      // first run is `r1`, so an unscoped id collides across panes even on
      // different threads.
      expect(target).toContain('-main-');
      expect(getByTestId('activity-run-header').getAttribute('aria-controls')).toBe(target);

      await fireEvent.click(getByTestId('activity-run-rail'));
      await tick();

      // The header outlives the clip, and must still point at it. Both halves
      // build the id from one derived value, because a `controls` that names a
      // string nothing emits fails silently — it looks right and announces
      // nothing.
      expect(getByTestId('activity-run-header').getAttribute('aria-controls')).toBe(target);
    });

    it('keeps the rail out of the accessibility tree', async () => {
      // It duplicates the header, and an invisible duplicate is a phantom tab
      // stop (a focus ring on a transparent 16px strip) plus the run's state
      // announced twice from two buttons naming one region. Pointer-only: the
      // whole block reads as one thing, so clicking its edge should still fold
      // it. Both attributes, because hiding a focusable element is its own
      // defect.
      const { getByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);
      const rail = getByTestId('activity-run-rail');

      expect(rail.getAttribute('aria-hidden')).toBe('true');
      expect(rail.getAttribute('tabindex')).toBe('-1');
      // And no second `aria-expanded` for the same region.
      expect(rail.hasAttribute('aria-expanded')).toBe(false);
      expect(rail.hasAttribute('aria-controls')).toBe(false);

      await fireEvent.click(rail);
      await tick();

      expect(getByTestId('activity-run').dataset.collapsed).toBe('true');
    });

    it('starts collapsed when the setting says so', async () => {
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId, queryByTestId } = await renderRun(finished([tool('t0', 0)]));

      expect(queryByTestId('activity-run-clip')).toBeNull();
      expect(getByTestId('activity-run-header')).toBeInTheDocument();
    });

    it('renders open while it is still working, whatever the default says', async () => {
      // Collapsing a thread must not mean going blind to what it is doing right
      // now, so a default of `collapsed` describes how a run SITS once it has
      // settled — a live one nobody has answered for renders open and folds
      // itself shut when it finishes.
      //
      // And it renders open HONESTLY: the header reports expanded, because a
      // chevron claiming collapsed over a clip full of streaming rows describes
      // neither what is on screen nor what the next click will do.
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);

      expect(getByTestId('activity-run').dataset.live).toBe('true');
      expect(getByTestId('activity-run').dataset.collapsed).toBe('false');
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
      expect(getByTestId('activity-run-header').getAttribute('aria-expanded')).toBe('true');
    });

    it('collapses a live run when asked, without waiting for it to finish', async () => {
      // The click is an answer about THIS run, and it beats the liveness that
      // decides for runs nobody has answered for. Previously liveness won, so
      // the clip stayed and the only thing the click moved was the chevron —
      // the reader collapsed the run they were watching and nothing closed.
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId, queryByTestId } = await renderRun([tool('t0', 0), tool('t1', 1)]);
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();

      await fireEvent.click(getByTestId('activity-run-header'));
      await tick();

      expect(queryByTestId('activity-run-clip')).toBeNull();
      expect(getByTestId('activity-run').dataset.collapsed).toBe('true');
      // Still the live run — it just is not showing its work any more.
      expect(getByTestId('activity-run').dataset.live).toBe('true');

      // And back, from the same header.
      await fireEvent.click(getByTestId('activity-run-header'));
      await tick();

      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
    });

    it('closes that clip once the run finishes', async () => {
      // happy-dom lays nothing out, so the fold measures a zero-height box and
      // takes its no-motion path — which is the right outcome for a box with
      // no height, and the reason the animation itself is asserted in
      // activityRunFold.browser.test.ts. What is asserted here is that
      // finishing is what closes it.
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId, queryByTestId, pane } = await renderRun([
        tool('t0', 0),
        tool('t1', 1),
      ]);
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();

      pane.upsertItem(makeItem({
        id: 'p0',
        itemIndex: 2,
        kind: 'assistant_text',
        summary: 'done',
      }));
      await tick();
      await tick();

      expect(getByTestId('activity-run').dataset.live).toBe('false');
      expect(queryByTestId('activity-run-clip')).toBeNull();
      expect(getByTestId('activity-run-header')).toBeInTheDocument();
    });

    it('waits out a reader who is inside the run when it finishes', async () => {
      // A fold is owed, not forced. Closing a run over somebody reading inside
      // it is the one thing worse than the jump the fold exists to avoid.
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId, queryByTestId, pane } = await renderRun(
        Array.from({ length: 14 }, (_, i) => tool(`t${i}`, i)),
      );
      const clip = getByTestId('activity-run-clip');

      // A gesture that leaves the clip's newest row: the reader is now in it.
      stampScroll(clip, { scrollTop: 600, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.wheel(clip, { deltaY: -120 });
      await fireEvent.scroll(clip);
      await tick();

      pane.upsertItem(makeItem({
        id: 'p0',
        itemIndex: 20,
        kind: 'assistant_text',
        summary: 'done',
      }));
      await tick();
      await tick();

      expect(getByTestId('activity-run').dataset.live).toBe('false');
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();

      // Back on the newest row, and the debt is paid.
      stampScroll(clip, { scrollTop: 1200, clientHeight: 300, scrollHeight: 1500 });
      await fireEvent.wheel(clip, { deltaY: 120 });
      await fireEvent.scroll(clip);
      await tick();
      await tick();

      expect(queryByTestId('activity-run-clip')).toBeNull();
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

  describe('header summary', () => {
    async function collapsed(items: Item[]) {
      await updateSetting('activityRunDefault', 'collapsed');
      return renderRun(items);
    }

    it('tallies per tool name', async () => {
      const { getByTestId } = await collapsed([
        tool('t0', 0),
        tool('t1', 1),
        tool('t2', 2, { toolName: 'Read', summary: 'Read: a.ts' }),
      ]);

      expect(getByTestId('activity-run-header-counts').textContent?.trim())
        .toBe('2 Bash, 1 Read');
    });

    it('tints each tool name with that tool\'s own icon hue', async () => {
      const { getByTestId } = await collapsed([
        tool('t0', 0),
        tool('t1', 1, { toolName: 'Read', summary: 'Read: a.ts' }),
        makeItem({ id: 't2', itemIndex: 2, kind: 'thinking', summary: 'pondering' }),
      ]);
      const counts = getByTestId('activity-run-header-counts');
      const hue = (label: string) =>
        counts.querySelector(`[data-tool-term="${label}"]`)?.className;

      // The same `--ico-*` tokens the expanded run's icons carry, so the line
      // reads as the block it stands for rather than a grey tally.
      expect(hue('Bash')).toBe('text-ico-terminal');
      expect(hue('Read')).toBe('text-ico-eye');
      // Reasoning has no tool name to classify — it is named by kind.
      expect(hue('thinking')).toBe('text-ico-brain');
      // The counts themselves stay muted: the colour says WHICH tool.
      expect(counts.className).toContain('text-fg-muted');
      expect(counts.textContent?.trim()).toBe('1 Bash, 1 Read, 1 thinking');
    });

    it('leaves an unknown tool the ordinary text colour', async () => {
      // `generic` resolves to the secondary text token, so a tool this build
      // does not know reads as plain text instead of borrowing a hue that
      // means something else.
      const { getByTestId } = await collapsed([
        tool('t0', 0, { toolName: 'DefinitelyNotAKnownTool' }),
      ]);

      expect(
        getByTestId('activity-run-header-counts')
          .querySelector('[data-tool-term]')?.className,
      ).toBe('text-ico-generic');
    });

    it('surfaces a failure it would otherwise hide', async () => {
      const { getByTestId, queryByTestId } = await collapsed([
        tool('t0', 0, { status: 'errored' }),
        tool('t1', 1),
      ]);

      expect(queryByTestId('activity-run-header-failure')).not.toBeNull();
      expect(getByTestId('activity-run-header-failure').querySelector('[data-state="error"]'))
        .not.toBeNull();
    });

    it('names what is still running', async () => {
      const { getByTestId } = await collapsed([
        tool('t0', 0),
        tool('t1', 1, { toolName: 'Grep', status: 'running' }),
      ]);

      expect(getByTestId('activity-run-header-running').textContent).toContain('Grep');
    });

    it('says nothing about failure or progress on a clean settled run', async () => {
      const { queryByTestId } = await collapsed([tool('t0', 0)]);

      expect(queryByTestId('activity-run-header-failure')).toBeNull();
      expect(queryByTestId('activity-run-header-running')).toBeNull();
    });

    it('recounts from live items as the run streams', async () => {
      await updateSetting('activityRunDefault', 'collapsed');
      const pane = await buildPane(undefined, [tool('t0', 0, { status: 'running' })]);
      const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

      expect(getByTestId('activity-run-header-running').textContent).toContain('Bash');

      pane.upsertItem(tool('t0', 0, { status: 'completed', updatedAt: Date.now() + 1 }));
      await tick();

      expect(getByTestId('activity-run-header-counts').textContent?.trim()).toBe('1 Bash');
      expect(queryByTestId('activity-run-header-running')).toBeNull();
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

    it('expands a run whose header could not show the hit', async () => {
      await updateSetting('activityRunDefault', 'collapsed');
      const { getByTestId, container } = await jumpTo('t20');

      // The header stays — it always does — but it now reports an OPEN run,
      // which is the only thing that can put the hit on screen.
      expect(getByTestId('activity-run-header').getAttribute('aria-expanded')).toBe('true');
      expect(getByTestId('activity-run-clip')).toBeInTheDocument();
      expect(container.querySelector('[data-item-id="t20"]')).not.toBeNull();
    });

    it('leaves the window alone when the target is already mounted', async () => {
      const { getByTestId, queryByTestId } = await jumpTo('t39');

      expect(getByTestId('activity-run-earlier').textContent).toContain('30 earlier');
      expect(queryByTestId('activity-run-later')).toBeNull();
    });

    it('holds the jumped-to window while activity arrives, until the reader scrolls back down', async () => {
      const { getByTestId, queryByTestId, container, pane } = await jumpTo('t20');

      await fireEvent.click(getByTestId('activity-run-later'));
      await tick();
      expect(queryByTestId('activity-run-later')).toBeNull();

      pane.upsertItem(tool('t41', 41));
      await tick();

      // A jump escapes bottom-follow deliberately, so the window stops
      // following the tail: new activity collects behind the boundary instead
      // of sliding the rows the reader jumped to up the clip.
      expect(container.querySelector('[data-item-id="t41"]')).toBeNull();
      expect(getByTestId('activity-run-later').textContent).toContain('1 later');
      expect(container.querySelector('[data-item-id="t15"]')).not.toBeNull();

      // Scrolling back down inside the clip re-sticks the controller, which
      // releases the window — the other half of the rule, and the half that
      // would otherwise strand a live run behind a boundary forever.
      //
      // The clip's overflow is stubbed the way OverlayScrollbar.test.ts stubs
      // its scroller: real element, real listeners, measured values supplied.
      // happy-dom reports zero geometry, and the intent machine correctly
      // ignores a wheel on a surface that cannot scroll.
      const clip = getByTestId('activity-run-clip');
      Object.defineProperty(clip, 'clientHeight', { get: () => 400, configurable: true });
      Object.defineProperty(clip, 'scrollHeight', { get: () => 1000, configurable: true });
      clip.scrollTop = 600; // its bottom
      await fireEvent.wheel(clip, { deltaY: 120 });
      await tick();

      expect(container.querySelector('[data-item-id="t41"]')).not.toBeNull();
      expect(queryByTestId('activity-run-later')).toBeNull();
    });

    it('holds a historical run\'s window when it becomes the live one', async () => {
      await updateSetting('activityRunWindowRows', 10);
      const pane = await buildPane(undefined, [
        ...LONG_RUN,
        makeItem({ id: 'p0', itemIndex: 40, kind: 'assistant_text', summary: 'done' }),
      ]);
      const { container, getByTestId, queryByTestId } = render(MessageTimeline, {
        props: { pane },
      });
      pane.requestScrollToItem('t20', { flash: true });
      await flushJump();

      // Prose closed this run, so the jump pinned its window with no
      // controller to record the escape on.
      expect(getByTestId('activity-run').dataset.live).toBe('false');
      expect(container.querySelector('[data-item-id="t20"]')).not.toBeNull();

      // The prose leaves — an interrupt reverts it, a queued item is
      // withdrawn — and the run it closed is now the timeline's tail, so it
      // gets a controller. A fresh controller says "not escaped", and taking
      // that at face value would release the pin and drop the reader at the
      // run's tail without them touching anything.
      pane.removeItemById('p0');
      await tick();

      expect(getByTestId('activity-run').dataset.live).toBe('true');
      expect(container.querySelector('[data-item-id="t20"]')).not.toBeNull();
      expect(container.querySelector('[data-item-id="t15"]')).not.toBeNull();
      expect(getByTestId('activity-run-later').textContent).toContain('15 later');
      expect(getByTestId('activity-run-header').getAttribute('aria-expanded')).toBe('true');
    });
  });
});
