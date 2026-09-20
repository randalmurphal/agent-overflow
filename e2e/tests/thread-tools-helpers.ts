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
