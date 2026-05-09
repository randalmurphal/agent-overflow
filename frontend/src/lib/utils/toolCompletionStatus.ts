// Backgrounded launch rows are persistent transcript records: per
// docs/architecture/turn-lifecycle.md §1 the launch row stays
// active while the host-side background work is unobserved; the actual
// completion lands on a separate `tool_completion` sibling row, which
// is where the badge belongs. The `null` return below preserves that
// invariant — backgrounded launches never receive a success/failure
// verdict here.

import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

export type CompletionStatus = 'success' | 'failure' | null;

export interface CompletionMetaOptions {
  /** Pre-parsed payload meta — pass when the caller already parsed it
   * (e.g. CommandOutput's `meta: CommandOutputMeta`) so we don't redo
   * the JSON.parse work. When omitted, the helper parses
   * `item.payloadMeta` itself. */
  meta?: Record<string, unknown> | null;
}

type ItemSubset = Pick<Item, 'kind' | 'status' | 'isBackground' | 'payloadMeta'>;

export function deriveCompletionStatus(
  item: ItemSubset,
  options?: CompletionMetaOptions,
): CompletionStatus {
  if (item.kind === 'tool_call' && item.isBackground === true) return null;
  if (item.status === 'streaming' || item.status === 'running') return null;
  if (item.status === 'errored' || item.status === 'killed' || item.status === 'declined') {
    return 'failure';
  }
  const meta = options?.meta !== undefined ? options.meta : parseJsonObject(item.payloadMeta);
  if (isErrorFlag(meta)) return 'failure';
  const code = readExitCode(meta);
  if (code !== null && code !== 0) return 'failure';
  return 'success';
}

export function completionBadgeTitleForStatus(status: ItemSubset['status']): string | undefined {
  if (status === 'killed') return 'Stopped';
  if (status === 'declined') return 'Declined';
  if (status === 'errored') return 'Failed';
  return undefined;
}

// Both casings appear in practice: Claude's parse_user.go writes
// `is_error` (snake), tool_lifecycle.go writes `isError` (camel) in
// some completion headers. Either flag signals failure.
function isErrorFlag(meta: Record<string, unknown> | null): boolean {
  if (!meta) return false;
  return meta.is_error === true || meta.isError === true;
}

// codex_background.go writes both `exit_code` and `exitCode` keys onto
// the same payload meta; readers must check both since some upstream
// paths only emit the snake_case variant.
function readExitCode(meta: Record<string, unknown> | null): number | null {
  if (!meta) return null;
  const camel = meta.exitCode;
  if (typeof camel === 'number' && Number.isFinite(camel)) return camel;
  const snake = meta.exit_code;
  if (typeof snake === 'number' && Number.isFinite(snake)) return snake;
  return null;
}
