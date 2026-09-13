// Presentation of the tools Agent Overflow itself serves to a provider
// (`ao-remote-tools`, `ao-browser-tools`), so their rows read like native
// tool calls: an icon for the family, a verb in the gutter, and the one
// argument a reader wants (the command, the URL, the selector) instead of
// `server.tool(key=value, …)`.
//
// Both providers stamp `meta.mcp = {server, tool}` and `meta.input` (the
// raw arguments) on an MCP tool row (`internal/provider/claude/
// parse_assistant.go`, `internal/provider/codex/protocol_meta.go`). This
// module reads only those two fields, so a tray projection that sets them
// (`internal/app/app_remote_watch.go` `remoteTrayItems`) presents through
// the same table as a live transcript row.
//
// Adding an AO MCP server: register it in `AO_TOOL_SERVERS` with its icon,
// family name and per-tool `what` builder. A tool missing from its server's
// table still gets the family icon; its gutter verb is the tool name minus
// the family prefix and its body the compact argument list.

import type { ToolKindIcon } from './toolCardHeader';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';

/** Mirrors `internal/app/app_remote_mcp.go#remoteMCPName`. */
export const REMOTE_TOOLS_SERVER = 'ao-remote-tools';

export interface AoToolPresentation {
  server: string;
  tool: string;
  icon: ToolKindIcon;
  /** Gutter verb, short enough for the fixed label column ("run", "click"). */
  label: string;
  /** Vocabulary for a collapsed activity run's header ("Remote run"). */
  headerLabel: string;
  /** The argument a reader wants: the command, the URL, the selector. */
  what: string;
  /** The computer the tool acts on, when its input names one. */
  computerId: string;
  /**
   * That computer's name as the projecting backend knew it, when the input
   * carries one (`computer_name`, written by the tray projection). A live
   * call names the computer by id only; the row resolves the name itself
   * when this client is attached there.
   */
  computerName: string;
}

type Input = Record<string, unknown>;

interface AoToolSpec {
  label: string;
  what: (input: Input) => string;
}

interface AoToolServer {
  family: string;
  icon: ToolKindIcon;
  prefix: string;
  tools: Record<string, AoToolSpec>;
  /** The input field naming the computer the tool acts on. */
  computerField?: string;
}

const REMOTE_JOB_ID_CHARS = 8;
const WHAT_MAX = 160;

function str(input: Input, key: string): string {
  const value = input[key];
  return typeof value === 'string' ? value.trim() : '';
}

function num(input: Input, key: string): number | null {
  const value = input[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

function strings(input: Input, key: string): string[] {
  const value = input[key];
  if (!Array.isArray(value)) return [];
  return value.filter((entry): entry is string => typeof entry === 'string');
}

function quoted(text: string): string {
  return JSON.stringify(text);
}

/** `job 98312d67`: the short prefix a reader can match against the tray. */
function jobRef(input: Input): string {
  const id = str(input, 'request_id');
  return id ? `job ${id.slice(0, REMOTE_JOB_ID_CHARS)}` : '';
}

function pageRef(input: Input): string {
  const id = str(input, 'page_id');
  return id ? `page ${id}` : '';
}

function withPage(what: string, input: Input): string {
  return what || pageRef(input) || '';
}

/**
 * The command text of a `remote_run` call: argv joined with shell-style
 * quoting for arguments that carry spaces or quotes, or the interpreter plus
 * `script` for an inline script. Mirrors `remoteJobCommand` in
 * `internal/app/app_remote_watch.go`, which renders the same text for the
 * tray. A tray projection passes it pre-rendered as `input.command`.
 */
export function remoteRunCommand(input: Input): string {
  const command = str(input, 'command');
  if (command) return command;
  const script = typeof input.script === 'string' ? input.script : '';
  let argv = strings(input, 'argv');
  if (script) argv = strings(input, 'interpreter');
  const parts = argv.map((arg) =>
    arg === '' || /[\s"'\\]/.test(arg) ? quoted(arg) : arg,
  );
  if (script) parts.push('script');
  return parts.join(' ');
}

function locatorText(input: Input): string {
  const locator = input.locator;
  if (!locator || typeof locator !== 'object' || Array.isArray(locator)) return '';
  const l = locator as Input;
  const css = str(l, 'css');
  if (css) return css;
  const role = str(l, 'role');
  const name = str(l, 'name');
  if (role) return name ? `${role} ${quoted(name)}` : role;
  for (const key of ['text', 'label', 'placeholder']) {
    const value = str(l, key);
    if (value) return `${key} ${quoted(value)}`;
  }
  return '';
}

const REMOTE_TOOLS: Record<string, AoToolSpec> = {
  remote_computers: { label: 'list', what: () => 'computers' },
  remote_jobs: { label: 'list', what: () => 'remote jobs' },
  remote_run: { label: 'run', what: remoteRunCommand },
  remote_status: { label: 'status', what: jobRef },
  remote_cancel: { label: 'cancel', what: jobRef },
  remote_read_log: { label: 'log', what: jobRef },
  remote_search_log: {
    label: 'search',
    what: (input) => {
      const query = str(input, 'query');
      const job = jobRef(input);
      return query ? `${quoted(query)}${job ? ` in ${job}` : ''}` : job;
    },
  },
  remote_fetch_log: { label: 'fetch', what: (input) => `log of ${jobRef(input)}`.trim() },
  remote_fetch_artifact: {
    label: 'fetch',
    what: (input) => {
      const path = str(input, 'path');
      const job = jobRef(input);
      return path ? `${path}${job ? ` from ${job}` : ''}` : job;
    },
  },
};

const BROWSER_TOOLS: Record<string, AoToolSpec> = {
  browser_open: { label: 'open', what: (input) => str(input, 'url') },
  browser_new_page: { label: 'open', what: () => 'new page' },
  browser_open_file: { label: 'open', what: (input) => str(input, 'path') },
  browser_pages: { label: 'list', what: () => 'pages' },
  browser_select_page: { label: 'select', what: pageRef },
  browser_label_page: {
    label: 'label',
    what: (input) => {
      const label = str(input, 'label');
      const page = pageRef(input);
      return label ? `${page ? `${page} as ` : ''}${quoted(label)}` : page;
    },
  },
  browser_session: { label: 'session', what: (input) => str(input, 'name') },
  browser_visibility: {
    label: 'show',
    what: (input) =>
      input.visible === false ? `hide ${pageRef(input) || 'companion'}` : withPage('', input),
  },
  browser_viewport: {
    label: 'viewport',
    what: (input) => {
      const action = str(input, 'action');
      const width = num(input, 'width');
      const height = num(input, 'height');
      if (action === 'set' && width !== null && height !== null) return `${width}×${height}`;
      return action;
    },
  },
  browser_close_page: { label: 'close', what: pageRef },
  browser_snapshot: { label: 'snapshot', what: pageRef },
  browser_screenshot: {
    label: 'capture',
    what: (input) => withPage(input.full_page === true ? 'full page' : '', input),
  },
  browser_locator: {
    label: 'locate',
    what: (input) => {
      const action = str(input, 'action');
      const target = locatorText(input);
      return [action, target].filter(Boolean).join(' ');
    },
  },
  browser_click: { label: 'click', what: (input) => str(input, 'selector') },
  browser_pointer: {
    label: 'pointer',
    what: (input) => {
      const action = str(input, 'action');
      const x = num(input, 'x');
      const y = num(input, 'y');
      const at = x !== null && y !== null ? `${x},${y}` : '';
      return [action, at].filter(Boolean).join(' at ');
    },
  },
  browser_dom: {
    label: 'dom',
    what: (input) => {
      const action = str(input, 'action');
      const node = str(input, 'node_id');
      return [action, node ? `node ${node}` : ''].filter(Boolean).join(' ');
    },
  },
  browser_type: {
    label: 'type',
    what: (input) => {
      const text = str(input, 'text');
      const selector = str(input, 'selector');
      return [text ? quoted(text) : '', selector ? `into ${selector}` : ''].filter(Boolean).join(' ');
    },
  },
  browser_press: {
    label: 'press',
    what: (input) => str(input, 'key') || strings(input, 'keys').join('+'),
  },
  browser_scroll: {
    label: 'scroll',
    what: (input) => {
      const selector = str(input, 'selector');
      const x = num(input, 'x') ?? 0;
      const y = num(input, 'y');
      const by = y !== null ? `by ${x !== 0 ? `${x},${y}` : y}` : '';
      return [selector, by].filter(Boolean).join(' ');
    },
  },
  browser_wait: {
    label: 'wait',
    what: (input) => {
      const ms = num(input, 'milliseconds');
      if (ms !== null) return `${ms} ms`;
      const url = str(input, 'url');
      if (url) return `for ${url}`;
      const target = str(input, 'selector') || locatorText(input);
      const state = str(input, 'state') || str(input, 'load_state');
      return [target ? `for ${target}` : '', state].filter(Boolean).join(' ');
    },
  },
  browser_history: { label: 'history', what: (input) => str(input, 'action') },
  browser_evaluate: { label: 'eval', what: (input) => str(input, 'expression') },
  browser_evaluate_readonly: { label: 'eval', what: (input) => str(input, 'expression') },
  browser_clipboard: {
    label: 'clipboard',
    what: (input) => {
      const action = str(input, 'action');
      const text = str(input, 'text');
      return [action, text ? quoted(text) : ''].filter(Boolean).join(' ');
    },
  },
  browser_console_logs: { label: 'console', what: (input) => str(input, 'filter') },
  browser_downloads: { label: 'download', what: (input) => str(input, 'action') },
  browser_assets: { label: 'assets', what: (input) => str(input, 'action') },
};

export const AO_TOOL_SERVERS: Record<string, AoToolServer> = {
  [REMOTE_TOOLS_SERVER]: {
    family: 'Remote',
    icon: 'monitor',
    prefix: 'remote_',
    tools: REMOTE_TOOLS,
    computerField: 'computer_id',
  },
  [BROWSER_TOOLS_SERVER]: {
    family: 'Browser',
    icon: 'globe',
    prefix: 'browser_',
    tools: BROWSER_TOOLS,
  },
};

function readMcp(itemMeta: Record<string, unknown> | null): { server: string; tool: string } | null {
  const mcp = itemMeta?.mcp;
  if (!mcp || typeof mcp !== 'object' || Array.isArray(mcp)) return null;
  const record = mcp as Record<string, unknown>;
  const server = typeof record.server === 'string' ? record.server : '';
  const tool = typeof record.tool === 'string' ? record.tool : '';
  return server ? { server, tool } : null;
}

function readInput(itemMeta: Record<string, unknown> | null): Input {
  const input = itemMeta?.input;
  return input && typeof input === 'object' && !Array.isArray(input) ? (input as Input) : {};
}

/** Compact `key=value, …` argument list for a tool the table does not know. */
function argsText(input: Input): string {
  return Object.entries(input)
    .map(([key, value]) => {
      if (typeof value === 'string') return `${key}=${quoted(value)}`;
      if (typeof value === 'number' || typeof value === 'boolean') return `${key}=${value}`;
      try {
        return `${key}=${JSON.stringify(value)}`;
      } catch {
        return key;
      }
    })
    .join(', ');
}

function clip(text: string): string {
  const oneLine = text.replace(/\s*\n\s*/g, ' ').trim();
  return oneLine.length <= WHAT_MAX ? oneLine : `${oneLine.slice(0, WHAT_MAX - 1)}…`;
}

/** Whether the row is a tool one of AO's own MCP servers served. */
export function isAoToolMeta(itemMeta: Record<string, unknown> | null): boolean {
  const mcp = readMcp(itemMeta);
  return mcp !== null && mcp.server in AO_TOOL_SERVERS;
}

/**
 * The presentation for an AO tool row, or null when the row is not one
 * (a native tool, or a user-configured MCP server).
 */
export function aoToolPresentation(itemMeta: Record<string, unknown> | null): AoToolPresentation | null {
  const mcp = readMcp(itemMeta);
  if (!mcp) return null;
  const server = AO_TOOL_SERVERS[mcp.server];
  if (!server) return null;
  const input = readInput(itemMeta);
  const spec = server.tools[mcp.tool];
  const label = spec
    ? spec.label
    : (mcp.tool.startsWith(server.prefix) ? mcp.tool.slice(server.prefix.length) : mcp.tool) || 'tool';
  const what = spec ? spec.what(input) : argsText(input);
  return {
    server: mcp.server,
    tool: mcp.tool,
    icon: server.icon,
    label,
    headerLabel: `${server.family} ${label}`,
    what: clip(what),
    computerId: server.computerField ? str(input, server.computerField) : '',
    computerName: server.computerField ? str(input, 'computer_name') : '',
  };
}
