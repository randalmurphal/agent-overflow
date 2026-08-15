import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

const FILE_CHANGE_TOOL_NAMES = new Set([
  'Edit',
  'MultiEdit',
  'Write',
  'NotebookEdit',
  'apply_patch',
  'file_change',
  'fileChange',
]);

/**
 * Number of file-level rows a file-change item projects into in chat.
 *
 * The persisted item remains the provider's one canonical operation. Rich
 * tool-result metadata is authoritative; `input.files` is the summary-only
 * fallback retained by the Codex adapter when no diff payload could be built.
 */
export function fileChangeDisplayRowCount(item: Item): number {
  if (!FILE_CHANGE_TOOL_NAMES.has(item.toolName?.trim() ?? '')) return 1;

  const payloadMeta = parseJsonObject(item.payloadMeta);
  const inlineDiff = objectField(payloadMeta, 'inlineDiff');
  const totalFiles = positiveInteger(inlineDiff?.totalFiles);
  if (totalFiles !== null) return totalFiles;
  if (Array.isArray(inlineDiff?.files) && inlineDiff.files.length > 0) {
    return inlineDiff.files.length;
  }

  const itemMeta = parseJsonObject(item.meta);
  const input = objectField(itemMeta, 'input');
  if (Array.isArray(input?.files)) {
    const paths = input.files.filter(
      (path): path is string => typeof path === 'string' && path.trim().length > 0,
    );
    if (paths.length > 0) return paths.length;
  }
  return 1;
}

function objectField(
  value: Record<string, unknown> | null,
  key: string,
): Record<string, unknown> | null {
  const field = value?.[key];
  return field !== null && typeof field === 'object' && !Array.isArray(field)
    ? field as Record<string, unknown>
    : null;
}

function positiveInteger(value: unknown): number | null {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return null;
  return Math.floor(value);
}
