// Cumulative diff state + refresh wiring for the diff panel. Isolating
// this keeps DiffPanelDrawer.svelte under the 300-line ceiling and
// makes the thread-wide fetch / provider-event subscription logic
// independently unit-testable.

import { onDestroy, onMount } from 'svelte';
import { wailsEventOn } from '../../../stores/events';
import { ListThreadDiffPayloads } from '../../../stores/bindings';
import type { Item } from '../../../types/models';
import {
  selectAgentDiffEntries,
  summarizeEntries,
  type AgentDiffEntry,
  type DiffStats,
} from '../../../utils/diffAggregation';
import { debounce } from '../../../utils/debounce';

const REFRESH_DEBOUNCE_MS = 100;

export interface CumulativeDiffItems {
  readonly items: Item[];
  readonly entries: AgentDiffEntry[];
  readonly stats: DiffStats;
}

/**
 * Thread-wide cumulative diff state, refreshed on thread switch and
 * on provider:item_upsert events for diff-bearing payloads. Callers
 * pass a reactive `getThreadId()` closure so the factory can re-fetch
 * when the pane's thread changes. Every `$state` lives inside the
 * factory — the consuming component just reads the returned getters.
 *
 * Must be invoked during component setup: the factory wires `onMount`
 * + `onDestroy` for the Wails event subscription and debounced
 * refresh. Calling it outside a component context throws.
 */
export function createCumulativeDiffItems(opts: {
  getThreadId: () => string | null;
}): CumulativeDiffItems {
  let items: Item[] = $state([]);
  const entries = $derived<AgentDiffEntry[]>(selectAgentDiffEntries(items));
  const stats = $derived<DiffStats>(summarizeEntries(entries, items));

  let fetchSeq = 0;
  async function refresh(): Promise<void> {
    const id = opts.getThreadId();
    const seq = ++fetchSeq;
    // Explicit null thread (pane cleared, no active thread) IS the
    // signal to wipe — the old data would belong to a thread the
    // user isn't viewing. Error paths below don't reset: a transient
    // binding failure would flicker the view empty for a frame and
    // then refill, which is worse UX than keeping the last-known set
    // visible while the next retry flows.
    if (!id) {
      items = [];
      return;
    }
    try {
      const fetched = (await ListThreadDiffPayloads(id)) as Item[] | null;
      if (seq !== fetchSeq) return;
      if (id !== opts.getThreadId()) return;
      items = (fetched ?? []).filter((item) => item.threadId === id);
    } catch (err) {
      if (seq !== fetchSeq) return;
      if (id !== opts.getThreadId()) return;
      console.error('cumulativeDiffItems: ListThreadDiffPayloads failed:', err);
      // Keep the previous `items` snapshot so the cumulative view
      // doesn't flash empty on a transient failure. The next
      // successful refresh (thread-switch effect or debounced upsert)
      // overwrites this with fresh data.
    }
  }

  const debouncedRefresh = debounce(() => { void refresh(); }, REFRESH_DEBOUNCE_MS);

  // Initial + on-thread-switch fetch. Reading getThreadId inside the
  // effect lets Svelte track the caller's reactive source.
  $effect(() => {
    opts.getThreadId();
    void refresh();
  });

  let cancelItemUpsert: (() => void) | null = null;

  onMount(() => {
    // `diff` payloads always qualify; `tool_result` payloads only when
    // their meta carries an `inlineDiff.availability == "exact_patch"`
    // hint — matches the selector in selectAgentDiffEntries so
    // unrelated tool_results (bash, read-file, etc.) don't trigger a
    // refresh on every upsert during streaming. The debounce still
    // collapses the tool_call → tool_completion burst into one query.
    cancelItemUpsert = wailsEventOn<Item>('provider:item_upsert', (item) => {
      if (!item || item.threadId !== opts.getThreadId()) return;
      if (item.payloadKind === 'diff') {
        debouncedRefresh();
        return;
      }
      if (item.payloadKind !== 'tool_result' || !item.payloadMeta) return;
      try {
        const meta = JSON.parse(item.payloadMeta) as {
          inlineDiff?: { availability?: string };
        };
        if (meta.inlineDiff?.availability === 'exact_patch') {
          debouncedRefresh();
        }
      } catch {
        // Malformed meta — nothing to add to the cumulative view.
      }
    });
  });

  onDestroy(() => {
    cancelItemUpsert?.();
    debouncedRefresh.cancel();
  });

  return {
    get items() { return items; },
    get entries() { return entries; },
    get stats() { return stats; },
  };
}
