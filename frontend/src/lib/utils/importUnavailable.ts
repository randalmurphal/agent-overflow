// Label for a transcript body whose payload an IMPORT could not carry over,
// as opposed to one that was never stored.
//
// The importer stamps `items.meta.import_unavailable` with the reason a
// payload is missing (a Claude tool output the CLI externalized and then
// garbage-collected; a Codex exec end-event that couldn't be matched to its
// tool call). Rows land in `ExpandablePayloadBody`'s empty branch either way,
// so without this the reader is told "No stored payload for this tool result"
// — true of a live thread's gap, misleading for an import that never had the
// bytes to begin with.
//
// One label serves every reason the importer stamps today
// ("tool-output-gc" for Claude, "exec-detail" for Codex) — the distinction
// matters to the importer, never to the reader, who only needs to know the
// bytes were never there to import. THIS FUNCTION is the seam: a future
// reason that deserves its own wording branches here, and no call site
// changes. A lookup table mapping every key to the same string would only
// look like it distinguished them.

import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

const META_KEY = 'import_unavailable';
const GENERIC_LABEL = 'Not available from import.';

/**
 * The label to show in place of a missing payload, or null when the item
 * isn't an import casualty and the caller's own empty message applies.
 */
export function importUnavailableLabel(item: Pick<Item, 'meta'> | null | undefined): string | null {
  const reason = parseJsonObject(item?.meta)?.[META_KEY];
  if (!reason) return null;
  return GENERIC_LABEL;
}
