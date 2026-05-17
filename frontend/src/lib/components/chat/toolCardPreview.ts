import type { Item } from '../../types/models';
import { findPathRanges } from '../../utils/pathLinkify';
import { waitAgentDisplayReceiverIds } from '../../utils/waitAgentDisplay';
import { isCommandToolName } from './commandDisplay';

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
  summaryMeta: Record<string, unknown> | null,
  itemMeta: Record<string, unknown> | null,
): string {
  if (item.toolName === 'wait_agent') {
    return waitAgentPreview(item, itemMeta);
  }
  const mcp = mcpPreviewFromMeta(itemMeta);
  if (mcp) return mcp;
  const fromSummary = (item.summary ?? '').trim();
  if (fromSummary) return fromSummary;
  if (summaryMeta) {
    const title = summaryMeta.title;
    if (typeof title === 'string' && title.trim()) return title.trim();
  }
  return fallbackPreviewForToolName(item.toolName);
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

/**
 * Compose the MCP body the redesign spec calls for — `server.tool(args)`
 * — from the wire-typed metadata both providers' parsers stamp onto the
 * item. `meta.mcp` carries the {server, tool} pair (the normalized
 * `MCP/<tool>` toolName drops the server half), `meta.input` carries
 * the raw argument dict. Returns `''` when the row is not MCP so the
 * caller falls back through the normal preview chain.
 */
function mcpPreviewFromMeta(itemMeta: Record<string, unknown> | null): string {
  if (!itemMeta) return '';
  const mcp = itemMeta.mcp;
  if (!mcp || typeof mcp !== 'object' || Array.isArray(mcp)) return '';
  const server = readString((mcp as Record<string, unknown>).server);
  const tool = readString((mcp as Record<string, unknown>).tool);
  if (!server && !tool) return '';
  const head = server && tool ? `${server}.${tool}` : server || tool;
  const args = formatMcpArgs(itemMeta.input);
  return `${head}(${args})`;
}

function readString(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

/**
 * Format an MCP input dict as a compact `key=value, key2=value2`
 * preview. Values are JSON-serialized so primitives render bare
 * (`42`, `"foo"`) and nested objects render as their JSON form,
 * matching how a developer reads a function call. The whole preview
 * truncates at MCP_ARGS_PREVIEW_MAX with an ellipsis so a giant
 * argument blob doesn't drown out the rest of the row; the truncation
 * threshold is generous enough that short arg dicts (the common case)
 * stay legible without wrapping.
 */
function formatMcpArgs(input: unknown): string {
  if (!input || typeof input !== 'object' || Array.isArray(input)) return '';
  const entries = Object.entries(input as Record<string, unknown>);
  if (entries.length === 0) return '';
  const parts: string[] = [];
  for (const [key, value] of entries) {
    parts.push(`${key}=${formatMcpArgValue(value)}`);
  }
  const joined = parts.join(', ');
  if (joined.length <= MCP_ARGS_PREVIEW_MAX) return joined;
  return `${joined.slice(0, MCP_ARGS_PREVIEW_MAX - 1)}…`;
}

function formatMcpArgValue(value: unknown): string {
  if (value === null || value === undefined) return 'null';
  if (typeof value === 'string') {
    if (value.length <= MCP_ARG_STRING_MAX) return JSON.stringify(value);
    return `${JSON.stringify(value.slice(0, MCP_ARG_STRING_MAX - 1))}…`;
  }
  if (typeof value === 'number' || typeof value === 'boolean') {
    return String(value);
  }
  try {
    const json = JSON.stringify(value);
    if (json.length <= MCP_ARG_STRING_MAX) return json;
    return `${json.slice(0, MCP_ARG_STRING_MAX - 1)}…`;
  } catch {
    return '…';
  }
}

const MCP_ARGS_PREVIEW_MAX = 120;
const MCP_ARG_STRING_MAX = 60;

function waitAgentPreview(item: Item, meta: Record<string, unknown> | null): string {
  const count = receiverThreadCount(meta);
  const noun = count === 1 ? 'agent' : 'agents';
  const verb = item.status === 'running' || item.status === 'streaming'
    ? 'Waiting on'
    : 'Waited on';
  return count > 0 ? `${verb} ${count} ${noun}` : `${verb} agents`;
}

function fallbackPreviewForToolName(toolName: string | undefined): string {
  const raw = (toolName ?? '').trim();
  if (!raw) return 'Tool';
  if (raw === 'MCP') return 'MCP tool';
  if (raw.startsWith('MCP/')) return raw.slice(4) || 'MCP tool';
  if (isCommandToolName(raw)) return 'Bash';
  return raw;
}

function receiverThreadCount(meta: Record<string, unknown> | null): number {
  const input = meta?.input;
  if (!input || typeof input !== 'object') return 0;
  return waitAgentDisplayReceiverIds(input as Record<string, unknown>).length;
}
