// Per-pane find: the provider registry and the find state of each pane.
//
// A pane body registers a provider while it is mounted; `find.in-pane`
// opens the bar of the focused layout pane, and a pane with no provider has
// no find. State (open, query, the focus to return) is keyed by layout pane
// id and outlives a body remount, so a thread switch or a reopened bar keeps
// the query. It is dropped when the pane leaves the layout, whatever body
// it last showed.

import { SvelteMap } from 'svelte/reactivity';
import { onPaneLayoutItemsRemoved } from './paneLayout.svelte';

/** What a provider reports about the current query. */
export interface PaneFindResult {
  /** Matches for the current query; 0 while it is empty. */
  readonly count: number;
  /** Index of the current match, or -1 when there is none. */
  readonly current: number;
  /** True while the provider has not answered the current query. */
  readonly pending: boolean;
  /** True when `count` stops at the provider's limit. */
  readonly capped?: boolean;
}

/**
 * One pane kind's find. `search` receives every query change, the empty
 * query included; a provider debounces and drops superseded answers itself.
 * `next` and `previous` are absent for a provider whose query only filters.
 * `clear` ends the session: highlights, timers and pending reads go.
 */
export interface PaneFindProvider {
  /** Completes the placeholder: "Find in {noun}". */
  readonly noun: string;
  /** Reactive. */
  readonly result: PaneFindResult;
  search(query: string): void;
  next?(): void;
  previous?(): void;
  clear(): void;
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

onPaneLayoutItemsRemoved((paneIds) => {
  for (const paneId of paneIds) states.delete(paneId);
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
}
