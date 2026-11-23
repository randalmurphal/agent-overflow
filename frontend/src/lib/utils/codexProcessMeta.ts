import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

/**
 * Extract the Codex unified-exec PTY process id from an item's `meta`
 * JSON blob. Triage allowlists it onto a root thread command's tray row
 * because it is the handle `TerminateCodexBackgroundTerminal` takes, the
 * Stop of a computer that predates `StopCodexBackgroundCommand`.
 *
 * Null when the row carries none; a row with neither this nor
 * `codexCommandStopsByItem` simply has no per-row stop affordance rather
 * than one that would target nothing.
 */
export function extractCodexProcessID(item: Item): string | null {
  const id = parseJsonObject(item.meta)?.process_id;
  return typeof id === 'string' && id.length > 0 ? id : null;
}

/**
 * Whether a Codex command's tray row is stopped by its item id
 * (`StopCodexBackgroundCommand`), which reaches the command in whichever
 * Codex thread runs it. The backend sets it once the command's process is
 * named and its thread takes requests; a command inside a code review has
 * no Stop and ends with the review.
 */
export function codexCommandStopsByItem(item: Item): boolean {
  return parseJsonObject(item.meta)?.stop_by_item === true;
}
