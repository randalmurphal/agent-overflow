// The BEHAVIOUR half of the 2026-08-29 preview-card fix: folding the card's
// position and text into one nullable `previewCard` derived must leave the
// live card pixel-identical and must tear it down cleanly when the anchor
// dies. It drives the real dying frame — the tick list replaced and the
// available height changed in one task, while the wheel drops the hover.
//
// What this test is NOT: a reproduction of the crash class. It passes against
// a deliberately un-fixed MessageNavRail, and that is expected rather than a
// weakness in the staging. Svelte's ordinary batched flush is parent-first, so
// the `{#if}` destroys the branch before any expression inside it re-runs; four
// shapes were tried (this interleaving, a guard on a separate signal, a nested
// `flushSync` from a user effect, a mid-pass write — the last refused outright
// by svelte's `state_unsafe_mutation`) and none throws. Exposing the class
// takes a tree-order violation, which only a nested `flushSync` inside
// `update_effect` or the `flush-caps` patch's mid-pass ABORT produces. The
// class guard is therefore the source rule in `lib/nullableGuardTotality.test.ts`;
// do not read a pass here as evidence that a branch is total.
//
// Real Chromium on purpose: happy-dom never delivers a ResizeObserver
// height, so `availableHeightPx` stays 0 there and `railTop` never
// invalidates independently — the whole point of the interleaving.
import { describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { Item } from '../../types/models';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type { ThreadPane } from '../../stores/thread.svelte';
import MessageNavRail from './MessageNavRail.svelte';

function item(partial: Partial<Item>): Item {
  return {
    id: 'i',
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'user_text',
    role: 'user',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...partial,
  } as Item;
}

function leaf(it: Item): TimelineNode {
  return { kind: 'leaf', item: it } as TimelineNode;
}

/** Six turns, each an ask plus its reply, so the card has both lines. */
function turnItems(count: number): Item[] {
  const items: Item[] = [];
  for (let i = 0; i < count; i++) {
    items.push(item({ id: `u${i}`, summary: `ask number ${i}`, turnIndex: i }));
    items.push(
      item({
        id: `a${i}`,
        kind: 'assistant_text',
        role: 'assistant',
        summary: `reply number ${i}`,
        turnIndex: i,
        itemIndex: 1,
      }),
    );
  }
  return items;
}

function makePane(items: Item[]): ThreadPane {
  const pane = {
    threadId: 't1',
    switchGeneration: 0,
    hasMoreHistory: false,
    hasMoreNewer: false,
    items,
    getItemById: (id: string) => items.find((it) => it.id === id),
  } as unknown as ThreadPane;
  return pane;
}

function frames(n: number): Promise<void> {
  return new Promise((resolve) => {
    let left = n;
    const step = () => (left-- > 0 ? requestAnimationFrame(step) : resolve());
    requestAnimationFrame(step);
  });
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Window-level error sink: an effect that throws during a microtask flush
 *  surfaces here, not as a rejected promise from the test's own await. */
function captureErrors(): { errors: string[]; stop: () => void } {
  const errors: string[] = [];
  // Chromium reports its own benign "loop completed with undelivered
  // notifications" through window.onerror whenever an RO callback writes
  // state that resizes again. It is not a thrown expression.
  const keep = (text: string) => !text.includes('ResizeObserver loop');
  const push = (text: string) => {
    if (keep(text)) errors.push(text);
  };
  const onError = (e: ErrorEvent) => push(String(e.error ?? e.message));
  const onRejection = (e: PromiseRejectionEvent) => push(String(e.reason));
  window.addEventListener('error', onError);
  window.addEventListener('unhandledrejection', onRejection);
  return {
    errors,
    stop: () => {
      window.removeEventListener('error', onError);
      window.removeEventListener('unhandledrejection', onRejection);
    },
  };
}

describe('message nav rail preview totality', () => {
  it('survives the anchor dying in the same flush that invalidates the rail top', async () => {
    setBindingMock('GetThreadUserMessageTicks', async () => []);
    const items = turnItems(6);
    const nodes = items.map(leaf);
    const { getByTestId, queryByTestId, rerender } = render(MessageNavRail, {
      props: {
        pane: makePane(items),
        nodes,
        getListRef: () => undefined,
        onJumpToItem: () => {},
      },
    });

    const strip = getByTestId('nav-rail-strip');
    // The rail's height is RO-fed; nothing about `railTop` is live until the
    // observer's first delivery lands.
    await frames(3);
    expect(strip.style.height, 'the ResizeObserver must have fed a real rail height')
      .not.toBe('0px');

    // Raise the preview: one dwell through the entry grace, then the card.
    await fireEvent.mouseMove(strip);
    await sleep(180);
    await tick();
    const card = getByTestId('nav-rail-preview');
    expect(card.textContent).toContain('ask number 0');
    // Both halves of the fold reach the DOM: the position still composes
    // `railTop` with the anchor's y, and the edge-flip translate survives.
    // A refactor that drops one half of `previewCard` fails here.
    expect(card.style.top, 'top must still fold railTop with the anchor offset').toContain('calc(');
    expect(card.style.transform, 'the edge-flip translate must survive the fold')
      .toMatch(/translateY\(-?[\d.]+%\)/);

    const sink = captureErrors();
    try {
      // The dying frame, all in ONE task so every write lands in one flush:
      // the composer inset changes the container height (RO → railTop), the
      // tick list is replaced (ticks → railTop AND the anchor's index), and
      // the wheel drops the hover (→ previewAnchor null). Every input the
      // folded derived reads moves at once.
      document.documentElement.style.setProperty('--composer-height', '220px');
      void rerender({
        pane: makePane(items),
        nodes: items.slice(0, 8).map(leaf),
        getListRef: () => undefined,
        onJumpToItem: () => {},
      });
      strip.dispatchEvent(new WheelEvent('wheel', { bubbles: true }));

      await tick();
      await frames(4);
      await tick();

      expect(sink.errors, 'no expression may throw while the anchor dies').toEqual([]);
      expect(queryByTestId('nav-rail-preview'), 'the card must be gone with its anchor')
        .toBeNull();

      // A pointer leave after the teardown leaves nothing behind either.
      await fireEvent.mouseLeave(strip);
      await frames(2);
      expect(queryByTestId('nav-rail-preview')).toBeNull();
      expect(document.querySelectorAll('[role="tooltip"]').length).toBe(0);
      expect(sink.errors).toEqual([]);
    } finally {
      sink.stop();
      document.documentElement.style.removeProperty('--composer-height');
    }
  });
});
