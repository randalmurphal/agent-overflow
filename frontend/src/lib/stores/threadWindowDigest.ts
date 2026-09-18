// The client half of the held-window description a `SyncThreadWindow`
// request carries (docs/architecture/thread-replica-sync.md §3.4, §5, and
// docs/architecture/timeline-window-pages.md §5).
//
// A turn on the open thread clears the pane's window attestation, so the
// next open has no stamp to send even though every row it holds is still
// current. The repair is to describe the ROWS instead of the thread: the
// edges, the count, the has-more flags, and a digest of the `(id, rev)`
// pairs inside. The server re-derives all of it from the database inside
// the sync transaction and answers a page-less `fresh` when it agrees, so
// nothing here is trusted — a wrong description costs one page.
//
// A page no longer ships every row in its range: an activity run ships a
// window of members plus a stub for the rest, so the window the pane
// holds is loaded rows PLUS shed rows PLUS each held run's unshipped
// members. The digest is an XOR of per-row hashes precisely so those
// three sources fold into one value without the pane holding the rows.
//
// The digest must agree byte-for-byte with `internal/store/window_digest.go`.
// The shared vectors in `src/test/fixtures/windowDigestVectors.json` are a
// byte-identical copy of `internal/store/testdata/window_digest_vectors.json`
// and both sides run them.
import {
  FNV1A64_ZERO,
  fnv1a64,
  formatFnv1a64,
  xorFnv1a64,
  type Fnv1a64,
} from '../utils/fnv1a';
import type { Item } from '../types/models';
import type { HeldWindow } from '../../../bindings/agent-overflow/internal/store/models';

/**
 * The revision a locally minted or deliberately altered row carries, so
 * it can never prove freshness. Mirror of Go `store.UnstampedItemRev`: a
 * negative revision matches no stamp, so a window containing one cannot
 * be described at all.
 */
export const UNSTAMPED_ITEM_REV = -1;

/**
 * The largest held window the server will verify (Go
 * `store.MaxHeldWindowItems`), counted in PHYSICAL rows. A window past it
 * is refused unverified, so describing one is wasted work —
 * `heldWindowOf` returns null instead. Far larger than a page's shipped
 * row budget because a page describes long runs with stubs rather than
 * rows, and the verification read is `(id, rev)` off the ordering index.
 */
export const MAX_HELD_WINDOW_ITEMS = 8000;

/**
 * The rows a history window can contain, mirroring the store's
 * `windowedTimelineFilter` (internal/store/paging.go): top-level rows
 * only, and never a `plan_update` notification. Subagent children render
 * inside their anchor's card and load on demand, and plan_update
 * notifications are a side channel; neither is part of any page, so
 * neither may enter the digest.
 */
export function isWindowedTimelineRow(item: Item): boolean {
  if (item.parentId) return false;
  return !(item.kind === 'notification' && item.toolName === 'plan_update');
}

/** One `(id, rev)` pair, the only thing the digest folds. */
export interface WindowDigestRow {
  id: string;
  rev: number;
}

// Framing byte, matching the Go side: a unit separator between a row's id
// and its rev, so no concatenation of an id and decimal digits can
// produce another row's canonical string. It is outside the id alphabet
// (ASCII words, ':' and '-'). No record separator: each row is hashed on
// its own and the hashes are XORed, so no two rows share a string.
const FIELD_SEP = '\x1f';

/**
 * One row's contribution: FNV-1a 64 over `id + 0x1f + decimal rev`,
 * folded as UTF-16 code units.
 */
export function windowDigestRowHash(row: WindowDigestRow): Fnv1a64 {
  return fnv1a64(row.id + FIELD_SEP + String(row.rev));
}

/**
 * Fold a set of timeline rows into the 16-lowercase-hex digest a client
 * sends with a held window.
 *
 * Each row hashes alone and the hashes are XORed, so the digest is
 * order-free and composable: a client that holds part of a run, shed part
 * of it and has a stub for the rest folds three digests into one instead
 * of rebuilding a canonical string it no longer has the rows for. Both
 * sides fold UTF-16 code units — Go decodes to UTF-16 before folding — so
 * the contract holds for any id, not only an ASCII one.
 *
 * The empty set folds to zero, the identity XOR needs: a client holding
 * nothing contributes nothing.
 */
export function windowDigest(rows: readonly WindowDigestRow[]): string {
  let folded: Fnv1a64 = FNV1A64_ZERO;
  for (const row of rows) folded = xorFnv1a64(folded, windowDigestRowHash(row));
  return formatFnv1a64(folded);
}

/**
 * What the activity runs a pane holds contribute to its held window: the
 * physical rows it does NOT hold as `Item`s (shed members plus every
 * member outside each run's loaded span) and their folded digest.
 *
 * Null from the registry means the contribution cannot be stated — a run
 * record is dirty, or a shed row carries no usable revision — and then
 * the pane describes no window at all.
 */
export interface HeldRunFold {
  count: number;
  digest: Fnv1a64;
}

/** The fold of a pane holding no activity-run stubs at all. */
export const NO_HELD_RUNS: HeldRunFold = { count: 0, digest: FNV1A64_ZERO };

/**
 * Describe the window a pane holds, or null when it holds none the server
 * could verify.
 *
 * `items` is the pane's live window in (turnIndex, itemIndex) order; rows
 * a page would not return are filtered out here exactly as the server
 * filters them, so a fold or a plan_update notification changes nothing
 * about the answer. `runs` adds the rows the pane describes through run
 * stubs rather than holding: their count joins the row count and their
 * digest XORs into the row digest (§5).
 *
 * Null when:
 *
 *  - `runs` is null. The registry could not state its contribution (a
 *    dirty record, an unusable stub digest), so nothing the pane could
 *    send would describe the range.
 *  - no row survives the filter. The server refuses an empty window
 *    anyway — it has no edges to resolve.
 *  - any row carries `rev < 0`. An unstamped row cannot verify, and the
 *    server refuses the window; sending it would cost the same page.
 *  - the physical count passes `MAX_HELD_WINDOW_ITEMS`, which the server
 *    refuses unverified.
 *
 * The has-more flags come from the caller's window metadata rather than
 * the rows, because they are what the reader sees: a window with the
 * right rows and the wrong ends renders the wrong affordance, so the
 * server checks them too.
 */
export function heldWindowOf(
  items: readonly Item[],
  hasMoreOlder: boolean,
  hasMoreNewer: boolean,
  runs: HeldRunFold | null,
): HeldWindow | null {
  if (!runs) return null;
  let oldestItemId = '';
  let newestItemId = '';
  let loaded = 0;
  let folded = runs.digest;
  for (const item of items) {
    if (!isWindowedTimelineRow(item)) continue;
    if (item.rev < 0) return null;
    loaded += 1;
    if (loaded === 1) oldestItemId = item.id;
    newestItemId = item.id;
    folded = xorFnv1a64(folded, windowDigestRowHash(item));
  }
  if (loaded === 0) return null;
  const count = loaded + runs.count;
  if (count > MAX_HELD_WINDOW_ITEMS) return null;
  return {
    oldestItemId,
    newestItemId,
    count,
    hasMoreOlder,
    hasMoreNewer,
    digest: formatFnv1a64(folded),
  };
}
