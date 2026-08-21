import type { Item } from '../../types/models';
import { findPathRanges } from '../../utils/pathLinkify';
import { waitAgentDisplayReceiverIds } from '../../utils/waitAgentDisplay';
import { isCommandToolName } from './commandDisplay';

const STRUCTURED_FILE_EDIT_TOOLS = new Set([
  'Edit',
  'MultiEdit',
  'Write',
  'NotebookEdit',
  'file_change',
  'fileChange',
]);

const STRUCTURED_PATH_PREVIEW_TOOLS = new Set(['Read', ...STRUCTURED_FILE_EDIT_TOOLS]);

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
  if (item.toolName === 'Skill') {
    const skill = skillPreviewFromMeta(itemMeta);
    if (skill) return skill;
  }
  if (item.toolName === 'ToolSearch') {
    const search = toolSearchPreview(itemMeta);
    if (search) return search;
  }
  if (item.toolName === 'SendMessage') {
    const peer = sendMessagePreview(itemMeta);
    if (peer) return peer;
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
 * Pre-rendered tool body preview: file-target tools first use
 * structured `meta.input` paths so display never depends on
 * triage's capped summary preview. Other tools fall back to
 * `toolCardInputPreview` and apply three strip passes that target
 * the common redundancies in the triage-built summary string:
 *
 *   1. Strip the leading `${toolName}: ` segment that
 *      `buildToolCallSummary` embeds at persist time. The tool kind
 *      already renders as the gutter label, so repeating it inside
 *      the body adds noise without information.
 *   2. Relativize a leading workspace-rooted absolute path to its
 *      workspace-relative form. The check lexically normalizes `..`
 *      segments first so outside-workspace paths still render fully.
 *      EditorLink's `path` keeps this
 *      relative form so click-to-open still resolves through
 *      `workspacePath`.
 *   3. Collapse repo-local or relative displayed paths to the basename
 *      of the leading path token (e.g. `src/lib/foo.ts` → `foo.ts`).
 *      Outside-workspace absolute paths render fully. The relative
 *      path is what gets clicked through; the displayed text is what a
 *      human reads at a glance, and the directory prefix is noise when
 *      every row in a turn is in the same project.
 *
 * Stripping only applies to the leading path token to avoid mangling
 * mid-string usage (e.g. `cd /path && ls`). Paths outside the
 * workspace pass through untouched, so calls into `/usr/...`,
 * `/etc/...`, or a sibling repo still render fully.
 *
 * Returns the same `{text, path?}` shape `decodeToolCardPreview`
 * produces so callers can render the text and feed `path` straight
 * into EditorLink. The EditorLink path stays workspace-relative
 * (`src/lib/foo.ts`) while the displayed text shows just `foo.ts` —
 * the EditorLink renders its own label from `text`, so the basename
 * is what the user sees and clicks on, but the underlying target is
 * the full relative path EditorLink's workspacePath prop joins back
 * into an absolute open target.
 */
export function presentToolCardInputPreview(
  item: Item,
  summaryMeta: Record<string, unknown> | null,
  itemMeta: Record<string, unknown> | null,
  workspacePath: string,
): ToolCardPreview {
  const fromStructuredPath = structuredPathPreview(item, itemMeta, workspacePath);
  if (fromStructuredPath) return fromStructuredPath;

  const raw = toolCardInputPreview(item, summaryMeta, itemMeta);
  const afterPrefix = stripToolNamePrefix(raw, item.toolName);
  const decoded = decodeToolCardPreview(afterPrefix);
  const text = displayTextForPreview(decoded.text, workspacePath);
  if (!decoded.path) return { text };
  const pathDisplay = displayPathForTarget(decoded.path.path, workspacePath);
  const displayText = text.startsWith(pathDisplay.target)
    ? pathDisplay.label + text.slice(pathDisplay.target.length)
    : text;
  return {
    text: displayText,
    path: { path: pathDisplay.target, line: decoded.path.line, col: decoded.path.col },
  };
}

export function structuredToolPathTarget(
  item: Item,
  itemMeta: Record<string, unknown> | null,
  workspacePath: string,
): string {
  const rawPath = primaryStructuredPath(item, itemMeta);
  if (!rawPath) return '';
  const decoded = decodeToolCardPreview(rawPath);
  const path = decoded.path?.path ?? rawPath;
  return displayPathForTarget(path, workspacePath).target;
}

/**
 * Every path carried by a structured file-edit launch. Codex uses the
 * singular `file_path` shape for one-file changes and `files` for a
 * multi-file change; both project to the same independent file rows.
 */
export function structuredFileEditPathTargets(
  item: Item,
  itemMeta: Record<string, unknown> | null,
  workspacePath: string,
): string[] {
  if (!STRUCTURED_FILE_EDIT_TOOLS.has((item.toolName ?? '').trim())) return [];

  const singular = structuredToolPathTarget(item, itemMeta, workspacePath);
  if (singular) return [singular];

  const input = itemMeta?.input;
  if (!input || typeof input !== 'object' || Array.isArray(input)) return [];
  const files = (input as Record<string, unknown>).files;
  if (!Array.isArray(files)) return [];

  const seen = new Set<string>();
  const paths: string[] = [];
  for (const value of files) {
    if (typeof value !== 'string' || !value.trim()) continue;
    const decoded = decodeToolCardPreview(value.trim());
    const path = decoded.path?.path ?? value.trim();
    const target = displayPathForTarget(path, workspacePath).target;
    if (!target || seen.has(target)) continue;
    seen.add(target);
    paths.push(target);
  }
  return paths;
}

function basenameOf(path: string): string {
  const lastSep = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'));
  return lastSep === -1 ? path : path.slice(lastSep + 1);
}

function structuredPathPreview(
  item: Item,
  itemMeta: Record<string, unknown> | null,
  workspacePath: string,
): ToolCardPreview | null {
  const rawPath = primaryStructuredPath(item, itemMeta);
  if (!rawPath) return null;
  const decoded = decodeToolCardPreview(rawPath);
  const path = decoded.path?.path ?? rawPath;
  const line = decoded.path?.line;
  const col = decoded.path?.col;
  const pathDisplay = displayPathForTarget(path, workspacePath);
  return {
    text: appendLocation(pathDisplay.label, line, col),
    path: { path: pathDisplay.target, line, col },
  };
}

function primaryStructuredPath(
  item: Item,
  itemMeta: Record<string, unknown> | null,
): string {
  const rawToolName = (item.toolName ?? '').trim();
  if (!usesStructuredPathPreview(rawToolName)) return '';
  const input = itemMeta?.input;
  if (!input || typeof input !== 'object' || Array.isArray(input)) return '';
  const record = input as Record<string, unknown>;
  const keys = rawToolName === 'NotebookEdit'
    ? ['notebook_path', 'file_path', 'path']
    : ['file_path', 'notebook_path', 'path'];
  for (const key of keys) {
    const value = record[key];
    if (typeof value === 'string' && value.trim()) return value.trim();
  }
  return '';
}

function usesStructuredPathPreview(toolName: string): boolean {
  return STRUCTURED_PATH_PREVIEW_TOOLS.has(toolName);
}

function appendLocation(text: string, line: number | undefined, col: number | undefined): string {
  if (!line || line <= 0) return text;
  return col && col > 0 ? `${text}:${line}:${col}` : `${text}:${line}`;
}

interface PathDisplay {
  target: string;
  label: string;
}

function displayPathForTarget(path: string, workspacePath: string): PathDisplay {
  const target = relativizeLeadingWorkspacePath(path, workspacePath);
  if (target !== path) {
    return { target, label: basenameOf(target) };
  }
  if (!isAbsolutePath(path)) return displayRelativePath(path);
  return { target: path, label: path };
}

function displayRelativePath(path: string): PathDisplay {
  const normalized = normalizeRelativePath(path);
  if (normalized.escapesRoot) return { target: path, label: path };
  return { target: normalized.path, label: basenameOf(normalized.path) };
}

function displayTextForPreview(text: string, workspacePath: string): string {
  const relText = relativizeLeadingWorkspacePath(text, workspacePath);
  if (relText !== text) return relText;
  return text;
}

function isAbsolutePath(path: string): boolean {
  return path.startsWith('/') || path.startsWith('\\') || /^[A-Za-z]:[\\/]/.test(path);
}

function stripToolNamePrefix(text: string, toolName: string | undefined): string {
  if (!toolName) return text;
  const trimmed = toolName.trim();
  if (!trimmed) return text;
  const prefix = `${trimmed}: `;
  return text.startsWith(prefix) ? text.slice(prefix.length) : text;
}

// Returns `text` unchanged unless it lexically normalizes to a path under
// the workspace root. Refuses to strip the bare root (no child path) so
// we never return an empty preview, and matches both `/` and `\`
// separators so Windows paths sent through Codex on WSL render relatively
// too. This is a display/open-target normalization only; backend editor
// opening still owns real filesystem validation.
function relativizeLeadingWorkspacePath(text: string, workspacePath: string): string {
  const relative = workspaceRelativePath(text, workspacePath);
  return relative ?? text;
}

interface NormalizedAbsolutePath {
  path: string;
  comparePath: string;
  separator: '/' | '\\';
}

function workspaceRelativePath(path: string, workspacePath: string): string | null {
  const normalizedPath = normalizeAbsolutePath(path);
  const normalizedRoot = normalizeAbsolutePath(workspacePath);
  if (!normalizedPath || !normalizedRoot) return null;
  if (normalizedPath.separator !== normalizedRoot.separator) return null;
  const root = normalizedRoot.comparePath;
  const target = normalizedPath.comparePath;
  if (target.length <= root.length) return null;
  if (!target.startsWith(root)) return null;
  const sep = normalizedPath.separator;
  if (target.charAt(root.length) !== sep) return null;
  return normalizedPath.path.slice(normalizedRoot.path.length + 1);
}

function normalizeAbsolutePath(raw: string): NormalizedAbsolutePath | null {
  const trimmed = raw.trim();
  if (!trimmed) return null;
  const windowsDrive = /^([A-Za-z]:)[\\/]/.exec(trimmed);
  if (windowsDrive) {
    const drive = windowsDrive[1].toUpperCase();
    const rest = trimmed.slice(windowsDrive[0].length);
    const path = `${drive}\\${normalizePathSegments(rest.split(/[\\/]+/)).join('\\')}`;
    return {
      path,
      comparePath: path.toLowerCase(),
      separator: '\\',
    };
  }
  if (trimmed.startsWith('/')) {
    const path = `/${normalizePathSegments(trimmed.slice(1).split(/[\\/]+/)).join('/')}`;
    return {
      path,
      comparePath: path,
      separator: '/',
    };
  }
  return null;
}

function normalizePathSegments(segments: string[]): string[] {
  const normalized: string[] = [];
  for (const segment of segments) {
    if (!segment || segment === '.') continue;
    if (segment === '..') {
      if (normalized.length > 0) normalized.pop();
      continue;
    }
    normalized.push(segment);
  }
  return normalized;
}

interface NormalizedRelativePath {
  path: string;
  escapesRoot: boolean;
}

function normalizeRelativePath(raw: string): NormalizedRelativePath {
  const separator = raw.includes('\\') && !raw.includes('/') ? '\\' : '/';
  const normalized: string[] = [];
  let escapesRoot = false;
  for (const segment of raw.split(/[\\/]+/)) {
    if (!segment || segment === '.') continue;
    if (segment === '..') {
      if (normalized.length === 0) {
        escapesRoot = true;
        continue;
      }
      normalized.pop();
      continue;
    }
    normalized.push(segment);
  }
  const path = normalized.length > 0 ? normalized.join(separator) : raw;
  return { path, escapesRoot };
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

/**
 * ToolSearch is the Claude Code 2.1.150+ deferred-tool schema loader.
 * Two query shapes appear in practice:
 *
 *   - `select:<Name>` or `select:<NameA>,<NameB>` — schema hydration
 *     for one or more deferred tools. The model fires this before the
 *     first invocation of each new tool. Render as
 *     "Loaded schema: <Names>" so the row says what got loaded rather
 *     than echoing the raw `select:` query.
 *   - Free-text keyword query — rarer; the model is searching the
 *     deferred-tool catalogue by capability. Render the query verbatim
 *     so users see exactly what the model asked for.
 *
 * Falls back to the empty string when the input is missing or
 * malformed; the caller chains through to the standard preview path.
 */
function skillPreviewFromMeta(itemMeta: Record<string, unknown> | null): string {
  const input = itemMeta?.input;
  if (!input || typeof input !== 'object' || Array.isArray(input)) return '';
  const skill = (input as Record<string, unknown>).skill;
  return typeof skill === 'string' ? skill.trim() : '';
}

/**
 * `SendMessage` addresses another Claude session on this machine through
 * Claude Code's cross-session inbox. The recipient's NAME is the whole
 * point of the row — the body text is already the next thing on screen if
 * the reader opens the card, but who it went to is not recoverable from
 * anywhere else.
 *
 * The CLI accepts `to` / `message` and echoes back its own canonical
 * `recipient` / `content` alongside them (2.1.237, observed on the wire),
 * so both spellings are read. Falls back to the empty string, which lets
 * the caller use the ordinary summary path.
 */
function sendMessagePreview(itemMeta: Record<string, unknown> | null): string {
  const input = itemMeta?.input;
  if (!input || typeof input !== 'object' || Array.isArray(input)) return '';
  const fields = input as Record<string, unknown>;
  const raw = typeof fields.recipient === 'string' ? fields.recipient : fields.to;
  const recipient = typeof raw === 'string' ? raw.trim() : '';
  return recipient ? `To ${recipient}` : '';
}

function toolSearchPreview(itemMeta: Record<string, unknown> | null): string {
  const input = itemMeta?.input;
  if (!input || typeof input !== 'object' || Array.isArray(input)) return '';
  const query = (input as Record<string, unknown>).query;
  if (typeof query !== 'string') return '';
  const trimmed = query.trim();
  if (!trimmed) return '';
  if (trimmed.startsWith('select:')) {
    const names = trimmed
      .slice('select:'.length)
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    if (names.length === 0) return 'Loaded schema';
    return `Loaded schema: ${names.join(', ')}`;
  }
  return trimmed;
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
