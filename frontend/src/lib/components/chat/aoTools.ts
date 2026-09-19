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
// The model addresses computers, jobs and pages by id; a person knows them
// by name. Every id in an input is presented through `AoToolNames`, which
// the row builds from what it can see (attached computers, the thread's
// job listing, its live pages), and an id whose name is unknown falls back
// to a short form rather than a bare UUID.
//
// Adding an AO MCP server: register it in `AO_TOOL_SERVERS` with its icon,
// family name and per-tool `what` builder. A tool missing from its server's
// table still gets the family icon; its gutter verb is the tool name minus
// the family prefix and its body the compact argument list.

import type { ToolKindIcon } from './toolCardHeader';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import { formatDurationMs } from '../../utils/format';

/** Mirrors `internal/app/app_remote_mcp.go#remoteMCPName`. */
export const REMOTE_TOOLS_SERVER = 'ao-remote-tools';

/** Mirrors `internal/app/app_thread_tools_mcp.go#threadMCPName`. */
export const THREAD_TOOLS_SERVER = 'ao-thread-tools';

/** Names for the ids a tool input carries; '' when the name is unknown. */
export interface AoToolNames {
  computer(id: string): string;
  job(requestId: string): string;
  page(pageId: string): string;
}

export const NO_AO_NAMES: AoToolNames = {
  computer: () => '',
  job: () => '',
  page: () => '',
};

export interface AoToolPresentation {
  server: string;
  tool: string;
  icon: ToolKindIcon;
  /** Gutter verb, short enough for the fixed label column ("run", "click"). */
  label: string;
  /** Vocabulary for a collapsed activity run's header ("Remote run"). */
  headerLabel: string;
  /** The argument a reader wants: the command, the URL, the selector. One bounded line. */
  what: string;
  /** `what` before clipping; differs only when the header could not show it all. */
  fullWhat: string;
  /** The computer the tool acts on, when its input names one. */
  computerId: string;
  /** That computer's name, from the names given or the projection's own `computer_name`. */
  computerName: string;
  /** The remote job the tool addresses, when its input names one. */
  requestId: string;
}

/** One line of the expanded body: an input the header does not show. */
export interface AoToolFact {
  label: string;
  value: string;
}

type Input = Record<string, unknown>;

interface AoToolSpec {
  label: string;
  what: (input: Input, names: AoToolNames) => string;
}

interface AoToolServer {
  family: string;
  icon: ToolKindIcon;
  prefix: string;
  tools: Record<string, AoToolSpec>;
  /** The input field naming the computer the tool acts on. */
  computerField?: string;
}

const SHORT_ID_CHARS = 8;
const WHAT_MAX = 160;
const FACT_MAX = 2000;

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

/** A job by its label, or `job 98312d67` when this client cannot name it. */
export function jobName(requestId: string, names: AoToolNames): string {
  if (!requestId) return '';
  return names.job(requestId) || `job ${requestId.slice(0, SHORT_ID_CHARS)}`;
}

/** A computer by name, or `computer c0ffee11` when this client cannot name it. */
export function computerName(id: string, names: AoToolNames): string {
  if (!id) return '';
  return names.computer(id) || `computer ${id.slice(0, SHORT_ID_CHARS)}`;
}

function jobRef(input: Input, names: AoToolNames): string {
  return jobName(str(input, 'request_id'), names);
}

/** A page by its label or title; a page nobody named is just "page". */
function pageRef(input: Input, names: AoToolNames): string {
  const id = str(input, 'page_id');
  if (!id) return '';
  return names.page(id) || 'page';
}

function withPage(what: string, input: Input, names: AoToolNames): string {
  return what || pageRef(input, names) || '';
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
    what: (input, names) => {
      const query = str(input, 'query');
      const job = jobRef(input, names);
      return query ? `${quoted(query)}${job ? ` in ${job}` : ''}` : job;
    },
  },
  remote_fetch_log: { label: 'fetch', what: (input, names) => `log of ${jobRef(input, names)}`.trim() },
  remote_fetch_artifact: {
    label: 'fetch',
    what: (input, names) => {
      const path = str(input, 'path');
      const job = jobRef(input, names);
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
    what: (input, names) => {
      const label = str(input, 'label');
      const page = pageRef(input, names);
      return label ? `${page ? `${page} as ` : ''}${quoted(label)}` : page;
    },
  },
  browser_session: { label: 'session', what: (input) => str(input, 'name') },
  browser_visibility: {
    label: 'show',
    what: (input, names) =>
      input.visible === false ? `hide ${pageRef(input, names) || 'companion'}` : withPage('', input, names),
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
    what: (input, names) => withPage(input.full_page === true ? 'full page' : '', input, names),
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


/**
 * A thread by the title the result carried, else by the reference the call
 * made. `thread_id` accepts an unambiguous prefix, so the input alone is
 * often eight characters of a UUID; `threadResultTitle` recovers the name
 * from the reply and the row prefers it.
 */
function threadRef(input: Input): string {
  const id = str(input, 'thread_id');
  if (!id) return '';
  return id.length > SHORT_ID_CHARS ? `thread ${id.slice(0, SHORT_ID_CHARS)}` : `thread ${id}`;
}

function threadListRef(input: Input): string {
  const ids = strings(input, 'thread_ids');
  if (ids.length === 1) return threadRef({ thread_id: ids[0] });
  if (ids.length > 1) return `${ids.length} threads`;
  const tokens = strings(input, 'tokens');
  if (tokens.length) return `${tokens.length} request${tokens.length === 1 ? '' : 's'}`;
  return '';
}

/** The first line of a free-text argument, which is what the row shows. */
function firstLine(input: Input, key: string): string {
  const text = str(input, key);
  if (!text) return '';
  const [line] = text.split('\n');
  return line.trim();
}

function withThread(what: string, input: Input): string {
  const thread = threadRef(input);
  if (!what) return thread;
  return thread ? `${what} → ${thread}` : what;
}

/** Tool order mirrors `internal/threadtools/schemas.go#ToolNames`. */
const THREAD_TOOLS: Record<string, AoToolSpec> = {
  thread_search: {
    label: 'search',
    what: (input) => {
      const query = str(input, 'query');
      return query ? quoted(query) : threadRef(input) || 'threads';
    },
  },
  thread_show: { label: 'read', what: (input) => threadRef(input) },
  thread_item: {
    label: 'item',
    what: (input) => {
      const item = str(input, 'item_id');
      const thread = threadRef(input);
      return item ? `${item}${thread ? ` in ${thread}` : ''}` : thread;
    },
  },
  thread_options: { label: 'options', what: () => 'spawn options' },
  thread_spawn: { label: 'spawn', what: (input) => firstLine(input, 'prompt') },
  thread_send: { label: 'send', what: (input) => withThread(firstLine(input, 'message'), input) },
  thread_ask: { label: 'ask', what: (input) => withThread(firstLine(input, 'question'), input) },
  thread_reply: { label: 'reply', what: (input) => firstLine(input, 'text') },
  thread_status: { label: 'status', what: (input) => threadListRef(input) || 'requests' },
  thread_cancel: {
    label: 'cancel',
    what: (input) => threadRef(input) || (str(input, 'token') ? 'request' : ''),
  },
  thread_update: { label: 'update', what: (input) => threadListRef(input) },
  thread_group: {
    label: 'group',
    what: (input) => {
      // The schema takes exactly one of rename, pin or delete, and names
      // the group by `group` or `group_id`.
      const name = str(input, 'group') || str(input, 'group_id');
      const rename = str(input, 'rename');
      if (rename) return `rename ${name || 'group'} → ${rename}`;
      const pin = str(input, 'pin');
      if (pin) return `pin ${name || 'group'} ${pin}`;
      if (input.delete === true) return `delete ${name || 'group'}`;
      return name;
    },
  },
  thread_remind: { label: 'remind', what: (input) => firstLine(input, 'note') },
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
  [THREAD_TOOLS_SERVER]: {
    family: 'Thread',
    icon: 'speech-bubble',
    prefix: 'thread_',
    tools: THREAD_TOOLS,
    computerField: 'computer_id',
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
    .map(([key, value]) => `${key}=${valueText(value)}`)
    .join(', ');
}

function valueText(value: unknown): string {
  if (typeof value === 'string') return quoted(value);
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    return String(value);
  }
}

function clip(text: string, max = WHAT_MAX): string {
  const oneLine = text.replace(/\s*\n\s*/g, ' ').trim();
  return oneLine.length <= max ? oneLine : `${oneLine.slice(0, max - 1)}…`;
}

/** Whether the row is a tool one of AO's own MCP servers served. */
export function isAoToolMeta(itemMeta: Record<string, unknown> | null): boolean {
  return aoToolServer(itemMeta) !== null;
}

/** The AO server a row's tool belongs to, or null for any other row. */
export function aoToolServer(itemMeta: Record<string, unknown> | null): string | null {
  const mcp = readMcp(itemMeta);
  return mcp !== null && mcp.server in AO_TOOL_SERVERS ? mcp.server : null;
}

/**
 * The presentation for an AO tool row, or null when the row is not one
 * (a native tool, or a user-configured MCP server).
 */
export function aoToolPresentation(
  itemMeta: Record<string, unknown> | null,
  names: AoToolNames = NO_AO_NAMES,
): AoToolPresentation | null {
  const mcp = readMcp(itemMeta);
  if (!mcp) return null;
  const server = AO_TOOL_SERVERS[mcp.server];
  if (!server) return null;
  const input = readInput(itemMeta);
  const spec = server.tools[mcp.tool];
  const label = spec
    ? spec.label
    : (mcp.tool.startsWith(server.prefix) ? mcp.tool.slice(server.prefix.length) : mcp.tool) || 'tool';
  const fullWhat = (spec ? spec.what(input, names) : argsText(input)).trim();
  const computerId = server.computerField ? str(input, server.computerField) : '';
  return {
    server: mcp.server,
    tool: mcp.tool,
    icon: server.icon,
    label,
    headerLabel: `${server.family} ${label}`,
    what: clip(fullWhat),
    fullWhat,
    computerId,
    computerName: computerId ? names.computer(computerId) || str(input, 'computer_name') : '',
    requestId: server.computerField ? str(input, 'request_id') : '',
  };
}

// Inputs the header already presents, or ids a person does not read: the
// body names what they refer to instead.
const HIDDEN_FACTS = new Set([
  'computer_id', 'computer_name', 'request_id', 'project_id', 'page_id',
  'command', 'argv', 'interpreter', 'script',
]);

const FACT_LABELS: Record<string, string> = {
  workspace_path: 'workspace',
  timeout_seconds: 'timeout',
  wait_seconds: 'wait',
  max_output_bytes: 'output limit',
  max_bytes: 'read limit',
  full_page: 'full page',
  node_id: 'node',
  load_state: 'load state',
};

/**
 * What the expanded body of an AO tool row lists beside its result: the
 * computer, job or page by name, every other input the header left out,
 * and the script when the command runs one. Empty for a row whose header
 * already says everything.
 */
export function aoToolFacts(
  itemMeta: Record<string, unknown> | null,
  names: AoToolNames = NO_AO_NAMES,
): AoToolFact[] {
  const mcp = readMcp(itemMeta);
  if (!mcp || !(mcp.server in AO_TOOL_SERVERS)) return [];
  const input = readInput(itemMeta);
  const facts: AoToolFact[] = [];
  const computer = str(input, 'computer_id');
  if (computer) facts.push({ label: 'computer', value: names.computer(computer) || str(input, 'computer_name') || computer });
  const request = str(input, 'request_id');
  if (request && mcp.tool !== 'remote_run') facts.push({ label: 'job', value: names.job(request) || request });
  const page = str(input, 'page_id');
  if (page) facts.push({ label: 'page', value: names.page(page) || page });
  for (const [key, value] of Object.entries(input)) {
    if (HIDDEN_FACTS.has(key) || value === undefined || value === null || value === '') continue;
    const text = typeof value === 'string' ? value : valueText(value);
    facts.push({ label: FACT_LABELS[key] ?? key.replace(/_/g, ' '), value: clip(text, FACT_MAX) });
  }
  const script = str(input, 'script');
  if (script) facts.push({ label: 'script', value: script.length > FACT_MAX ? `${script.slice(0, FACT_MAX - 1)}…` : script });
  return facts;
}

/** A remote tool reply as its row shows it: the outcome, then the output. */
export interface RemoteResultView {
  /** `succeeded · exit 0 · 12.0s`, `running in the background`, `failed · exit 1`. */
  outcome: string;
  /** The destination's own error, when the reply carries one. */
  error: string;
  /** The oldest output the reply carried, when the tail did not reach the start. */
  head: string;
  /** The newest output the reply carried. */
  output: string;
  /** Guidance the reply carried about output that is not here. */
  hint: string;
}

/**
 * Reads a `remote_run`, `remote_status` or `remote_cancel` reply
 * (`internal/app/app_remote_mcp_result.go` remoteMCPResult) for the row's
 * body. Null for any other text, which renders as it is.
 */
export function remoteResultView(data: string): RemoteResultView | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
  const reply = parsed as Input;
  const state = str(reply, 'state');
  if (!state || typeof reply.id !== 'string') return null;
  const parts = [state === 'running' && reply.backgrounded === true ? 'running in the background' : state];
  const exit = num(reply, 'exitCode');
  if (state !== 'running' && exit !== null && exit >= 0) parts.push(`exit ${exit}`);
  const started = num(reply, 'startedAt');
  const finished = num(reply, 'finishedAt');
  if (started !== null && finished !== null && finished > 0 && finished >= started) {
    parts.push(formatDurationMs(finished - started));
  }
  return {
    outcome: parts.join(' · '),
    error: str(reply, 'error'),
    head: typeof reply.outputHead === 'string' ? reply.outputHead : '',
    output: typeof reply.output === 'string' ? reply.output : '',
    hint: str(reply, 'outputHint'),
  };
}

/** A thread tool reply as its row shows it: what the call touched. */
export interface ThreadResultView {
  /** The thread's title, when the reply names one. */
  title: string;
  /** Its state, when the reply carries one ("running", "idle"). */
  state: string;
  /** A one-line count for a reply that names many threads or rows. */
  summary: string;
}

/**
 * Reads a thread-tools reply (`internal/threadtools`) for the row's body.
 * The header can only show the arguments, and `thread_id` accepts a
 * prefix, so the thread's NAME is only ever in the result. Null for a
 * reply that names no thread, which renders as it is.
 */
export function threadResultView(data: string): ThreadResultView | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
  const reply = parsed as Input;
  const title = str(reply, 'title');
  const state = str(reply, 'state');
  const summary = threadRowSummary(reply);
  if (!title && !state && !summary) return null;
  return { title, state, summary };
}

/** "3 threads" for a search page, counting both result shapes. */
function threadRowSummary(reply: Input): string {
  let rows = Array.isArray(reply.rows) ? reply.rows.length : 0;
  const computers = reply.computers;
  if (Array.isArray(computers)) {
    for (const group of computers) {
      if (group && typeof group === 'object' && Array.isArray((group as Input).rows)) {
        rows += ((group as Input).rows as unknown[]).length;
      }
    }
  } else if (!Array.isArray(reply.rows)) {
    return '';
  }
  return `${rows} thread${rows === 1 ? '' : 's'}`;
}
