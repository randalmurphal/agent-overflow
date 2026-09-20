// Scenario framing and wire narrowing shared by the thread-tools specs.
//
// A thread-tools spec drives the app through a mock provider that makes
// REAL MCP calls against `ao-thread-tools` (the scenario `mcpCall` /
// `mcpList` steps, docs/architecture/agent-harness.md). Everything here
// exists so a spec states which tools a turn calls and what the thread
// then says, instead of hand-writing provider frames per case.
//
// Scenario rules are scoped by the mock's cwd, which is the thread's
// workspace: two threads that must run different scripts have to live in
// different projects. Every helper takes that cwd explicitly.

import { expect } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';

export const THREAD_TOOLS_SERVER = 'ao-thread-tools';

export type ProviderName = 'claude' | 'codex';

/** One tool call a turn makes. */
export interface ToolCall {
  tool: string;
  args?: Record<string, unknown>;
  toolUseId?: string;
  /** Bounds the HTTP call. A parked wait needs its own value. */
  timeoutMs?: number;
}

/** A step inside one scripted turn. */
export type TurnStep =
  | { call: ToolCall }
  | { list: { server?: string; timeoutMs?: number } }
  | { capture: { var: string; from: string; pattern: string } }
  | { emitLines: string[] }
  /** Park the turn until the test advances the named gate. */
  | { gate: string }
  /** Ask the app for a decision and branch on it. */
  | { approval: ApprovalRequest }
  | { delayMs: number };

/** One provider approval a turn raises, with the branches it takes. */
export interface ApprovalRequest {
  toolName: string;
  input?: Record<string, unknown>;
  toolUseId?: string;
  onAllow?: TurnStep[];
  onDeny?: TurnStep[];
  timeoutMs?: number;
}

/** One scripted turn: its steps, then the assistant text it ends with. */
export interface ScriptedTurn {
  steps?: TurnStep[];
  /** Final assistant text. Empty keeps the turn silent but complete. */
  text?: string;
}

/**
 * The token line every agent-written message carries
 * (internal/threadtools/footer.go). A responder scenario captures the
 * token out of it the way a model reads it off the footer.
 */
export const FOOTER_TOKEN_PATTERN = 'thread_reply with token (\\S+),';

/** `thread_spawn` / `thread_send` / `thread_ask` answer JSON. */
export const RESULT_TOKEN_PATTERN = '"token":"([^"]+)"';
/** The thread id in a spawn answer. */
export const RESULT_THREAD_PATTERN = '"thread_id":"([^"]+)"';

function jsonString(value: string): string {
  return JSON.stringify(value);
}

function claudeTurnLines(text: string): string[] {
  const id = 'msg-${TURN}';
  if (text === '') return ['{"type":"result","subtype":"success","is_error":false}'];
  return [
    `{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"${id}","role":"assistant"}}}`,
    '{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}',
    `{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":${jsonString(text)}}}}`,
    '{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}',
    '{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}',
    `{"type":"assistant","message":{"id":"${id}","role":"assistant","content":[{"type":"text","text":${jsonString(text)}}]}}`,
    '{"type":"result","subtype":"success","is_error":false}',
  ];
}

function codexTurnLines(text: string): string[] {
  const lines: string[] = [];
  if (text !== '') {
    lines.push(
      `{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":{"type":"agentMessage","id":"msg-\${TURN}","status":"inProgress","text":""}}}`,
      `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","itemId":"msg-\${TURN}","delta":${jsonString(text)}}}`,
      `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":{"type":"agentMessage","id":"msg-\${TURN}","status":"completed","text":${jsonString(text)}}}}`,
    );
  }
  lines.push(
    '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}',
  );
  return lines;
}

function stepJSON(step: TurnStep): Record<string, unknown> {
  if ('call' in step) {
    const { tool, args, toolUseId, timeoutMs } = step.call;
    return {
      mcpCall: {
        server: THREAD_TOOLS_SERVER,
        tool,
        args: args ?? {},
        ...(toolUseId ? { toolUseId } : {}),
        // A tool that parks needs a bound above its own wait; the
        // default is a minute (scenario.DefaultMcpCallTimeoutMs).
        ...(timeoutMs ? { timeoutMs } : {}),
      },
    };
  }
  if ('list' in step) {
    return {
      mcpList: {
        server: step.list.server ?? THREAD_TOOLS_SERVER,
        ...(step.list.timeoutMs ? { timeoutMs: step.list.timeoutMs } : {}),
      },
    };
  }
  if ('capture' in step) return { capture: step.capture };
  if ('gate' in step) return { waitSignal: { name: step.gate } };
  if ('approval' in step) {
    const { toolName, input, toolUseId, onAllow, onDeny, timeoutMs } = step.approval;
    return {
      approval: {
        toolName,
        ...(input ? { input } : {}),
        ...(toolUseId ? { toolUseId } : {}),
        ...(onAllow ? { onAllow: onAllow.map(stepJSON) } : {}),
        ...(onDeny ? { onDeny: onDeny.map(stepJSON) } : {}),
        ...(timeoutMs ? { timeoutMs } : {}),
      },
    };
  }
  if ('emitLines' in step) return { emit: { lines: step.emitLines } };
  return { delayMs: step.delayMs };
}

/**
 * A scenario whose turns run thread tools and then answer.
 *
 * Claude frames its own assistant message; Codex opens the turn with
 * `turn/started` before any item, so the tool rows land inside a turn the
 * app has opened.
 */
export function threadToolsScenario(opts: {
  name: string;
  provider: ProviderName;
  turns: ScriptedTurn[];
  afterTurns?: 'repeatLast' | 'silent' | 'exit';
}): Record<string, unknown> {
  const turns = opts.turns.map((turn, index) => {
    const steps: Array<Record<string, unknown>> = [];
    if (opts.provider === 'codex') {
      steps.push({
        emit: {
          lines: [
            '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}',
          ],
        },
      });
    }
    for (const step of turn.steps ?? []) steps.push(stepJSON(step));
    const text = turn.text ?? '';
    const lines = opts.provider === 'claude' ? claudeTurnLines(text) : codexTurnLines(text);
    steps.push({ emit: { lines } });
    return { label: `${opts.name}-${index + 1}`, steps };
  });
  return {
    version: 1,
    name: opts.name,
    description: `thread tools: ${opts.name}`,
    provider: opts.provider,
    turns,
    afterTurns: opts.afterTurns ?? 'repeatLast',
  };
}

/** A scenario that answers with text and calls nothing. */
export function plainScenario(opts: {
  name: string;
  provider: ProviderName;
  texts: string[];
  afterTurns?: 'repeatLast' | 'silent' | 'exit';
}): Record<string, unknown> {
  return threadToolsScenario({
    name: opts.name,
    provider: opts.provider,
    turns: opts.texts.map((text) => ({ text })),
    afterTurns: opts.afterTurns,
  });
}

/** Install a cwd-scoped rule. Every thread-tools spec scopes by workspace. */
export async function setScenario(
  harness: HarnessApp,
  cwd: string,
  scenario: Record<string, unknown>,
): Promise<void> {
  await harness.rpc('HarnessSetScenario', { cwd, scenario });
}

/** The tool answer, decoded. A refusal carries `isError` and prose. */
export interface ToolAnswer<T = Record<string, unknown>> {
  isError: boolean;
  /** Raw answer text: a tool's JSON, or a refusal's prose. */
  text: string;
  /** Parsed JSON, or undefined for a refusal (which is prose). */
  value?: T;
}

/**
 * Await one tool's answer on the mock control channel.
 *
 * The answer is the only place a refusal's text exists: the tool result
 * frame carries it to the model, and the timeline shows a failed tool
 * row without the prose.
 */
export async function awaitToolAnswer<T = Record<string, unknown>>(
  harness: HarnessApp,
  filter: { tool: string; cwd?: string; mockId?: string; timeoutMs?: number },
): Promise<ToolAnswer<T>> {
  const event = await harness.awaitMcpResult({
    server: THREAD_TOOLS_SERVER,
    tool: filter.tool,
    cwd: filter.cwd,
    mockId: filter.mockId,
    timeoutMs: filter.timeoutMs,
  });
  const text = event.report.result ?? '';
  const answer: ToolAnswer<T> = { isError: Boolean(event.report.isError), text };
  if (!answer.isError) {
    try {
      answer.value = JSON.parse(text) as T;
    } catch {
      // A successful tool always answers JSON; leaving value unset makes
      // the spec's own assertion the failure rather than a parse throw.
    }
  }
  return answer;
}

/** Release one parked `gate` step. */
export async function advanceGate(
  harness: HarnessApp,
  mockId: string,
  name: string,
): Promise<void> {
  await harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name });
}

/** Wait until a mock parks on the named gate. */
export async function awaitGate(
  harness: HarnessApp,
  name: string,
  cwd?: string,
): Promise<{ mockId: string }> {
  return await harness.waitForEvent<{ mockId: string; cwd: string; report: { kind: string; detail?: string } }>(
    'harness:mock',
    (ev) =>
      ev.report.kind === 'waiting_signal' &&
      ev.report.detail === name &&
      (cwd === undefined || ev.cwd === cwd),
  );
}

/** Thread row shape the specs read back through HarnessListThreadRows. */
export interface ThreadRow {
  id: string;
  title: string;
  mode: string;
  archived: boolean;
  projectId: string;
  groupId?: string;
  pinnedAt?: number | null;
  pinGroup?: number | null;
  workspacePath: string;
  worktreePath?: string;
  branch?: string;
  provider: string;
  model: string;
  runtimeMode: string;
  reasoningEffort: string;
  forkedFromThreadId?: string;
}

/** Every non-archived thread row the backend holds, drafts included. */
export async function threadRows(harness: HarnessApp): Promise<ThreadRow[]> {
  return await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
}

/**
 * One filler line of a seeded payload's padding
 * (internal/harnessrpc.appendSeedPad). Every line is this wide, whichever
 * side it is on, so a fixture's own data sits at an offset a spec can
 * compute instead of searching for.
 */
const PAD_LINE_BYTES = 'pad before 000001: generated filler line\n'.length;

/** Padding of `bytes` rounds up to whole filler lines. */
function paddedBytes(bytes: number): number {
  return Math.ceil(bytes / PAD_LINE_BYTES) * PAD_LINE_BYTES;
}

/** A seeded thread plus what a spec has to know about it. */
export interface SeededThreadFixture {
  /** The `threads` entry of a HarnessSeed spec. */
  thread: Record<string, unknown>;
}

/** A thread near 38k items, with a marker at its head, middle and tail. */
export interface BigThreadFixture extends SeededThreadFixture {
  /** Items the thread holds, all told. */
  items: number;
  /** Turns the thread holds. */
  turns: number;
  /** Phrase in the first turn. */
  head: string;
  /** Phrase in the middle turn, which is what `around` is anchored on. */
  anchor: string;
  /** Phrase in the last turn. */
  tail: string;
  /** Items in the five-turn window `around` the anchor with context 2. */
  aroundItems: number;
}

/**
 * A thread of 38006 items: two blocks of a thousand bulk turns with one
 * distinctive turn at the head, the middle and the tail. `tag` makes every
 * marker unique, so a search that fans out over two computers can name the
 * one thread it means.
 */
export function bigThreadFixture(tag: string, title: string): BigThreadFixture {
  const bulkTurns = 1000;
  const bulkItems = 18;
  const bulk = {
    userText: 'keep sweeping',
    items: Array.from({ length: bulkItems }, (_unused, index) => ({
      kind: 'assistant_text',
      summary: `swept region ${index + 1}`,
    })),
    repeat: bulkTurns,
  };
  const head = `${tag}HEAD`;
  const anchor = `${tag}ANOMALY`;
  const tail = `${tag}TAIL`;
  return {
    thread: {
      title,
      provider: 'claude',
      turns: [
        {
          userText: 'begin the sweep',
          items: [{ kind: 'assistant_text', summary: `${head} sweep started` }],
        },
        bulk,
        {
          userText: 'what went wrong in the middle',
          items: [{ kind: 'assistant_text', summary: `${anchor} the scheduler stalled at tick 41` }],
        },
        bulk,
        {
          userText: 'wrap up',
          items: [{ kind: 'assistant_text', summary: `${tail} sweep complete` }],
        },
      ],
    },
    // Three single turns of two rows each, plus two blocks of a thousand
    // turns carrying a user row and eighteen answers.
    items: 3 * 2 + 2 * bulkTurns * (bulkItems + 1),
    turns: 3 + 2 * bulkTurns,
    head,
    anchor,
    tail,
    // context 2 takes the anchor turn and two bulk turns on each side.
    aroundItems: 2 + 4 * (bulkItems + 1),
  };
}

/** A thread whose one tool call holds a multi-megabyte output. */
export interface BigItemFixture extends SeededThreadFixture {
  /** The whole payload's size in bytes. */
  size: number;
  /** The phrase buried in the middle of it. */
  needle: string;
  /** Byte offset of `needle` inside the payload. */
  needleOffset: number;
}

/**
 * A tool output of about 4MB with one phrase buried halfway through it,
 * which is the only shape `thread_item`'s query and range reads exist for.
 */
export function bigItemFixture(tag: string, title: string): BigItemFixture {
  const needle = `${tag}NEEDLE panic: tokenizer overran the escape\n`;
  const padBefore = 2_000_000;
  const padAfter = 2_000_000;
  return {
    thread: {
      title,
      provider: 'codex',
      turns: [
        {
          userText: 'run the fuzzer and tell me what it found',
          items: [
            { kind: 'assistant_text', summary: 'It crashed; the log is long.' },
            {
              kind: 'tool_call',
              toolName: 'Bash',
              summary: 'go test -run Fuzz ./internal/lex',
              payload: { kind: 'tool_call_result', data: needle, padBefore, padAfter },
            },
          ],
        },
      ],
    },
    size: paddedBytes(padBefore) + needle.length + paddedBytes(padAfter),
    needle,
    needleOffset: paddedBytes(padBefore),
  };
}

/**
 * Wait for one thread's turn to settle.
 *
 * A thread that is still working refuses the next send, so a test that
 * drives several turns waits here between them. The thread id matters:
 * a harness shared by a whole spec file carries the completions of every
 * earlier test, and an unfiltered wait would consume one of those and
 * send into a turn that is still open.
 */
export async function awaitTurnCompleted(
  harness: HarnessApp,
  threadID: string,
  timeoutMs = 120_000,
): Promise<void> {
  await harness.waitForEvent<{ threadId?: string }>(
    'provider:turn_completed',
    (data) => data.threadId === threadID,
    timeoutMs,
  );
}

/**
 * The cursor out of a paged answer.
 *
 * The alternation binds an empty ${CURSOR} on the final page, which
 * carries `done` and no cursor: a capture that matched nothing is a
 * fixture error, and the last call of a paging loop is not one.
 */
export const SHOW_CURSOR_PATTERN = '"cursor":"([^"]+)"|"done":true';

/** One provider's row of a thread_options answer, as far as efforts go. */
export interface ProviderOptionRow {
  provider: string;
  models: Array<{ model: string; efforts?: string[]; default_effort?: string }>;
}

/**
 * Per-model efforts, the half of a catalog a spawn's `effort` comes from.
 * Shared by the single-computer and the paired spec: the row a paired
 * computer answers with has the same shape as this computer's own.
 *
 * Efforts are the catalog's, not a list of the tool's own: a model that
 * offers no reasoning tiers (Claude Haiku 4.5) states none and marks no
 * default, and one that offers tiers marks its default among them. Both
 * providers offer tiers on at least one model, so an empty catalog cannot
 * pass this as "no model has efforts".
 */
export function expectEffortsPerModel(providers: ProviderOptionRow[]): void {
  for (const name of ['claude', 'codex']) {
    const entry = providers.find((row) => row.provider === name);
    expect(entry, `provider ${name}`).toBeDefined();
    expect(entry!.models.length).toBeGreaterThan(0);
    for (const model of entry!.models) {
      const efforts = model.efforts ?? [];
      const label = `${name}/${model.model} efforts`;
      if (efforts.length === 0) {
        expect(model.default_effort ?? '', label).toBe('');
        continue;
      }
      expect(efforts, label).toContain(model.default_effort);
    }
    const offered = entry!.models.filter((model) => (model.efforts ?? []).length > 0);
    expect(offered.length, `${name} models offering efforts`).toBeGreaterThan(0);
  }
}
