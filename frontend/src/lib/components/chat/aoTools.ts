// Presentation of the tools Agent Overflow itself serves to a provider
// (`ao-remote-tools`, `ao-browser-tools`, `ao-thread-tools`,
// `ao-workflow-tools`), so their rows read like native
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
// A thread or workflow tool's reply says what the call touched (the new
// thread's title, a request's token, a run's workflow), and triage copies
// those few fields onto the row as `meta.ao_result`
// (`internal/triage/ao_tool_result.go`), so the header can name them
// before the reply is read. The expanded body decodes the reply itself
// (`aoToolResults.ts`).
//
// The model addresses computers, jobs, pages, threads and runs by id; a
// person knows them by name. Every id is presented through `AoToolNames`,
// which the row builds from what it can see (attached computers, the
// thread's job listing, its live pages, the thread list, the workflow
// runs), and an id whose name is unknown falls back to a short form rather
// than a bare UUID.
//
// Adding an AO MCP server: register it in `AO_TOOL_SERVERS` with its icon,
// family name and per-tool `what` builder. A tool missing from its server's
// table still gets the family icon; its gutter verb is the tool name minus
// the family prefix and its body the compact argument list.

import type { ToolKindIcon } from './toolCardHeader';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import { formatCalendarDate, formatTimeOfDay } from '../../utils/format';

/** Mirrors `internal/app/app_remote_mcp.go#remoteMCPName`. */
export const REMOTE_TOOLS_SERVER = 'ao-remote-tools';

/** Mirrors `internal/app/app_thread_tools_mcp.go#threadMCPName`. */
export const THREAD_TOOLS_SERVER = 'ao-thread-tools';

/** Mirrors `internal/workflowtools/server.go#MCPServerName`. */
export const WORKFLOW_TOOLS_SERVER = 'ao-workflow-tools';

/** Names for the ids a tool input carries; '' when the name is unknown. */
export interface AoToolNames {
  computer(id: string): string;
  job(requestId: string): string;
  page(pageId: string): string;
  /** A thread's title. `id` may be the unambiguous prefix the model typed. */
  thread(id: string): string;
  /** A workflow run's name: its workflow and goal. */
  run(runId: string): string;
}

export const NO_AO_NAMES: AoToolNames = {
  computer: () => '',
  job: () => '',
  page: () => '',
  thread: () => '',
  run: () => '',
};

/**
 * What a thread or workflow tool's reply said, as triage stamped it on the
 * row (`meta.ao_result`). Every field is '' when the reply did not carry it,
 * the call failed, or the row predates the stamp.
 */
export interface AoResultStamp {
  token: string;
  threadId: string;
  title: string;
  computer: string;
  runId: string;
  workflowId: string;
  state: string;
  outcome: string;
}

const NO_RESULT: AoResultStamp = {
  token: '', threadId: '', title: '', computer: '', runId: '', workflowId: '', state: '', outcome: '',
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
  /** What the reply said, from the row's stamp. */
  result: AoResultStamp;
}

/** One line of the expanded body: an input the header does not show. */
export interface AoToolFact {
  label: string;
  value: string;
}

type Input = Record<string, unknown>;

interface AoToolSpec {
  label: string;
  what: (input: Input, names: AoToolNames, result: AoResultStamp) => string;
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
/** One header line. `metaInputLeafRenderCaps.test.ts` holds it under the wire leaf floor. */
export const WHAT_MAX = 160;
/** One fact in the expanded body; longer values fold behind "show more". */
export const FACT_MAX = 2000;

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

/** An id as a person can tell it apart: its first eight characters. */
export function shortId(id: string): string {
  return id.length > SHORT_ID_CHARS ? id.slice(0, SHORT_ID_CHARS) : id;
}

/**
 * A thread by the title the reply carried, else the title this client
 * knows it by, else `thread 1a2b3c4d`. `thread_id` accepts an unambiguous
 * prefix, so the input alone is often eight characters of a UUID.
 */
export function threadName(id: string, names: AoToolNames, title = ''): string {
  if (title) return title;
  if (!id) return '';
  return names.thread(id) || `thread ${shortId(id)}`;
}

function threadRef(input: Input, names: AoToolNames, result: AoResultStamp = NO_RESULT): string {
  const id = str(input, 'thread_id');
  return threadName(id, names, id && result.title ? result.title : '');
}

function threadListRef(input: Input, names: AoToolNames): string {
  const ids = strings(input, 'thread_ids');
  if (ids.length === 1) return threadName(ids[0], names);
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

/** A message to a thread: who it went to, then what it said. */
function toThread(target: string, line: string): string {
  return [target, line].filter(Boolean).join(' · ');
}

/** Which part of a thread a thread_show call read. */
function showWindow(input: Input): string {
  const turns = num(input, 'turns');
  switch (str(input, 'window')) {
    case 'head':
      return `first ${turns ?? 20} turns`;
    case 'last_answer':
      return 'last answer';
    case 'around':
      return `around ${str(input, 'item_id') || 'an item'}`;
    case 'since':
      return `since ${str(input, 'item_id') || str(input, 'since') || 'an item'}`;
    case 'all':
      return 'whole thread';
    default:
      return input.cursor ? 'next page' : `last ${turns ?? 20} turns`;
  }
}

/** What part of an item a thread_item call read. */
function itemRange(input: Input): string {
  const query = str(input, 'query');
  if (query) return `find ${quoted(query)}`;
  const lines = str(input, 'lines');
  if (lines) return `lines ${lines}`;
  const offset = num(input, 'offset');
  return offset !== null ? `from byte ${offset}` : '';
}

/** What a thread_search call looked for: the query, else its filters. */
function searchWhat(input: Input, names: AoToolNames): string {
  const query = str(input, 'query');
  const within = str(input, 'thread_id') ? `in ${threadName(str(input, 'thread_id'), names)}` : '';
  if (query) return [quoted(query), within].filter(Boolean).join(' ');
  if (within) return within;
  const kind = [input.archived === true ? 'archived' : '', str(input, 'state'), str(input, 'provider')];
  const where = [str(input, 'group') ? `in ${str(input, 'group')}` : '', input.spawned_by_me === true ? 'spawned here' : ''];
  if (![...kind, ...where].some(Boolean)) return 'recent threads';
  return [...kind, 'threads', ...where].filter(Boolean).join(' ');
}

/** What a thread_status call asked about. */
function statusWhat(input: Input, names: AoToolNames): string {
  const asked = threadListRef(input, names) || 'all requests';
  const wait = num(input, 'wait_seconds');
  return wait ? `${asked} · wait ${wakeInterval(wait)}` : asked;
}

/** Tool order mirrors `internal/threadtools/schemas.go#ToolNames`. */
const THREAD_TOOLS: Record<string, AoToolSpec> = {
  thread_search: { label: 'search', what: (input, names) => searchWhat(input, names) },
  thread_show: {
    label: 'read',
    what: (input, names, result) => toThread(threadRef(input, names, result), showWindow(input)),
  },
  thread_item: {
    label: 'item',
    what: (input, names, result) => {
      const item = str(input, 'item_id');
      return [threadRef(input, names, result), item ? `item ${shortId(item)}` : '', itemRange(input)]
        .filter(Boolean)
        .join(' · ');
    },
  },
  thread_options: { label: 'options', what: () => 'spawn options' },
  thread_spawn: {
    label: 'spawn',
    what: (input, _names, result) => result.title || str(input, 'title') || firstLine(input, 'prompt'),
  },
  thread_send: {
    label: 'send',
    what: (input, names, result) => toThread(threadRef(input, names, result), firstLine(input, 'message')),
  },
  thread_ask: {
    label: 'ask',
    what: (input, names, result) => toThread(threadRef(input, names, result), firstLine(input, 'question')),
  },
  thread_reply: {
    label: 'reply',
    what: (input, names, result) =>
      toThread(threadName(result.threadId, names), firstLine(input, 'text') || str(input, 'decision')),
  },
  thread_status: { label: 'status', what: (input, names) => statusWhat(input, names) },
  thread_cancel: {
    label: 'cancel',
    what: (input, names) =>
      threadRef(input, names) || str(input, 'label') || (str(input, 'token') ? `request ${shortId(str(input, 'token'))}` : ''),
  },
  thread_update: { label: 'update', what: (input, names) => threadListRef(input, names) },
  thread_group: {
    label: 'group',
    what: (input) => {
      // The schema takes exactly one of rename or delete, and names the
      // group by `group` or `group_id`. `pin` stays for calls recorded in
      // history before groups stopped being pinnable.
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
  thread_loop: { label: 'loop', what: (input) => wakeWhat(input) },
  thread_watch: { label: 'watch', what: (input) => wakeWhat(input) },
};

/** A loop or watch registration: its label and cadence, or the label it stops. */
function wakeWhat(input: Input): string {
  const label = str(input, 'label');
  if (input.stop === true) return `stop ${label}`.trim();
  const every = num(input, 'every_seconds');
  const cadence = input.long_running === true ? 'long-running' : every ? `every ${wakeInterval(every)}` : '';
  return [label, cadence].filter(Boolean).join(' · ');
}

function wakeInterval(seconds: number): string {
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
}

/**
 * A run by the name this client knows it by, else its workflow and a short
 * id when the reply named the workflow, else `run 1a2b3c4d`.
 */
export function runName(id: string, names: AoToolNames, workflowId = ''): string {
  if (!id) return '';
  return names.run(id) || [workflowId, `run ${shortId(id)}`].filter(Boolean).join(' ');
}

function runRef(input: Input, names: AoToolNames, result: AoResultStamp = NO_RESULT): string {
  const id = str(input, 'run_id') || result.runId;
  return runName(id, names, result.runId === id ? result.workflowId : '');
}

/** A run and, when the call names one, the phase it reads. */
function runPhaseRef(input: Input, names: AoToolNames, result: AoResultStamp): string {
  const phase = str(input, 'phase_id');
  return [runRef(input, names, result), phase ? `phase ${phase}` : ''].filter(Boolean).join(' · ');
}

/** A run and where the reply left it: `digest run 1a2b3c4d · running`. */
function runWithState(input: Input, names: AoToolNames, result: AoResultStamp): string {
  return [runRef(input, names, result), result.state].filter(Boolean).join(' · ');
}

function seedNames(input: Input): string {
  const seeds = input.seeds;
  if (!seeds || typeof seeds !== 'object' || Array.isArray(seeds)) return '';
  return Object.keys(seeds).join(', ');
}

const WORKFLOW_TOOLS: Record<string, AoToolSpec> = {
  workflow_list: { label: 'list', what: () => 'workflows' },
  workflow_validate: { label: 'validate', what: (input) => str(input, 'id') || str(input, 'path') },
  workflow_schema: { label: 'schema', what: () => 'workflow schema' },
  workflow_new: {
    label: 'new',
    what: (input) => {
      const id = str(input, 'id');
      const starter = str(input, 'starter');
      return starter ? `${id} from ${starter}` : id;
    },
  },
  run_start: {
    label: 'start',
    what: (input, _names, result) => {
      const goal = firstLine(input, 'goal');
      return [str(input, 'workflow_id'), goal ? quoted(goal) : '', result.state].filter(Boolean).join(' · ');
    },
  },
  run_status: { label: 'status', what: runWithState },
  run_watch: {
    label: 'watch',
    what: (input, names, result) =>
      [runRef(input, names, result), input.tree === true ? 'and its called runs' : ''].filter(Boolean).join(' '),
  },
  run_output: { label: 'output', what: runWithState },
  run_inspect: { label: 'inspect', what: runPhaseRef },
  run_narrative: { label: 'narrative', what: runPhaseRef },
  run_list: { label: 'list', what: (input) => (input.active_only === true ? 'active runs' : 'runs') },
  run_control: {
    label: 'control',
    what: (input, names, result) =>
      [str(input, 'action').replace(/_/g, ' '), runPhaseRef(input, names, result)].filter(Boolean).join(' '),
  },
  run_amend: {
    label: 'amend',
    what: (input, names, result) => [runRef(input, names, result), seedNames(input)].filter(Boolean).join(' · '),
  },
  run_guide: {
    label: 'guide',
    what: (input, names, result) => [runRef(input, names, result), firstLine(input, 'text')].filter(Boolean).join(' · '),
  },
  run_resolve: {
    label: 'resolve',
    what: (input, names, result) => [str(input, 'decision'), runRef(input, names, result)].filter(Boolean).join(' '),
  },
  memory_add: {
    label: 'remember',
    what: (input) => {
      const kind = str(input, 'kind');
      const text = firstLine(input, 'text');
      return kind && text ? `${kind}: ${text}` : kind || text;
    },
  },
  memory_list: { label: 'memory', what: (input, names) => str(input, 'kind') || runRef(input, names) || 'campaign notes' },
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
  [WORKFLOW_TOOLS_SERVER]: {
    family: 'Workflow',
    icon: 'checklist',
    prefix: '',
    tools: WORKFLOW_TOOLS,
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

/** The row's `meta.ao_result` stamp, or empty fields when it has none. */
export function readAoResultStamp(itemMeta: Record<string, unknown> | null): AoResultStamp {
  const stamp = itemMeta?.ao_result;
  if (!stamp || typeof stamp !== 'object' || Array.isArray(stamp)) return NO_RESULT;
  const r = stamp as Input;
  return {
    token: str(r, 'token'),
    threadId: str(r, 'thread_id'),
    title: str(r, 'title'),
    computer: str(r, 'computer'),
    runId: str(r, 'run_id'),
    workflowId: str(r, 'workflow_id'),
    state: str(r, 'state'),
    outcome: str(r, 'outcome'),
  };
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

/**
 * An input value as a person reads it: a list as its items, an object as
 * one `key: value` line per key (the fact keeps newlines), JSON only for
 * what nests deeper.
 */
function factText(value: unknown): string {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map((entry) => (typeof entry === 'string' ? entry : valueText(entry))).join(', ');
  if (value && typeof value === 'object') {
    return Object.entries(value as Input)
      .map(([key, entry]) => `${key}: ${typeof entry === 'string' ? entry : valueText(entry)}`)
      .join('\n');
  }
  return valueText(value);
}

function clip(text: string, max = WHAT_MAX): string {
  const oneLine = text.replace(/\s*\n\s*/g, ' ').trim();
  return oneLine.length <= max ? oneLine : `${oneLine.slice(0, max - 1)}…`;
}

/**
 * A thread_loop, thread_watch or thread_remind call's summary with the next
 * fire time its reply named, which triage stamps on the row as
 * `next_fire_at` (internal/triage/thread_tools_schedule.go). The time is the
 * call's answer, so the line keeps it after the schedule moves on: a fire on
 * the row's own day (`next_fire_from`) reads as a time, any other as a date
 * and time, the same whenever the row is read. The tray shows what is
 * scheduled now.
 */
function withNextFire(what: string, itemMeta: Record<string, unknown> | null): string {
  const at = itemMeta?.next_fire_at;
  if (typeof at !== 'number' || !Number.isFinite(at) || at <= 0) return what;
  const from = itemMeta?.next_fire_from;
  const sameDay = typeof from === 'number' && from > 0 && new Date(at).toDateString() === new Date(from).toDateString();
  const when = sameDay ? formatTimeOfDay(at) : `${formatCalendarDate(at)} ${formatTimeOfDay(at)}`;
  return [what, `next ${when}`].filter(Boolean).join(' · ');
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
  const result = readAoResultStamp(itemMeta);
  const spec = server.tools[mcp.tool];
  const label = spec
    ? spec.label
    : (mcp.tool.startsWith(server.prefix) ? mcp.tool.slice(server.prefix.length) : mcp.tool) || 'tool';
  const fullWhat = withNextFire((spec ? spec.what(input, names, result) : argsText(input)).trim(), itemMeta);
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
    result,
  };
}

// Inputs the header already presents, or ids a person does not read: the
// body names what they refer to instead.
const HIDDEN_FACTS = new Set([
  'computer_id', 'computer_name', 'request_id', 'project_id', 'page_id',
  'command', 'argv', 'interpreter', 'script',
  'thread_id', 'thread_ids', 'run_id', 'token', 'tokens', 'cursor',
  'home_files',
]);

// Inputs a tool's header or decoded reply already says in its own words
// (a read's window, a loop's cadence, the spawned thread's title as its
// link, notify as armed or not), so the input list does not repeat them.
const ALREADY_SHOWN: Record<string, ReadonlySet<string>> = {
  thread_spawn: new Set(['title', 'notify', 'worktree', 'base', 'provider', 'model', 'effort', 'group']),
  thread_send: new Set(['notify']),
  thread_ask: new Set(['notify']),
  thread_search: new Set(['query', 'state', 'group', 'provider', 'spawned_by_me', 'archived']),
  thread_show: new Set(['window', 'turns', 'item_id', 'since']),
  thread_item: new Set(['item_id', 'query', 'lines', 'offset']),
  thread_status: new Set(['wait_seconds']),
  thread_loop: new Set(['label', 'every_seconds', 'stop']),
  thread_watch: new Set(['label', 'every_seconds', 'long_running', 'stop']),
  run_start: new Set(['workflow_id', 'goal']),
};

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
  // One thread or run is named in the header; a list of them is not.
  const threads = strings(input, 'thread_ids');
  if (threads.length > 1) facts.push({ label: 'threads', value: clip(threads.map((t) => threadName(t, names)).join(', '), FACT_MAX) });
  // A standing agent's home files ride a spawn or send as paths; the row
  // counts them rather than listing paths a person does not read.
  const homeFiles = strings(input, 'home_files');
  if (homeFiles.length) facts.push({ label: 'files', value: `${homeFiles.length} file${homeFiles.length === 1 ? '' : 's'} attached` });
  const tokens = [str(input, 'token'), ...strings(input, 'tokens')].filter(Boolean);
  if (tokens.length) facts.push({ label: tokens.length === 1 ? 'request' : 'requests', value: tokens.map(shortId).join(', ') });
  const shown = ALREADY_SHOWN[mcp.tool];
  for (const [key, value] of Object.entries(input)) {
    if (HIDDEN_FACTS.has(key) || value === undefined || value === null || value === '') continue;
    if (shown?.has(key)) continue;
    const text = factText(value);
    facts.push({ label: FACT_LABELS[key] ?? key.replace(/_/g, ' '), value: text.length > FACT_MAX ? `${text.slice(0, FACT_MAX - 1)}…` : text });
  }
  const script = str(input, 'script');
  if (script) facts.push({ label: 'script', value: script.length > FACT_MAX ? `${script.slice(0, FACT_MAX - 1)}…` : script });
  return facts;
}
