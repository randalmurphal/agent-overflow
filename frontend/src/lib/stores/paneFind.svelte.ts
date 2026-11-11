// Per-pane find: the provider registry and the find state of each pane.
//
// A pane body registers a provider while it is mounted; `find.in-pane`
// opens the bar of the focused layout pane, and a pane with no provider has
// no find. Everything here is keyed by a find key: a layout pane id, or a
// region key (`paneFindRegionKey`) for a pane whose body declares regions
// (`declarePaneFindRegions`), such as a list beside a detail. Each region
// has its own provider, state and bar; the commands act on the pane's
// target region (`paneFindTarget`): the one on screen when only one is,
// else the one last focused or clicked, else the declared initial one.
// State (open, query, the focus to return) outlives a body remount, so a
// thread switch or a reopened bar keeps the query. It is dropped when the
// pane leaves the layout, whatever body it last showed.

import { SvelteMap } from 'svelte/reactivity';
import { onPaneLayoutItemsRemoved } from './paneLayout.svelte';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { errString } from '../utils/errors';

/** What a provider reports about the current query. */
export interface PaneFindResult {
  /**
   * Matches for the current query; 0 while it is empty. null when the
   * provider found a match but cannot count them.
   */
  readonly count: number | null;
  /** Index of the current match, or -1 when there is none or it is unknown. */
  readonly current: number;
  /** True while the provider has not answered the current query. */
  readonly pending: boolean;
  /** True when `count` stops at the provider's limit. */
  readonly capped?: boolean;
  /** True when `count` grows while `pending`; the bar shows it then. */
  readonly progressive?: boolean;
  /** Why the current query has no answer; the bar shows it. */
  readonly error?: string | null;
}

/**
 * One pane kind's find. `search` receives every query change, the empty
 * query included; a provider debounces and drops superseded answers itself.
 * `next` and `previous` are absent for a provider whose query only filters.
 * `clear` ends the session: highlights, timers and pending reads go.
 * A provider with `openNative` has its own find UI: an open goes there with
 * the seed and no bar opens. That UI closes without telling the provider, so
 * the pane's find never reads as open for it and `find.close` leaves its
 * Escape to the UI; another open seeds it again.
 */
export interface PaneFindProvider {
  /** Completes the placeholder: "Find in {noun}", or "Filter {noun}" for a filter. */
  readonly noun: string;
  /** Reactive. */
  readonly result: PaneFindResult;
  search(query: string): void;
  next?(): void;
  previous?(): void;
  /** A filter's Enter: focus its first result. False when there is none. */
  focusFirst?(): Promise<boolean> | boolean;
  clear(): void;
  openNative?(seed: string): void;
}

/** True for a provider that steps through matches rather than filtering. */
export function isSteppingFind(provider: PaneFindProvider): boolean {
  return provider.next !== undefined || provider.previous !== undefined;
}

interface PaneFindState {
  open: boolean;
  query: string;
  /** Bumped on every open; the bar focuses and selects its input. */
  focusRequest: number;
  /** The element that had focus when the bar opened. */
  returnFocus: HTMLElement | null;
}

export type PaneFindView = Readonly<PaneFindState>;

const providers = new SvelteMap<string, PaneFindProvider>();
const states = new SvelteMap<string, PaneFindState>();

// ---- Regions ----------------------------------------------------------------

const REGION_SEPARATOR = '#';

/** The region named in every list-beside-detail pane. */
export const PANE_FIND_LIST_REGION = 'list';
export const PANE_FIND_DETAIL_REGION = 'detail';

/** The find key of a named region of a layout pane. */
export function paneFindRegionKey(paneId: string, region: string): string {
  return `${paneId}${REGION_SEPARATOR}${region}`;
}

/** The find key a pane's detail region registers, mounts its bar and
 * names its highlights under. */
export function paneFindDetailKey(paneId: string): string {
  return paneFindRegionKey(paneId, PANE_FIND_DETAIL_REGION);
}

export function paneFindListKey(paneId: string): string {
  return paneFindRegionKey(paneId, PANE_FIND_LIST_REGION);
}

/** The layout pane a find key belongs to. */
export function paneOfFindKey(key: string): string {
  const at = key.indexOf(REGION_SEPARATOR);
  return at < 0 ? key : key.slice(0, at);
}

export interface PaneFindRegions {
  /** The region find targets before the reader focused or clicked one. */
  initial: string;
  /** The region on screen when only one is (compact), else null. */
  shown?: () => string | null;
}

interface RegionDeclaration extends PaneFindRegions {
  /** The region last focused or clicked; null until one is. */
  active: string | null;
}

const regions = new SvelteMap<string, RegionDeclaration>();
const regionElements = new Map<string, HTMLElement>();

/** Declares that the pane's find acts per region; returns its release.
 * A later declaration for the same pane replaces this one. */
export function declarePaneFindRegions(paneId: string, declaration: PaneFindRegions): () => void {
  const entry: RegionDeclaration = $state({ ...declaration, active: null });
  regions.set(paneId, entry);
  return () => {
    if (regions.get(paneId) === entry) regions.delete(paneId);
  };
}

/** The reader focused or clicked inside the region `key` names. Ignored
 * for a pane that declares no regions. */
export function activatePaneFindRegion(key: string): void {
  const at = key.indexOf(REGION_SEPARATOR);
  if (at < 0) return;
  const entry = regions.get(key.slice(0, at));
  const region = key.slice(at + 1);
  if (entry && entry.active !== region) entry.active = region;
}

/** Records the region's element, the root the find command reads a seed
 * selection from; returns its release. */
export function setPaneFindRegionElement(key: string, element: HTMLElement): () => void {
  regionElements.set(key, element);
  return () => {
    if (regionElements.get(key) === element) regionElements.delete(key);
  };
}

export function paneFindRegionElement(key: string): HTMLElement | null {
  return regionElements.get(key) ?? null;
}

/** The find key the commands act on for a layout pane: the pane itself,
 * or its target region. Reactive. */
export function paneFindTarget(paneId: string | null): string | null {
  if (paneId === null) return null;
  const entry = regions.get(paneId);
  if (!entry) return paneId;
  const region = entry.shown?.() ?? entry.active ?? entry.initial;
  return paneFindRegionKey(paneId, region);
}

/**
 * A pane's find reads: one in flight, at most one waiting. Keyed by layout
 * pane id rather than by provider, so a read a body left out when it
 * unmounted still holds the pane's slot and a remounted body's provider
 * queues behind it.
 */
export interface PaneFindReads {
  /** Run `read` once the pane has none in flight; it replaces a waiting one. */
  request(read: () => Promise<void>): void;
}

interface ReadSlot {
  busy: boolean;
  waiting: (() => Promise<void>) | null;
}

const readSlots = new Map<string, ReadSlot>();

function runRead(paneId: string, slot: ReadSlot, read: () => Promise<void>): void {
  slot.busy = true;
  let running: Promise<void>;
  try {
    running = read();
  } catch (err) {
    running = Promise.reject(err);
  }
  void running
    .catch((err: unknown) => reportFrontendDiagnostic('pane find read failed', errString(err)))
    .finally(() => {
      slot.busy = false;
      if (readSlots.get(paneId) !== slot) return;
      const next = slot.waiting;
      slot.waiting = null;
      if (next) runRead(paneId, slot, next);
    });
}

export function paneFindReads(paneId: string): PaneFindReads {
  return {
    request(read) {
      let slot = readSlots.get(paneId);
      if (!slot) {
        slot = { busy: false, waiting: null };
        readSlots.set(paneId, slot);
      }
      if (slot.busy) slot.waiting = read;
      else runRead(paneId, slot, read);
    },
  };
}

onPaneLayoutItemsRemoved((paneIds) => {
  const removed = new Set(paneIds);
  for (const key of states.keys()) if (removed.has(paneOfFindKey(key))) states.delete(key);
  for (const key of readSlots.keys()) if (removed.has(paneOfFindKey(key))) readSlots.delete(key);
  for (const paneId of removed) regions.delete(paneId);
});

/** Registers `provider` for the layout pane; returns its release. */
export function registerPaneFindProvider(paneId: string, provider: PaneFindProvider): () => void {
  providers.set(paneId, provider);
  const state = states.get(paneId);
  if (state?.open && state.query) provider.search(state.query);
  return () => {
    if (providers.get(paneId) !== provider) return;
    providers.delete(paneId);
    provider.clear();
  };
}

export function hasPaneFindProvider(paneId: string | null): boolean {
  return paneId !== null && providers.has(paneId);
}

export function getPaneFindProvider(paneId: string): PaneFindProvider | null {
  return providers.get(paneId) ?? null;
}

export function getPaneFind(paneId: string): PaneFindView | null {
  return states.get(paneId) ?? null;
}

export function isPaneFindOpen(paneId: string | null): boolean {
  return paneId !== null && hasPaneFindProvider(paneId) && states.get(paneId)?.open === true;
}

export interface OpenPaneFindOptions {
  /** Replaces the query (the reader's selection). */
  seed?: string;
  /** Focus to return on close; ignored when the bar is already open. */
  returnFocus?: HTMLElement | null;
}

/** Opens or refocuses the pane's bar. False when the pane has no find. */
export function openPaneFind(paneId: string, options: OpenPaneFindOptions = {}): boolean {
  const provider = providers.get(paneId);
  if (!provider) return false;
  if (provider.openNative) {
    provider.openNative(options.seed ?? '');
    return true;
  }
  let state = states.get(paneId);
  if (!state) {
    // Allocated here, outside any reader, so the bar's reads track it.
    const created: PaneFindState = $state({ open: false, query: '', focusRequest: 0, returnFocus: null });
    states.set(paneId, created);
    state = created;
  }
  const wasOpen = state.open;
  const seed = options.seed ?? '';
  const seeded = seed !== '' && seed !== state.query;
  if (seeded) state.query = seed;
  if (!wasOpen) state.returnFocus = options.returnFocus ?? null;
  state.open = true;
  state.focusRequest += 1;
  if ((!wasOpen || seeded) && state.query) provider.search(state.query);
  return true;
}

export function setPaneFindQuery(paneId: string, query: string): void {
  const state = states.get(paneId);
  if (!state?.open || state.query === query) return;
  state.query = query;
  providers.get(paneId)?.search(query);
}

export function stepPaneFind(paneId: string, direction: 1 | -1): void {
  if (!states.get(paneId)?.open) return;
  const provider = providers.get(paneId);
  if (direction === 1) provider?.next?.();
  else provider?.previous?.();
}

/** A filter's Enter: focus its first result. False when it has none. */
export async function focusFirstPaneFindResult(paneId: string): Promise<boolean> {
  if (!states.get(paneId)?.open) return false;
  const provider = providers.get(paneId);
  if (!provider?.focusFirst) return false;
  return provider.focusFirst();
}

/** Closes the bar and clears its highlights. The query stays for a reopen. */
export function closePaneFind(paneId: string): void {
  const state = states.get(paneId);
  if (!state?.open) return;
  state.open = false;
  providers.get(paneId)?.clear();
}

/** Takes the focus the bar should return to after a close. */
export function takePaneFindReturnFocus(paneId: string): HTMLElement | null {
  const state = states.get(paneId);
  if (!state) return null;
  const element = state.returnFocus;
  state.returnFocus = null;
  return element;
}

export function resetPaneFindForTest(): void {
  providers.clear();
  states.clear();
  readSlots.clear();
  regions.clear();
  regionElements.clear();
}
