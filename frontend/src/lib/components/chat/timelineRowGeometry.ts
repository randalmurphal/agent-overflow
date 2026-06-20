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
        handleMeasuredHeight(state, Math.round(entry.contentRect.height));
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

  function handleMeasuredHeight(state: RowReservationState, height: number): void {
    const params = state.params;
    if (!params || height <= 0) return;
    state.lastMeasuredHeight = height;

    if (state.reservedHeight > 0 && height < state.reservedHeight) {
      return;
    }

    cache.rememberTimelineRowHeight(params, height);
    if (state.reservedHeight > 0) {
      releaseReservation(state, false);
    }
  }

  function releaseReservation(state: RowReservationState, rememberLastMeasured: boolean): void {
    if (state.reservedHeight === 0) return;
    state.row.style.minHeight = state.initialMinHeight;
    state.reservedHeight = 0;
    clearReservationTimer(state);

    if (rememberLastMeasured && state.params && state.lastMeasuredHeight > 0) {
      cache.rememberTimelineRowHeight(state.params, state.lastMeasuredHeight);
    }
  }

  function clearReservationTimer(state: RowReservationState): void {
    if (!state.releaseTimer) return;
    clearTimeout(state.releaseTimer);
    state.releaseTimer = null;
  }
}

function directRowGeometryContent(row: HTMLElement): HTMLElement | null {
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
