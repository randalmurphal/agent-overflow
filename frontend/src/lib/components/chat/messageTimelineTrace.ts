// Diagnostic UI-render-trace helper for MessageTimeline. Pulled out of
// the component so the orchestrator stays focused on layout and scroll
// integration. Production builds short-circuit at the
// `isUiRenderTraceEnabled()` check, so this module is dev-only payload.

import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineSource } from '../../stores/threadPaneRoles';
import { transformTranslateY } from './tailSlide';
import { timelineNodeTurnIndex, type TimelineNode } from '../../utils/subagentGrouping';
import {
  isUiOracleTraceEnabled,
  isUiRenderTraceEnabled,
  recordUiTrace,
  scheduleDomUiTrace,
} from '../../utils/uiRenderTrace';

const MAX_TRACE_NODES = 120;
const MAX_TRACE_DOM_ROWS = 64;

// Both row probes below observe `[data-row-index]` rows and only care about
// rows being ADDED or REMOVED, never about mutations *inside* an already-tracked
// row. `mutationTargetIsInsideRow` lets each MutationObserver skip the per-node
// `querySelectorAll` sweep for the constant stream of text mutations a streaming
// row emits — without it, every chunk triggers a full row re-scan (the
// optimization is documented at MessageTimeline.svelte's row-resize $effect).
// Shared by both probes so they can't drift.
const ROW_SELECTOR = '[data-row-index]';

// Every probe below keys per-element state on WeakMaps, never Maps. The
// `mutationTargetIsInsideRow` guard means removals that happen INSIDE a row
// (ActivityRun's child windowing evicting [data-run-child] subtrees) never
// reach the probes' untrack paths — a strong Map there pinned every evicted
// reasoning body plus its whole subtree for the pane's lifetime (13.5k
// detached nodes in a day-long dev session, 2026-08-22 heap snapshot).
// ResizeObserver holds its targets weakly per spec, so WeakMap keys are the
// only strong ref a probe could add; keeping them weak makes a missed
// untrack cost nothing. The scan test in messageTimelineTrace.test.ts
// enforces this for new probes.
const mutationTargetIsInsideRow = (target: Node): boolean => {
  if (target instanceof Element) {
    return target.closest(ROW_SELECTOR) !== null;
  }
  return target.parentElement?.closest(ROW_SELECTOR) !== null;
};

function visitRows(node: Node, visit: (row: Element) => void): void {
  if (!(node instanceof Element)) return;
  if (node.matches(ROW_SELECTOR)) visit(node);
  node.querySelectorAll(ROW_SELECTOR).forEach(visit);
}

/**
 * Delay only a diagnostic observer's mandatory first delivery. Rows can mount
 * inside the virtualizer's own ResizeObserver callback. Observing them from
 * the MutationObserver microtask in that same rendering update gives Chromium
 * a new shallower notification after its depth pass, which emits a loop error.
 * These probes use the first delivery only as a baseline, so next-frame
 * registration loses no diagnostic event. WeakRefs preserve the probe rule
 * that a missed removal must never retain detached row DOM.
 */
function createDeferredRowTracker(
  root: Element,
  track: (row: Element) => void,
  untrack: (row: Element) => void,
): {
  added(node: Node): void;
  removed(node: Node): void;
  dispose(): void;
} {
  let pending: WeakRef<Element>[] = [];
  const queued = new WeakSet<Element>();
  let frame: number | undefined;

  const queue = (row: Element) => {
    if (queued.has(row)) return;
    queued.add(row);
    pending.push(new WeakRef(row));
    frame ??= requestAnimationFrame(() => {
      frame = undefined;
      const current = pending;
      pending = [];
      for (const ref of current) {
        const candidate = ref.deref();
        if (!candidate || !queued.has(candidate)) continue;
        queued.delete(candidate);
        if (candidate.isConnected && root.contains(candidate)) track(candidate);
      }
    });
  };

  const remove = (row: Element) => {
    queued.delete(row);
    untrack(row);
  };

  return {
    added: (node) => visitRows(node, queue),
    removed: (node) => visitRows(node, remove),
    dispose() {
      if (frame !== undefined) cancelAnimationFrame(frame);
      frame = undefined;
      pending = [];
    },
  };
}

export function recordTimelineRenderTrace(
  pane: TimelineSource,
  groupedNodes: readonly TimelineNode[],
  scrollEl: HTMLElement | undefined,
  listRef: TimelineVirtualizerHandle | undefined,
): void {
  if (!isUiRenderTraceEnabled()) return;
  recordUiTrace('timeline.state', {
    threadId: pane.threadId,
    itemCount: pane.items.length,
    timelineRevision: pane.timelineRevision,
    groupedNodeCount: groupedNodes.length,
    // Per-node thread ids are redundant with the top-level `threadId`
    // and omitting them shaves ~60 bytes per node (`itemThreadId` is a
    // 38-char UUID repeated for every leaf). Cross-thread inline rows
    // are the only case where the per-node thread id would differ — we
    // accept losing that distinction in the trace to keep the
    // post-shrink retention budget intact. `orphan` is omitted when
    // false (the common case) for the same reason.
    nodes: groupedNodes.slice(0, MAX_TRACE_NODES).map((node) => {
      if (node.kind === 'leaf') {
        const out: Record<string, unknown> = {
          kind: 'leaf',
          itemId: node.item.id,
          itemKind: node.item.kind,
          turnIndex: node.item.turnIndex,
        };
        if (node.orphan === true) out.orphan = true;
        return out;
      }
      if (node.kind === 'group') {
        return {
          kind: 'group',
          parentId: node.parent.id,
          childCount: node.children.length,
          turnIndex: node.parent.turnIndex,
        };
      }
      if (node.kind === 'wait_group') {
        return {
          kind: 'wait_group',
          parentId: node.parent.id,
          childCount: node.children.length,
          turnIndex: node.parent.turnIndex,
        };
      }
      if (node.kind === 'read_group') {
        return {
          kind: 'read_group',
          groupKey: node.groupKey,
          memberCount: node.members.length,
          turnIndex: node.members[0]?.turnIndex ?? 0,
        };
      }
      if (node.kind === 'activity_run') {
        return {
          kind: 'activity_run',
          runId: node.runId,
          childCount: node.children.length,
          itemCount: node.memberItemIds.length,
          mountedFrom: node.mountedFrom,
          mountedRows: node.mountedRows,
          collapsed: node.collapsed,
          // Both halves of the run's follow ownership, so a trace can answer
          // "who owned the position" without a DOM inspect — the 2026-08-19
          // jump was diagnosed from a bookmark that could not.
          live: node.live,
          atTail: node.atTail,
          turnIndex: timelineNodeTurnIndex(node),
        };
      }
      const _exhaustive: never = node;
      return _exhaustive;
    }),
    // The items array used to live here. It dominated trace file bytes
    // (single timeline.state snapshot averaged ~63 KB on a 228-item
    // thread, burning ~57% of the 10 MB rotation cap on data that
    // barely changes between consecutive emissions). The DOM trace
    // (timeline.dom, scheduled below) captures rendered row identity and
    // scroll geometry; the kind/turn information is in `nodes` above.
  });
  scheduleDomUiTrace('timeline', 'timeline.dom', () => ({
    threadId: pane.threadId,
    rowCount: scrollEl?.querySelectorAll('[data-item-id]').length ?? 0,
    rows: Array.from(scrollEl?.querySelectorAll<HTMLElement>('[data-item-id]') ?? [])
      .slice(0, MAX_TRACE_DOM_ROWS)
      .map((el) => ({
        itemId: el.dataset.itemId ?? '',
      })),
    scrollOffset: listRef ? Math.round(listRef.getScrollOffset()) : 0,
    scrollSize: listRef ? Math.round(listRef.getScrollSize()) : 0,
    viewportSize: listRef ? Math.round(listRef.getViewportSize()) : 0,
  }));
}

export function startTimelineRowResizeTrace(root: Element): () => void {
  if (!isUiOracleTraceEnabled()) return () => {};
  if (typeof ResizeObserver === 'undefined') return () => {};

  const trackedHeights = new WeakMap<Element, number | null>();

  const resizeObserver = new ResizeObserver((entries) => {
    for (const entry of entries) {
      const row = entry.target;
      if (!row.isConnected) {
        untrackElement(row);
        continue;
      }
      const prevHeight = trackedHeights.get(row);
      if (prevHeight === undefined) continue;
      const newHeight = Math.round(entry.contentRect.height);
      if (prevHeight === null) {
        trackedHeights.set(row, newHeight);
        continue;
      }
      if (newHeight === prevHeight) continue;
      trackedHeights.set(row, newHeight);
      const itemEl = row.querySelector<HTMLElement>('[data-item-id]');
      recordUiTrace('timeline.row.resize', {
        rowIndex: (row as HTMLElement).dataset.rowIndex ?? '',
        itemId: itemEl?.dataset.itemId ?? '',
        prevHeight,
        newHeight,
        delta: newHeight - prevHeight,
      });
    }
  });

  const trackElement = (el: Element) => {
    if (trackedHeights.has(el)) return;
    trackedHeights.set(el, null);
    resizeObserver.observe(el);
  };

  const untrackElement = (el: Element) => {
    if (!trackedHeights.has(el)) return;
    resizeObserver.unobserve(el);
    trackedHeights.delete(el);
  };

  root.querySelectorAll(ROW_SELECTOR).forEach(trackElement);
  const deferredRows = createDeferredRowTracker(root, trackElement, untrackElement);

  const mo = new MutationObserver((mutations) => {
    for (const m of mutations) {
      if (mutationTargetIsInsideRow(m.target)) continue;
      m.addedNodes.forEach(deferredRows.added);
      m.removedNodes.forEach(deferredRows.removed);
    }
  });
  mo.observe(root, {
    childList: true,
    subtree: true,
  });

  return () => {
    mo.disconnect();
    deferredRows.dispose();
    resizeObserver.disconnect();
  };
}

// --- Row margin-divergence probe (settle-flicker regression oracle) ----
//
// STANDING MONITOR for the settle-flicker bug class, root-caused and FIXED in
// the virtua era; the physics is identical under the bespoke virtualizer.
// The bug: a row's trailing bottom margin collapsed OUT of its `[data-row-index]`
// content box (every ancestor was plain — `.px-6` adds no vertical padding,
// nothing formed a BFC) and was trapped only by the row wrapper's
// `contain: layout style` (its own formatting context — VirtualRow.svelte).
// The wrapper counted the escaped margin in its measured total while the
// row's content-box ResizeObserver did not, so the two disagreed during
// streaming reflow → oscillation → scrollTop clamp → `spring.oscillationSnap`
// (the visible flicker). The fix makes each row its own BFC so the margin is
// contained and the two measurements agree:
//   app.css `[data-row-geometry-content] { display: flow-root }`.
//
// This probe observes BOTH the wrapper and its inner row (content-box, no forced
// layout) and emits `timeline.margin.diverge` only when a frame moves the
// wrapper by a DIFFERENT amount than the row — the escaped-margin signature.
// With the fix in place it must stay SILENT; ANY emission is a regression (a new
// wrapper chain that re-opened the collapse-out path), and the dumped edge
// margins name the offending level. Dev-only.
//
// Full analysis: docs/architecture/settle-flicker-analysis.md
const MARGIN_DIVERGENCE_MIN_PX = 4;

function pxToNum(value: string): number {
  const n = Number.parseFloat(value);
  return Number.isFinite(n) ? Math.round(n) : 0;
}

// Walk the first- or last-child chain from `el` down to the deepest leaf,
// recording each level's tag + vertical margins. A margin that collapses OUT
// of the row can sit at any level of the edge chain (the row's own `mt-4`, a
// markdown block, the footer), and CSS collapses the chain to its max — so the
// whole chain is captured rather than guessing the level. Capped so a
// pathological tree can't bloat the record.
const EDGE_CHAIN_MAX_DEPTH = 14;
function edgeChain(el: Element, edge: 'first' | 'last'): Array<Record<string, unknown>> {
  const chain: Array<Record<string, unknown>> = [];
  let cur: Element | null = edge === 'first' ? el.firstElementChild : el.lastElementChild;
  for (let depth = 0; cur && depth < EDGE_CHAIN_MAX_DEPTH; depth++) {
    const cs = getComputedStyle(cur);
    chain.push({
      t: cur.tagName.toLowerCase(),
      c: (cur.getAttribute('class') ?? '').split(/\s+/).slice(0, 2).join(' '),
      mt: pxToNum(cs.marginTop),
      mb: pxToNum(cs.marginBottom),
    });
    cur = edge === 'first' ? cur.firstElementChild : cur.lastElementChild;
  }
  return chain;
}

export function startRowMarginDivergenceTrace(root: Element): () => void {
  if (!isUiOracleTraceEnabled()) return () => {};
  if (typeof ResizeObserver === 'undefined') return () => {};

  const wrapperToRow = new WeakMap<Element, HTMLElement>();
  const rowToWrapper = new WeakMap<Element, Element>();
  const lastHeight = new WeakMap<Element, number>();

  const ro = new ResizeObserver((entries) => {
    // Build this frame's changed-height set first so a wrapper entry can ask
    // whether its row also changed in the same batch (RO coalesces per frame).
    const changed = new Map<Element, number>();
    for (const entry of entries) {
      if (!entry.target.isConnected) continue;
      changed.set(entry.target, Math.round(entry.contentRect.height));
    }
    for (const [el, h] of changed) {
      const row = wrapperToRow.get(el);
      if (!row) continue; // not a wrapper (it's a row entry) — handled via its wrapper
      const prevWrapperH = lastHeight.get(el);
      const wrapperDelta = prevWrapperH === undefined ? 0 : h - prevWrapperH;
      const rowPrev = lastHeight.get(row);
      const rowNow = changed.get(row);
      const rowDelta = rowNow === undefined || rowPrev === undefined ? 0 : rowNow - rowPrev;
      if (
        prevWrapperH !== undefined &&
        Math.abs(wrapperDelta - rowDelta) >= MARGIN_DIVERGENCE_MIN_PX
      ) {
        const csRow = getComputedStyle(row);
        recordUiTrace('timeline.margin.diverge', {
          rowIndex: row.dataset.rowIndex ?? '',
          itemId: row.querySelector<HTMLElement>('[data-item-id]')?.dataset.itemId ?? '',
          wrapperDelta,
          rowDelta,
          diverge: wrapperDelta - rowDelta,
          // The row's own margin (e.g. mt-4) is trapped by the wrapper's
          // `contain: layout` but excluded from the row's content box.
          rowMt: pxToNum(csRow.marginTop),
          rowMb: pxToNum(csRow.marginBottom),
          // If a divergence recurs, the offending margin sits on one of these
          // edge chains — the level whose mb/mt is no longer contained by the
          // row BFC names the wrapper that re-opened the collapse-out path.
          firstChain: edgeChain(row, 'first'),
          lastChain: edgeChain(row, 'last'),
        });
      }
    }
    for (const [el, h] of changed) lastHeight.set(el, h);
  });

  const track = (row: Element) => {
    if (!(row instanceof HTMLElement)) return;
    const wrapper = row.parentElement;
    if (!wrapper || rowToWrapper.has(row)) return;
    wrapperToRow.set(wrapper, row);
    rowToWrapper.set(row, wrapper);
    ro.observe(wrapper);
    ro.observe(row);
  };
  const untrack = (row: Element) => {
    const wrapper = rowToWrapper.get(row);
    if (!wrapper) return;
    ro.unobserve(wrapper);
    ro.unobserve(row);
    wrapperToRow.delete(wrapper);
    rowToWrapper.delete(row);
    lastHeight.delete(wrapper);
    lastHeight.delete(row);
  };

  root.querySelectorAll(ROW_SELECTOR).forEach(track);
  const deferredRows = createDeferredRowTracker(root, track, untrack);

  const mo = new MutationObserver((mutations) => {
    for (const m of mutations) {
      if (mutationTargetIsInsideRow(m.target)) continue;
      m.addedNodes.forEach(deferredRows.added);
      m.removedNodes.forEach(deferredRows.removed);
    }
  });
  mo.observe(root, { childList: true, subtree: true });

  return () => {
    mo.disconnect();
    deferredRows.dispose();
    ro.disconnect();
  };
}

// --- Reasoning-tail jump probe (stale-pin regression oracle) ---------------
//
// STANDING MONITOR for the streaming-thinking flicker, root-caused and FIXED.
// The bug: the reasoning-tail body (TailClampedText, shared by ThinkingBlock
// and CompactionReasoning) kept its collapsed 3-line window bottom-pinned with
// an imperative `$effect: scrollTop = scrollHeight` whose only dependency was
// `text`. A mid-stream WIDTH change (the scroll-spring width-reflow oscillation
// — same strand as commit a5a5d032) re-wrapped the `whitespace-pre-wrap` body
// and grew its content height with NO text delta to re-run the pin, so the
// newest line scrolled out of the clamped window until the next delta snapped
// it back — the user-visible stutter at spring start. The fix replaces the
// imperative pin with a CSS flex bottom-anchor (`justify-content: flex-end`)
// the layout engine re-evaluates on every reflow, width re-wraps included:
//   TailClampedText.svelte (collapsed body).
//
// This probe tracks each reasoning-tail body and, ONLY when a frame re-wraps it
// (width changed) with NO accompanying text delta — the exact stale-pin
// trigger — does one bounded geometry read to check whether the newest glyph
// fell below the clamped viewport. With the fix it must stay SILENT; any
// `timeline.thinking.tailJump` emission means the bottom-anchor regressed (or,
// run against the pre-fix build, confirms the trigger fires live). The
// width-change-without-text gate keeps the forced read off the per-delta
// streaming path. Dev-only.
//
// Full analysis: docs/architecture/settle-flicker-analysis.md
// These testids must stay in sync with ReasoningTailRow's `${idPrefix}-body`
// (idPrefix 'thinking' / 'compaction-reasoning'). The oracle is silent=healthy,
// so a rename would make it track nothing and go permanently dark with no
// failing test — messageTimelineTrace.test.ts renders both rows and asserts
// this selector still matches (the drift guard). Exported for that test.
export const REASONING_TAIL_SELECTOR =
  '[data-testid="thinking-body"], [data-testid="compaction-reasoning-body"]';
const TAIL_JUMP_MIN_PX = 2;

// Pixels the newest glyph sits BELOW the bottom edge of the clamped window.
// A forced read — callers gate it to the rare re-wrap-without-delta frame.
// Descends to the deepest trailing leaf: TailClampedText renders `text`
// inside an inner slide wrapper (the line-slide FLIP's transform target), so
// `body.lastChild` is an element, not the text node. Client rects include
// that wrapper's in-flight transform, and a re-wrap can land mid-slide (the
// component clears the slide on a width change, but only when the RO
// delivers, after this read) — subtract its live translateY so the oracle
// measures the LAYOUT
// position and a slide can't read as a stale-pin jump.
// (Twin of `lastCharRect`/`tailOverflowPx` in tailClampedText.browser.test.ts;
// intentionally separate — the test keeps sub-pixel precision, this rounds
// and thresholds for a dev-trace payload.)
function tailOverflowPx(body: HTMLElement): number {
  let last: Node | null = body.lastChild;
  while (last?.lastChild) last = last.lastChild;
  if (!last) return 0;
  const len = last.textContent?.length ?? 0;
  const range = document.createRange();
  if (last.nodeType === Node.TEXT_NODE && len > 0) {
    range.setStart(last, len - 1);
    range.setEnd(last, len);
  } else {
    range.selectNodeContents(last);
  }
  const wrapper = body.firstElementChild;
  const slideTy = wrapper ? transformTranslateY(getComputedStyle(wrapper).transform) : 0;
  return Math.round(
    range.getBoundingClientRect().bottom - slideTy - body.getBoundingClientRect().bottom,
  );
}

export function startReasoningTailJumpTrace(root: Element): () => void {
  if (!isUiOracleTraceEnabled()) return () => {};
  if (typeof ResizeObserver === 'undefined') return () => {};

  // Per body: last seen content width + text length, or null until the first
  // RO callback seeds the baseline. A resize whose width changed but whose text
  // length did NOT is a re-wrap with no delta — the stale-pin trigger.
  // WeakMap is load-bearing here, not defensive: bodies inside an
  // ActivityRun's mount window are evicted by in-row mutations the observer
  // below deliberately skips, so `untrack` never runs for them (see the
  // guard comment below).
  const seen = new WeakMap<Element, { width: number; textLen: number } | null>();

  const ro = new ResizeObserver((entries) => {
    for (const entry of entries) {
      const body = entry.target;
      if (!(body instanceof HTMLElement) || !body.isConnected) continue;
      const width = Math.round(entry.contentRect.width);
      const textLen = body.textContent?.length ?? 0;
      const prev = seen.get(body);
      seen.set(body, { width, textLen });
      if (!prev) continue; // first callback seeds the baseline
      if (width === prev.width) continue; // not a re-wrap
      if (textLen !== prev.textLen) continue; // carried a text delta — normal scroll path
      const overflow = tailOverflowPx(body); // bounded forced read, gated above
      if (overflow > TAIL_JUMP_MIN_PX) {
        recordUiTrace('timeline.reasoning.tailJump', {
          // `itemId` matches the sibling probes' correlation key so a tailJump
          // can be joined against this row's `timeline.row.resize` /
          // `timeline.margin.diverge`. `bodyId` keeps the `${idPrefix}-` prefix
          // so the reasoning kind (thinking vs compaction) stays visible.
          itemId:
            body.closest('[data-row-index]')?.querySelector<HTMLElement>('[data-item-id]')?.dataset
              .itemId ?? '',
          bodyId: body.id || '',
          prevWidth: prev.width,
          width,
          tailOverflowPx: overflow,
        });
      }
    }
  });

  const track = (el: Element) => {
    if (!(el instanceof HTMLElement) || seen.has(el)) return;
    seen.set(el, null); // baseline pending — first RO callback fills it
    ro.observe(el);
  };
  const untrack = (el: Element) => {
    if (!seen.has(el)) return;
    ro.unobserve(el);
    seen.delete(el);
  };

  root.querySelectorAll(REASONING_TAIL_SELECTOR).forEach(track);

  const mo = new MutationObserver((mutations) => {
    for (const m of mutations) {
      // Skip the per-node rescan for mutations INSIDE an already-tracked row —
      // the same guard both sibling probes use (streaming markdown churns child
      // lists every chunk). This is a DELIBERATE coverage trade, not a safety
      // claim: ActivityRun's child windowing mounts and evicts reasoning
      // bodies through in-row mutations this skip never sees. Bodies paged in
      // that way are historical (no text deltas coming), so the stale-pin
      // trigger this oracle watches for cannot fire on them — the live
      // streaming body always mounts with its row at the plane level, which
      // this observer does see. The evictions the skip misses are why `seen`
      // MUST stay a WeakMap: with a strong Map, every skipped eviction pinned
      // the body's whole subtree (the 2026-08-22 detached-DOM leak).
      if (mutationTargetIsInsideRow(m.target)) continue;
      m.addedNodes.forEach((n) => {
        if (!(n instanceof Element)) return;
        if (n.matches(REASONING_TAIL_SELECTOR)) track(n);
        n.querySelectorAll?.(REASONING_TAIL_SELECTOR).forEach(track);
      });
      m.removedNodes.forEach((n) => {
        if (!(n instanceof Element)) return;
        if (n.matches(REASONING_TAIL_SELECTOR)) untrack(n);
        n.querySelectorAll?.(REASONING_TAIL_SELECTOR).forEach(untrack);
      });
    }
  });
  mo.observe(root, { childList: true, subtree: true });

  return () => {
    mo.disconnect();
    ro.disconnect();
  };
}
