import type { Item } from '../../types/models';
import { findPathRanges } from '../../utils/pathLinkify';
import { waitAgentDisplayReceiverIds } from '../../utils/waitAgentDisplay';
import type { ToolKindVisual } from './toolCardHeader';

/**
 * Decoded preview shape. The single-segment plain-text result is the
 * default; when the preview includes a path-like token at the start,
 * we surface a structured form that the renderer wires through
 * EditorLink. We intentionally only linkify a leading path — the
 * preview is short (often "Read foo.ts") and a heuristic that linked
 * embedded paths in mid-sentence would catch chip-style summaries
 * unintentionally.
 */
export interface ToolCardPreview {
  text: string;
  path?: { path: string; line?: number; col?: number };
}

export function toolCardInputPreview(
  item: Item,
  classification: ToolKindVisual,
  summaryMeta: Record<string, unknown> | null,
  itemMeta: Record<string, unknown> | null,
): string {
  if (item.toolName === 'wait_agent') {
    return waitAgentPreview(item, itemMeta);
  }
  const fromSummary = (item.summary ?? '').trim();
  if (fromSummary) return fromSummary;
  if (summaryMeta) {
    const title = summaryMeta.title;
    if (typeof title === 'string' && title.trim()) return title.trim();
  }
  return classification.displayName;
}

/**
 * Same source string, but split into a leading path (if present) plus
 * the residual prose so the renderer can attach an EditorLink. The
 * path token must begin at offset 0 — anything else is left alone to
 * avoid confusing chip-style summaries (e.g. "Wrote 3 files (a.ts,
 * b.ts, c.ts)") with tool inputs that take a single path.
 */
export function decodeToolCardPreview(text: string): ToolCardPreview {
  if (!text) return { text };
  const ranges = findPathRanges(text);
  if (ranges.length === 0 || ranges[0].start !== 0) return { text };
  const head = ranges[0];
  return {
    text,
    path: { path: head.path, line: head.line, col: head.col },
  };
}

function waitAgentPreview(item: Item, meta: Record<string, unknown> | null): string {
  const count = receiverThreadCount(meta);
  const noun = count === 1 ? 'agent' : 'agents';
  const verb = item.status === 'running' || item.status === 'streaming'
    ? 'Waiting on'
    : 'Waited on';
  return count > 0 ? `${verb} ${count} ${noun}` : `${verb} agents`;
}

function receiverThreadCount(meta: Record<string, unknown> | null): number {
  const input = meta?.input;
  if (!input || typeof input !== 'object') return 0;
  return waitAgentDisplayReceiverIds(input as Record<string, unknown>).length;
}
