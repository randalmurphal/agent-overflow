import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

/**
 * Extract the Claude `task_id` from an item's `meta` JSON blob.
 * Triage stamps this onto launch/completion/notification rows so the
 * frontend can correlate task lifecycle UI without reparsing the same
 * metadata string at every call site.
 */
export function extractClaudeTaskID(item: Item): string | null {
  const id = parseJsonObject(item.meta)?.task_id;
  return typeof id === 'string' && id.length > 0 ? id : null;
}
