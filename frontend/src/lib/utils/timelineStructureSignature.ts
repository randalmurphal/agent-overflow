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
//
// Leaf and read_group signatures are memoized per NODE. Both node kinds
// are cached by the projection (`subagentGrouping.ts` / `readGrouping.ts`)
// and every input here is immutable for the node's lifetime: a leaf's
// `item` is fixed at mint and the store replaces the Item — minting a
// fresh leaf — on any write, and a read_group's members likewise re-mint
// the node. So the memo needs no invalidation; entries die with their
// nodes. Group/wait_group nodes are minted fresh per pass (a memo would
// never hit) and an activity_run's inputs include stamps mutated after
// mint (`collapsed`, the mount window), so those compute directly.
// The priors capture calls this for every row in the window on a bounded
// interim cadence while streaming; the per-call string builds were a
// visible line of the 2026-08-25 allocation profile.
const signatureByNode = new WeakMap<TimelineNode, string>();

export function nodeSignature(node: TimelineNode): string {
  switch (node.kind) {
    case 'leaf': {
      const cached = signatureByNode.get(node);
      if (cached !== undefined) return cached;
      const item = node.item;
      const signature = `L:${item.id}:${item.status}:${item.summary.length}:${item.updatedAt}`;
      signatureByNode.set(node, signature);
      return signature;
    }
    case 'group':
      return `S:${node.groupKey}:${node.children.length}`;
    case 'wait_group':
      return `W:${node.groupKey}:${node.children.length}`;
    case 'read_group': {
      const cached = signatureByNode.get(node);
      if (cached !== undefined) return cached;
      const signature = `R:${node.groupKey}:${node.members.length}`;
      signatureByNode.set(node, signature);
      return signature;
    }
    // Collapse state changes the row's height dramatically (one header line
    // vs a header over a capped clip), so it belongs here rather than in the
    // entry-level expansionSig — folding it there would drop every row's prior
    // in the thread each time one run was toggled. The mount window is here for
    // the same reason: mounting another chunk grows a run that has not reached
    // its cap, and moving the window swaps which rows it measures, so a
    // signature blind to either replays a height that no longer applies.
    case 'activity_run':
      // TWO shapes, because the header is unconditional: closed is that header
      // alone, open is the header over a clip. `collapsed` is the whole answer —
      // it is resolved once per pass, liveness already folded in
      // (`ActivityRunIdentity.collapsedFor`), so nothing here has to re-decide
      // whether a live run counts as open.
      return `A:${node.runId}:${node.collapsed ? 'c' : 'o'}:${node.children.length}:${node.mountedFrom}:${node.mountedRows}`;
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
