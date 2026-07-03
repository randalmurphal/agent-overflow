import type { TimelineNode } from './subagentGrouping';

// Per-row content signature for the timeline virtualizer's size-priors
// replay (utils/virtual/priors.ts, `SizePriorsEntry.rows`). Changes
// whenever a row's measured height could change for a reason OTHER than
// scroll-pane width or row-UI expansion state — i.e. the node's
// STRUCTURE (kind, key, membership) or a leaf's CONTENT (text length,
// status, last-write stamp).
//
// This module used to export only a whole-window `timelineStructureSignature`
// — the newline-join of every loaded row's signature — that keyed ONE
// positional snapshot per thread. That whole-window key made priors
// brittle across window-composition changes: a session's loaded window
// grows to hundreds of rows (streaming appends, loadOlder, prunes), but a
// fresh app boot always starts from a small initial slice, so the boot
// window's whole-window signature essentially never matched a captured
// session's — the snapshot never replayed except on tiny threads. Keying
// each row independently by its OWN signature fixes this: the boot
// window's rows are a SUFFIX subset of a larger captured window, and each
// one resolves independently against the shared per-row map
// (`SizePriorsEntry.rows`) instead of requiring the whole window to
// match. The joined function was deleted once this was its only
// consumer (`components/chat/timelineSizePriors.svelte.ts`).
//
// It in turn superseded an even earlier key that read
// `pane.timelineRevision`, a monotonic per-pane mutation counter that is
// never restored on a cache-hit re-entry — so a revisit always computed
// a strictly-greater revision than capture and the replay never matched
// (the cache was inert). This signature fixes THAT by being
// REPRODUCIBLE: revisiting a settled row (same id, same content) yields
// the identical string, so its cached size replays and the
// estimate→measure cascade is skipped for that row; a background
// content change (streaming text grew, a tool result filled in) bumps
// `updatedAt`/`summary`/`status`, yielding a different string, so the
// now-stale size is refused for that row alone and it falls back to the
// kind/flat estimate.
//
// SELF-VALIDATING. Because the signature encodes content, a stale row is
// refused on the key alone — eviction (`thread.svelte.ts`
// removal/reswitch, `threads.svelte.ts removeThread`) is memory
// housekeeping, not a correctness requirement.
//
// Leaves carry their content-height inputs: `summary.length` (text
// height at a fixed width), `status` (a streaming/spinner row differs
// from its settled form), and `updatedAt` (Go bumps it on every
// streaming append — items_write.go — so it tracks growth that
// `summary.length` alone might miss, e.g. a tool result's payload fill).
// Group nodes carry their key + member count (a collapsed card's height
// tracks how many rows it folds; an expanded group differs in
// `expansionSig` instead, gated at the entry level, not here).
export function nodeSignature(node: TimelineNode): string {
  switch (node.kind) {
    case 'leaf': {
      const item = node.item;
      return `L:${item.id}:${item.status}:${item.summary.length}:${item.updatedAt}`;
    }
    case 'group':
      return `S:${node.groupKey}:${node.children.length}`;
    case 'wait_group':
      return `W:${node.groupKey}:${node.children.length}`;
    case 'read_group':
      return `R:${node.groupKey}:${node.members.length}`;
    default: {
      // Exhaustiveness guard: a new TimelineNode kind must extend the
      // signature, not silently sign as an empty/identical row (which
      // would let a stale prior replay across a structural change).
      const unreachable: never = node;
      throw new Error(
        `nodeSignature: unhandled node kind ${(unreachable as { kind: string }).kind}`,
      );
    }
  }
}
