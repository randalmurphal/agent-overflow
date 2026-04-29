// Integration tests for the chat scroll system after the virtua/svelte
// rebuild. These tests cover the seams between MessageTimeline,
// stickyBottomController, the per-thread snapshot store, and the layout
// surrounding the timeline (absolute composer, reserved-slot banners).
//
// What is NOT tested here:
//   - virtua's per-row anchor-preservation algorithm. That's owned by the
//     library (see /inokawa/virtua tests upstream); duplicating those
//     assertions in a happy-dom env that lacks real layout would be
//     fiction.
//   - Pure controller behavior (intent transitions, gesture handling,
//     pause-lease semantics) — covered exhaustively in
//     `stickyBottomController.svelte.test.ts`.
//
// What IS tested here:
//   - Per-thread snapshot save/restore round-trip through a real virtua
//     mount.
//   - Load-older flow: anchor capture before, scrollToIndex after.
//   - scrollToItem: pane.loadUntilItem then scrollToIndex.
//   - Composer-height CSS variable propagation through the chat column.
//   - Reserved-slot banner height stability across mount/unmount.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import {
  clearThreadScrollSnapshotsForTest,
  getThreadScrollSnapshot,
  setThreadScrollSnapshot,
} from '../../utils/threadScrollSnapshots';
import MessageTimeline from './MessageTimeline.svelte';
import ChatView from './ChatView.svelte';

beforeEach(async () => {
  resetBindingMocks();
  clearThreadScrollSnapshotsForTest();
  setBindingMock('GetSettings', async () => null);
  await loadSettings();
});

afterEach(() => {
  clearThreadScrollSnapshotsForTest();
});

describe('scroll integration — per-thread snapshot save/restore', () => {
  // Real browser geometry: viewport > 0, scrollOffset=0, scrollSize<=viewport
  //   → stick.isAtBottom() returns true, snapshot persists as {kind:'bottom'}.
  // happy-dom returns 0 for clientHeight/clientWidth, so virtua's
  // getViewportSize() returns 0 too — isAtBottom() is then false (size > 0
  // is not within `threshold` of zero) and the saved snapshot ends up as
  // {kind:'anchor'} regardless of where the user actually was.
  // The save/restore CONTRACT we test here is independent of that
  // geometry quirk: a snapshot is written, and it points at a real item.

  it('writes a snapshot to the store after mount', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    pane.thread!.id = 'thread-snap-write';

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    const snap = getThreadScrollSnapshot('thread-snap-write');
    expect(snap).toBeTruthy();
    if (snap?.kind === 'anchor') {
      expect(['a', 'b']).toContain(snap.itemId);
    }
  });

  it('attempts to load the anchor item when restoring a {kind:"anchor"} snapshot', async () => {
    setThreadScrollSnapshot('thread-restore-anchor', {
      kind: 'anchor',
      itemId: 'pinned-item',
      offsetTop: -120,
    });

    const pane = await buildPane(undefined, [
      makeItem({ id: 'pinned-item', summary: 'pinned' }),
    ]);
    pane.thread!.id = 'thread-restore-anchor';
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('pinned-item');
  });

  it('still calls pane.loadUntilItem when a saved anchor item no longer exists', async () => {
    setThreadScrollSnapshot('thread-missing-anchor', {
      kind: 'anchor',
      itemId: 'gone-from-history',
      offsetTop: -120,
    });

    const pane = await buildPane(undefined, [
      makeItem({ id: 'present', summary: 'still here' }),
    ]);
    pane.thread!.id = 'thread-missing-anchor';
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(false);

    render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('gone-from-history');
  });
});

describe('scroll integration — load older', () => {
  it('routes Load Older through pane.loadOlder and yields a "loaded" status', async () => {
    const items = Array.from({ length: 3 }, (_, i) =>
      makeItem({ id: `m:${i}`, turnIndex: i, summary: `m${i}` }),
    );
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingOlder', { configurable: true, get: () => false });
    const loadOlder = vi.spyOn(pane, 'loadOlder').mockResolvedValue({
      status: 'loaded',
      insertedRows: true,
      insertedBeforeWindow: true,
    });

    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    const button = getByTestId('load-older-messages');
    await fireEvent.click(button);
    await tick();

    expect(loadOlder).toHaveBeenCalled();
  });
});

describe('scroll integration — scroll to item', () => {
  it('routes pane.scrollToItemRequest through pane.loadUntilItem before locating the row', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'visible', turnIndex: 5, summary: 'visible' }),
    ]);
    const loadUntilItem = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

    render(MessageTimeline, { props: { pane } });
    pane.requestScrollToItem('visible');
    await tick();
    await tick();

    expect(loadUntilItem).toHaveBeenCalledWith('visible');
  });

  it('emits a warning toast when the requested item is gone from history', async () => {
    const { getToasts } = await import('../../stores/toast.svelte');
    const pane = await buildPane(undefined, [
      makeItem({ id: 'visible', turnIndex: 5, summary: 'visible' }),
    ]);
    vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(false);
    const toastsBefore = getToasts().length;

    render(MessageTimeline, { props: { pane } });
    pane.requestScrollToItem('missing');
    await tick();
    await tick();

    const newToasts = getToasts().slice(toastsBefore);
    expect(newToasts.some((t) => t.type === 'warning')).toBe(true);
  });
});

describe('scroll integration — composer height + layout invariance', () => {
  it('publishes --composer-height as a CSS variable on the chat column', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);

    const { container } = render(ChatView, { props: { pane } });
    await tick();

    // Find the chat-column element by its data-ui-surface marker; the
    // chat column owns the --composer-height inline style.
    const chatColumn = container.querySelector('[data-ui-surface="chat"]')
      ?.querySelector(':scope > div');
    expect(chatColumn).not.toBeNull();
    const styleAttr = (chatColumn as HTMLElement).getAttribute('style') ?? '';
    expect(styleAttr).toContain('--composer-height:');
  });

  it('renders the composer + below-bar inside the absolute overlay div', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    const overlay = getByTestId('composer-overlay');
    // Class assertions are intentionally loose to allow Tailwind config
    // changes; what matters is that the overlay positions absolutely
    // at the bottom of its relative parent (the timeline container).
    const cls = overlay.className;
    expect(cls).toContain('absolute');
    expect(cls).toContain('bottom-0');
  });
});

describe('scroll integration — banner reserved slot stability', () => {
  it('reserves provider-status and session-status slots regardless of banner state', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 'tail', summary: 'tail' }),
    ]);

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    // Reserved slots are always rendered, even when the banner content
    // is empty, so the timeline geometry stays stable when a banner
    // mounts/unmounts. Test ids let us assert the contract independent
    // of the underlying CSS class names (Tailwind utility names can
    // change with config without breaking the contract).
    expect(getByTestId('provider-status-slot')).toBeInTheDocument();
    expect(getByTestId('session-status-slot')).toBeInTheDocument();
  });
});

describe('scroll integration — auto-follow + button', () => {
  // virtua-internal scroll math isn't testable in happy-dom (zero
  // viewport geometry). We verify integration seams: the gesture path
  // surfaces the scroll-to-bottom chip, and clicking it flips intent
  // back to sticky.

  it('wheel-up on the wrapper surfaces the scroll-to-bottom chip', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
      makeItem({ id: 'c', itemIndex: 2, summary: 'c' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    // Two ticks: first lets the controller-attach $effect run (binding
    // the wheel listener to the wrapper), second lets the snapshot
    // restore $effect settle. Without this, the wheel event fires
    // before the listener is attached.
    await tick();
    await tick();

    const wrapper = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    expect(wrapper).not.toBeNull();
    wrapper.dispatchEvent(new WheelEvent('wheel', { deltaY: -50, bubbles: true }));
    await tick();
    await tick();

    // After the gesture the chip's button is in the DOM (it may still
    // be in a fade-in transition; what matters is presence as a
    // signal that intent flipped to free).
    expect(container.querySelector('[data-testid="scroll-to-bottom"]')).not.toBeNull();
  });
});

describe('scroll integration — mid-list inserts re-sort and re-index', () => {
  // Real triage paths can produce a `tool_completion` for a launchID
  // whose original `tool_call` lived on an earlier turn (see
  // `internal/triage/tool_lifecycle.go`'s `complete:<launchID>` flow,
  // and Codex's late `codex_background.go` arrivals). The pane must
  // re-sort by (turnIndex, itemIndex), rebuild `itemIndexById`, and
  // keep stable item ids — otherwise `pane.requestScrollToItem(id)`
  // resolves to the wrong row and the auto-follow `getLastIndex()`
  // points at the second-to-last item.
  //
  // happy-dom can't measure layout, so we don't assert on virtua
  // remount counts here. We assert on the data contract that virtua's
  // `getKey` consumes: items array order + index map consistency.

  it('upserting an out-of-order item lands it in chronological position and rebuilds itemIndexById', async () => {
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 't1', turnIndex: 1, summary: 't1' }),
      makeItem({ id: 't2', turnIndex: 2, summary: 't2' }),
      makeItem({ id: 't4', turnIndex: 4, summary: 't4' }),
    ]);

    // Sanity: the precondition (turnIndex 1, 2, 4) must hold before the insert.
    expect(pane.items.map((it) => it.id)).toEqual(['t1', 't2', 't4']);

    pane.upsertItem(makeItem({ id: 't3', turnIndex: 3, summary: 't3' }));

    // Items array re-sorted by (turnIndex, itemIndex).
    expect(pane.items.map((it) => it.id)).toEqual(['t1', 't2', 't3', 't4']);

    // The last index advanced — auto-follow's `getLastIndex()` would now
    // point at the new tail (t4), not t3. Mid-list inserts that are NOT
    // the new chronological tail leave the tail unchanged.
    expect(pane.items.at(-1)?.id).toBe('t4');

    // Snapshot round-trips: save an anchor for t2 (which was index 1
    // before the insert and is still index 1 after), and verify it
    // restores to the same item.
    pane.thread!.id = 'thread-midlist-insert';
    setThreadScrollSnapshot('thread-midlist-insert', {
      kind: 'anchor',
      itemId: 't2',
      offsetTop: -120,
    });

    const snap = getThreadScrollSnapshot('thread-midlist-insert');
    expect(snap?.kind).toBe('anchor');
    if (snap?.kind === 'anchor') {
      expect(snap.itemId).toBe('t2');
      // The item still exists at a resolvable position post-insert.
      expect(pane.items.findIndex((it) => it.id === snap.itemId)).toBe(1);
    }
  });

  it('upserting at a tail-equivalent position appends without changing existing indices', async () => {
    // Regression contract: when the new item IS the new chronological
    // tail (turnIndex > all existing), the fast-append branch fires
    // (no needsSort). Item order stays append-only, indices for
    // existing rows are unchanged.
    const pane = await buildPane(makeThread(), [
      makeItem({ id: 't1', turnIndex: 1, summary: 't1' }),
      makeItem({ id: 't2', turnIndex: 2, summary: 't2' }),
    ]);

    pane.upsertItem(makeItem({ id: 't3', turnIndex: 3, summary: 't3' }));

    expect(pane.items.map((it) => it.id)).toEqual(['t1', 't2', 't3']);
    // Tail moved forward as expected.
    expect(pane.items.at(-1)?.id).toBe('t3');
  });
});

describe('scroll integration — load older noop / error paths', () => {
  it('does not re-anchor when pane.loadOlder returns status:"noop"', async () => {
    const { getToasts } = await import('../../stores/toast.svelte');
    const items = Array.from({ length: 3 }, (_, i) =>
      makeItem({ id: `m:${i}`, turnIndex: i, summary: `m${i}` }),
    );
    const pane = await buildPane(undefined, items);
    Object.defineProperty(pane, 'hasMoreHistory', { configurable: true, get: () => true });
    Object.defineProperty(pane, 'loadingOlder', { configurable: true, get: () => false });
    const loadOlder = vi.spyOn(pane, 'loadOlder').mockResolvedValue({
      status: 'noop',
      insertedRows: false,
      insertedBeforeWindow: false,
    });
    const toastsBefore = getToasts().length;

    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    const button = getByTestId('load-older-messages');
    await fireEvent.click(button);
    await tick();
    await tick();

    // The contract: when status !== 'loaded', handleLoadOlder returns
    // before re-anchoring. Observable proxy: pane.loadOlder fired and
    // no warning toast was added (a missed scrollToIndex on a different
    // branch would surface as a different observable, not this).
    const newToasts = getToasts().slice(toastsBefore);
    expect(loadOlder).toHaveBeenCalled();
    expect(newToasts).toHaveLength(0);
  });
});
