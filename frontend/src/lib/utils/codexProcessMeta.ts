import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

/**
 * Extract the Codex unified-exec PTY process id from an item's `meta`
 * JSON blob. Triage allowlists it onto the transient tray row (and onto
 * the persisted command row) because it is the handle
 * `thread/backgroundTerminals/terminate` joins on — the Codex sibling of
 * Claude's `task_id`.
 *
 * Null when the wire hasn't named a process yet; a row with no process
 * id simply has no per-row stop affordance rather than one that would
 * target nothing.
 */
export function extractCodexProcessID(item: Item): string | null {
  const id = parseJsonObject(item.meta)?.process_id;
  return typeof id === 'string' && id.length > 0 ? id : null;
}
