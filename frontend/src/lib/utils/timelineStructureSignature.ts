import type { TimelineNode } from './subagentGrouping';

// A signature over the rendered timeline node sequence that changes whenever a
// row's measured height could change for a reason OTHER than scroll-pane width
// or row-UI expansion state — i.e. the STRUCTURE (which nodes, in what order,
// how many) or a leaf's CONTENT (text length, status, last-write stamp).
//
// This is the structure/content dimension of the virtua measured-size cache's
// validity key (see utils/threadVirtuaSizeCache.ts). It superseded an earlier
// version of that key that read `pane.timelineRevision`, a monotonic per-pane
// mutation counter that is never restored on a cache-hit re-entry — so a revisit
// always computed a strictly-greater revision than capture and the replay never
// matched (the cache was inert). `pane.timelineRevision` still exists and still
// drives timeline-derivation reactivity; it was just the wrong input to key the
// size replay on. The signature fixes that by being REPRODUCIBLE: revisiting
// a settled thread (same items, same order) yields the identical string, so the
// cached sizes replay and the estimate→measure cascade is skipped; a background
// content change (streaming text grew, a tool result filled in) bumps
// `updatedAt`/`summary`/`status`, yielding a different string, so the now-stale
// sizes are refused and virtua falls back to the flat estimate.
//
// SELF-VALIDATING. Because the signature encodes content, a stale snapshot is
// refused on the key alone — eviction (thread.svelte.ts removal/reswitch,
// threads.svelte.ts removeThread) is memory housekeeping, not a correctness
// requirement. That matters because `events.ts` evicts the item cache on any
// content change but never touched this cache; without content in the key, a
// backgrounded thread that changed and got reloaded would replay stale sizes.
//
// Each node is encoded POSITIONALLY because virtua's size cache is indexed by
// position. Leaves carry their content-height inputs: `summary.length` (text
// height at a fixed width), `status` (a streaming/spinner row differs from its
// settled form), and `updatedAt` (Go bumps it on every streaming append —
// items_write.go — so it tracks growth that `summary.length` alone might miss,
// e.g. a tool result's payload fill). Group nodes carry their key + member
// count (a collapsed card's height tracks how many rows it folds; an expanded
// group differs in `expansionSig` and is refused there). The `\n` join is
// collision-safe: item ids, statuses, group keys, and numbers never contain a
// newline, so two distinct node sequences cannot serialize to the same string.
export function timelineStructureSignature(nodes: readonly TimelineNode[]): string {
  const parts: string[] = new Array(nodes.length);
  for (let i = 0; i < nodes.length; i++) {
    parts[i] = nodeSignature(nodes[i]);
  }
  return parts.join('\n');
}

function nodeSignature(node: TimelineNode): string {
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
      // signature, not silently sign as an empty/identical row (which would
      // let the cache replay stale sizes across a structural change).
      const unreachable: never = node;
      throw new Error(
        `timelineStructureSignature: unhandled node kind ${(unreachable as { kind: string }).kind}`,
      );
    }
  }
}
