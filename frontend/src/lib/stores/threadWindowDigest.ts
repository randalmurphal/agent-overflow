// The client half of the held-window description a `SyncThreadWindow`
// request carries (docs/architecture/thread-replica-sync.md §3.4, §5).
//
// A turn on the open thread clears the pane's window attestation, so the
// next open has no stamp to send even though every row it holds is still
// current. The repair is to describe the ROWS instead of the thread: the
// edges, the count, the has-more flags, and a digest of the `(id, rev)`
// pairs inside. The server re-derives all of it from the database inside
// the sync transaction and answers a page-less `fresh` when it agrees, so
// nothing here is trusted — a wrong description costs one page.
//
// The digest must agree byte-for-byte with `internal/store/window_digest.go`.
// The shared vectors in `src/test/fixtures/windowDigestVectors.json` are a
// byte-identical copy of `internal/store/testdata/window_digest_vectors.json`
// and both sides run them.
import { fnv1a64Hex } from '../utils/fnv1a';
import type { Item } from '../types/models';
import type { HeldWindow } from '../../../bindings/agent-overflow/internal/store/models';

/**
 * The revision a locally minted or deliberately altered row carries, so
 * it can never prove freshness. Mirror of Go `store.UnstampedItemRev`: a
 * negative revision matches no stamp, so a window containing one always
 * pages.
 */
export const UNSTAMPED_ITEM_REV = -1;

/**
 * The largest held window the server will verify (Go
 * `store.MaxHeldWindowItems`). A window past it is refused unverified, so
 * describing one is wasted work — `heldWindowOf` returns null instead.
 * The pane's own retention ceiling
 * (`ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS`, 2400) is larger, so a
 * pane holding between the two pays one page on reopen.
 */
export const MAX_HELD_WINDOW_ITEMS = 2000;

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

// Framing bytes, matching the Go side: a unit separator between a row's
// id and its rev, a record separator after each row, so no concatenation
// of ids and decimal digits can produce another window's canonical
// string. Both are outside the id alphabet (ASCII words, ':' and '-').
const FIELD_SEP = '\x1f';
const ROW_SEP = '\x1e';

/**
 * Fold an ordered run of timeline rows into the 16-lowercase-hex digest a
 * client sends with a held window.
 *
 * Canonical string: `id + 0x1f + decimal rev + 0x1e` per row, in
 * (turnIndex, itemIndex) order. Both sides fold that string's UTF-16
 * code units — Go decodes it to UTF-16 before folding — so the contract
 * holds for any id, not only an ASCII one.
 *
 * The empty run hashes to the offset basis and is never confusable with a
 * real window, because every non-empty run appends at least one separator.
 */
export function windowDigest(rows: readonly WindowDigestRow[]): string {
  let canonical = '';
  for (const row of rows) canonical += row.id + FIELD_SEP + String(row.rev) + ROW_SEP;
  return fnv1a64Hex(canonical);
}

/**
 * Describe the window a pane holds, or null when it holds none the server
 * could verify.
 *
 * `items` is the pane's live window in (turnIndex, itemIndex) order; rows
 * a page would not return are filtered out here exactly as the server
 * filters them, so a fold or a plan_update notification changes nothing
 * about the answer. Null when no row survives the filter: the server
 * refuses an empty window anyway (it has no edges to resolve), and
 * sending one would only describe the absence of rows. Null again past
 * `MAX_HELD_WINDOW_ITEMS`, which the server refuses unverified.
 *
 * The canonical string is built inline rather than through an
 * intermediate row array: this runs on the cold-open path over the whole
 * window, and a per-row object would exist only to be hashed once.
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
): HeldWindow | null {
  let oldestItemId = '';
  let newestItemId = '';
  let count = 0;
  let canonical = '';
  for (const item of items) {
    if (!isWindowedTimelineRow(item)) continue;
    count += 1;
    if (count > MAX_HELD_WINDOW_ITEMS) return null;
    if (count === 1) oldestItemId = item.id;
    newestItemId = item.id;
    canonical += item.id + FIELD_SEP + String(item.rev) + ROW_SEP;
  }
  if (count === 0) return null;
  return {
    oldestItemId,
    newestItemId,
    count,
    hasMoreOlder,
    hasMoreNewer,
    digest: fnv1a64Hex(canonical),
  };
}
