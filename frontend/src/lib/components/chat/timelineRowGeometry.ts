import type { Action } from 'svelte/action';
import type { Item } from '../../types/models';
import {
  normalizeTimelineRowGeometryKey,
  type TimelineRowGeometryKey,
} from '../../stores/threadRowUiState.svelte';
import {
  timelineNodeKey,
  type TimelineNode,
} from '../../utils/subagentGrouping';

export const ROW_GEOMETRY_CONTENT_ATTR = 'data-row-geometry-content';

// Bound a stale reservation if the old measured height no longer matches
// the remounted row. 750ms covers the normal image/markdown remount settle
// path without leaving a long-lived blank gap for legitimate shrink cases.
const ROW_GEOMETRY_STALE_RESERVATION_RELEASE_MS = 750;

export interface TimelineRowGeometryReservationParams extends TimelineRowGeometryKey {}

export interface TimelineRowGeometryCache {
  cachedTimelineRowHeight(key: TimelineRowGeometryKey): number | undefined;
  rememberTimelineRowHeight(key: TimelineRowGeometryKey, height: number): void;
}

interface RowReservationState {
  row: HTMLElement;
  content: HTMLElement | null;
  params: TimelineRowGeometryReservationParams | null;
  initialMinHeight: string;
  reservedHeight: number;
  lastMeasuredHeight: number;
  lastMeasuredWidth: number;
  releaseTimer: ReturnType<typeof setTimeout> | null;
}

export function timelineNodeGeometrySignature(
  node: TimelineNode,
  currentLeafItem: Item | null,
  isGroupExpanded: (groupKey: string) => boolean,
  rowShellSignature: string,
): string {
  const prefix = `shell:${rowShellSignature}`;
  if (node.kind === 'leaf') {
    return `${prefix}|leaf:${itemGeometrySignature(currentLeafItem ?? node.item)}`;
  }

  if (node.kind === 'read_group') {
    return [
      prefix,
      'read',
      node.groupKey,
      ...node.members.map(itemGeometrySignature),
    ].join('|');
  }

  if (node.kind === 'wait_group') {
    return [
      prefix,
      'wait',
      node.groupKey,
      itemGeometrySignature(node.parent),
      node.completion ? itemGeometrySignature(node.completion) : '',
      node.descendantCount,
      node.children.length,
      ...node.children.slice(0, 25).map((child) =>
        timelineNodeGeometrySignature(child, null, isGroupExpanded, 'nested-wait-child')),
    ].join('|');
  }

  const expanded = isGroupExpanded(node.groupKey);
  return [
    prefix,
    node.kind,
    node.groupKey,
    expanded ? 'expanded' : 'collapsed',
    itemGeometrySignature(node.parent),
    node.descendantCount,
    node.loadedDescendantCount,
    node.latestChildSummary.length,
  ].join('|');
}

export function createTimelineRowGeometryReservation(
  cache: TimelineRowGeometryCache,
): Action<HTMLElement, TimelineRowGeometryReservationParams> {
  const statesByContent = new WeakMap<Element, RowReservationState>();
  let observer: ResizeObserver | null = null;

  function ensureObserver(): ResizeObserver | null {
    if (observer || typeof ResizeObserver === 'undefined') return observer;
    observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const state = statesByContent.get(entry.target);
        if (!state) continue;
        handleMeasuredHeight(
          state,
          Math.round(entry.contentRect.height),
          Math.round(entry.contentRect.width),
        );
      }
    });
    return observer;
  }

  return (row: HTMLElement, initialParams: TimelineRowGeometryReservationParams) => {
    const state: RowReservationState = {
      row,
      content: null,
      params: null,
      initialMinHeight: row.style.minHeight,
      reservedHeight: 0,
      lastMeasuredHeight: 0,
      lastMeasuredWidth: 0,
      releaseTimer: null,
    };

    bindContentElement(state);
    applyParams(state, initialParams);

    return {
      update(nextParams: TimelineRowGeometryReservationParams) {
        bindContentElement(state);
        applyParams(state, nextParams);
      },
      destroy() {
        clearReservationTimer(state);
        releaseReservation(state, false);
        if (state.content) {
          observer?.unobserve(state.content);
          statesByContent.delete(state.content);
        }
        state.content = null;
      },
    };
  };

  function bindContentElement(state: RowReservationState): void {
    const nextContent = directRowGeometryContent(state.row);
    if (nextContent === state.content) return;

    if (state.content) {
      observer?.unobserve(state.content);
      statesByContent.delete(state.content);
    }

    state.content = nextContent;
    if (!nextContent) return;
    statesByContent.set(nextContent, state);
    ensureObserver()?.observe(nextContent);
  }

  // KEEP THIS FREE OF SYNCHRONOUS LAYOUT READS. applyParams runs on every
  // action update — every reactive param change, which during streaming is
  // many times per second. An earlier strand fix read the row's own width
  // here with a synchronous getBoundingClientRect(); the forced reflow it
  // triggered each update drove a per-frame content-height oscillation, and
  // useStickToBottom's oscillation-snap recovery limit-cycled on it — a
  // sustained ±~16px scroll cycle that showed as the timeline text
  // "vibrating"/flickering, both idle and while streaming. Everything this
  // reservation needs about width/height arrives ASYNCHRONOUSLY via the
  // ResizeObserver (handleMeasuredHeight's contentRect). Read from there —
  // never with a sync layout query (getBoundingClientRect, offsetHeight/Width,
  // getComputedStyle on a laid-out element, scrollHeight) in this path.
  function applyParams(
    state: RowReservationState,
    nextParams: TimelineRowGeometryReservationParams,
  ): void {
    const normalized = normalizeTimelineRowGeometryKey(nextParams);
    if (!normalized) {
      state.params = null;
      clearReservationTimer(state);
      releaseReservation(state, false);
      return;
    }

    const current = state.params;
    if (
      current
      && current.key === normalized.key
      && current.signature === normalized.signature
      && current.width === normalized.width
      && sameOwnerItemIds(current.ownerItemIds, normalized.ownerItemIds)
    ) {
      return;
    }

    state.params = normalized;
    state.lastMeasuredHeight = 0;
    state.lastMeasuredWidth = 0;
    clearReservationTimer(state);

    const cachedHeight = cache.cachedTimelineRowHeight(normalized);
    if (!cachedHeight) {
      releaseReservation(state, false);
      return;
    }

    state.reservedHeight = cachedHeight;
    state.row.style.minHeight = `${cachedHeight}px`;
    state.releaseTimer = setTimeout(() => {
      state.releaseTimer = null;
      releaseReservation(state, true);
    }, ROW_GEOMETRY_STALE_RESERVATION_RELEASE_MS);
  }

  function handleMeasuredHeight(
    state: RowReservationState,
    height: number,
    measuredWidth: number,
  ): void {
    const params = state.params;
    if (!params || height <= 0) return;
    state.lastMeasuredHeight = height;
    if (measuredWidth > 0) state.lastMeasuredWidth = measuredWidth;

    // Hold the reservation while the remounted row is still settling shorter
    // than what we reserved (image / markdown reflow). The applyParams timer
    // is the backstop if it never grows back.
    if (state.reservedHeight > 0 && height < state.reservedHeight) {
      return;
    }

    rememberMeasuredHeight(state);
    if (state.reservedHeight > 0) {
      releaseReservation(state, false);
    }
  }

  // Cache the height under the width the ResizeObserver actually measured it
  // at, NOT params.width. params.width is the surface width threaded through
  // props; it lags by a frame during a column-width reflow, so keying off it
  // caches a tall narrow-layout height under the new wide width — and the next
  // remount at the wide width reserves that too-tall height and strands the
  // timeline above the composer. contentRect.width is atomic with the height
  // just reported, so it is always the width this height is valid for. At a
  // steady width it equals params.width (zero horizontal padding between the
  // scroll surface's content box and data-row-geometry-content), so this only
  // diverges across a reflow.
  function rememberMeasuredHeight(state: RowReservationState): void {
    if (!state.params || state.lastMeasuredHeight <= 0) return;
    const width = state.lastMeasuredWidth > 0 ? state.lastMeasuredWidth : state.params.width;
    cache.rememberTimelineRowHeight({ ...state.params, width }, state.lastMeasuredHeight);
  }

  function releaseReservation(state: RowReservationState, rememberLastMeasured: boolean): void {
    if (state.reservedHeight === 0) return;
    state.row.style.minHeight = state.initialMinHeight;
    state.reservedHeight = 0;
    clearReservationTimer(state);

    if (rememberLastMeasured) {
      rememberMeasuredHeight(state);
    }
  }

  function clearReservationTimer(state: RowReservationState): void {
    if (!state.releaseTimer) return;
    clearTimeout(state.releaseTimer);
    state.releaseTimer = null;
  }
}

export function directRowGeometryContent(row: HTMLElement): HTMLElement | null {
  for (const child of row.children) {
    if (
      child instanceof HTMLElement
      && child.hasAttribute(ROW_GEOMETRY_CONTENT_ATTR)
    ) {
      return child;
    }
  }
  return null;
}

function itemGeometrySignature(item: Item): string {
  const payloadMeta = item.payloadMeta ?? '';
  return [
    item.threadId,
    item.id,
    item.kind,
    item.status,
    item.turnIndex,
    item.itemIndex,
    item.updatedAt,
    item.summary.length,
    item.payloadId ?? '',
    item.payloadKind ?? '',
    payloadMeta.length,
    item.completionOf ?? '',
    item.parentId ?? '',
    item.isBackground === true ? 'bg' : '',
  ].join(':');
}

function sameOwnerItemIds(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  for (let index = 0; index < a.length; index += 1) {
    if (a[index] !== b[index]) return false;
  }
  return true;
}

export function timelineRowGeometryKey(
  node: TimelineNode,
  currentLeafItem: Item | null,
  width: number,
  isGroupExpanded: (groupKey: string) => boolean,
  rowShellSignature: string,
): TimelineRowGeometryKey {
  return {
    key: timelineNodeKey(node),
    signature: timelineNodeGeometrySignature(
      node,
      currentLeafItem,
      isGroupExpanded,
      rowShellSignature,
    ),
    width,
    ownerItemIds: timelineNodeOwnerItemIds(node, currentLeafItem),
  };
}

function timelineNodeOwnerItemIds(node: TimelineNode, currentLeafItem: Item | null): string[] {
  if (node.kind === 'leaf') return [(currentLeafItem ?? node.item).id];
  if (node.kind === 'read_group') return node.members.map((item) => item.id);
  if (node.kind === 'wait_group') {
    return [
      node.parent.id,
      ...(node.completion ? [node.completion.id] : []),
    ];
  }
  return [node.parent.id];
}
