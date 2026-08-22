// Shared fixtures for the agent-visibility specs
// (docs/specs/agent-visibility.md § "Success criteria").
//
// Every Claude wire shape below is modelled on a checked-in 2026-08-22
// capture — `docs/references/fixtures/claude/{task_progress,
// can_use_tool_agent_id,forked_skill,background_tasks_control}_20260822
// .ndjson` — and the same shapes the parser's replay tests pin
// (`internal/provider/claude/subagent_visibility_replay_test.go`). The
// scenarios are written inline rather than added to the embedded library
// because they exist to drive ONE spec each; the library is for shapes
// more than one consumer replays.
//
// Two deliberate deviations from the captures:
//
//   - `session_id` is omitted everywhere. The mock owns the session id and
//     a hard-coded one from a capture would make the app's own session
//     bookkeeping disagree with the process it is talking to.
//   - `canUseToolLine` builds the control_request by hand for a scenario
//     that wants to choose the ordering itself. The scenario `approval`
//     step is the normal route and now carries `toolUseId`/`agentId` too
//     (added with these specs, since a request without them is the
//     main-agent shape and cannot scope to a card); prefer it, because the
//     mock then waits for the real decision and branches on it.

import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';

// ---------------------------------------------------------------------
// Wire-line builders
// ---------------------------------------------------------------------

type Json = Record<string, unknown>;

function withParent(envelope: Json, parentToolUseId?: string): Json {
  return parentToolUseId ? { ...envelope, parent_tool_use_id: parentToolUseId } : envelope;
}

function j(envelope: Json): string {
  return JSON.stringify(envelope);
}

/** An assistant message whose single content block is a tool_use. */
export function toolUseLine(
  messageId: string,
  toolUseId: string,
  name: string,
  input: Json,
  parentToolUseId?: string,
): string {
  return j(
    withParent(
      {
        type: 'assistant',
        message: {
          id: messageId,
          role: 'assistant',
          model: 'claude-mock-1',
          content: [{ type: 'tool_use', id: toolUseId, name, input }],
        },
      },
      parentToolUseId,
    ),
  );
}

/** A `user` envelope carrying one tool_result block. */
export function toolResultLine(
  toolUseId: string,
  content: unknown,
  opts: {
    parentToolUseId?: string;
    isError?: boolean;
    toolUseResult?: unknown;
  } = {},
): string {
  const block: Json = { type: 'tool_result', tool_use_id: toolUseId, content };
  if (opts.isError) block.is_error = true;
  const envelope: Json = {
    type: 'user',
    message: { role: 'user', content: [block] },
  };
  if (opts.toolUseResult !== undefined) envelope.tool_use_result = opts.toolUseResult;
  return j(withParent(envelope, opts.parentToolUseId));
}

/**
 * The §E5 async-agent launch ack. The literal opening sentence and the
 * `agentId:` line are the parser's text fallback; the structured
 * `tool_use_result` is the primary signal, and `isAsync` is what marks
 * the launch row background.
 */
export function asyncAgentAckLine(
  toolUseId: string,
  agentId: string,
  description: string,
  parentToolUseId?: string,
): string {
  return toolResultLine(
    toolUseId,
    [
      {
        type: 'text',
        text:
          `Async agent launched successfully.\nagentId: ${agentId} (internal ID - do not mention to user.` +
          ` Use SendMessage with to: '${agentId}', summary: '<5-10 word recap>' to continue this agent.)`,
      },
    ],
    {
      parentToolUseId,
      toolUseResult: {
        isAsync: true,
        status: 'async_launched',
        agentId,
        description,
      },
    },
  );
}

/** Streamed assistant text: message_start … message_stop plus the
 * coalesced envelope the app dedupes against the registered message id. */
export function textLines(
  messageId: string,
  text: string,
  parentToolUseId?: string,
): string[] {
  const p = parentToolUseId ? { parent_tool_use_id: parentToolUseId } : {};
  return [
    j({ type: 'stream_event', event: 'message_start', ...p, data: { type: 'message_start', message: { id: messageId, role: 'assistant' } } }),
    j({ type: 'stream_event', event: 'content_block_start', ...p, data: { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } } }),
    j({ type: 'stream_event', event: 'content_block_delta', ...p, data: { type: 'content_block_delta', delta: { type: 'text_delta', text } } }),
    j({ type: 'stream_event', event: 'content_block_stop', ...p, data: { type: 'content_block_stop', index: 0 } }),
    j({ type: 'stream_event', event: 'message_stop', ...p, data: { type: 'message_stop' } }),
    j(withParent({ type: 'assistant', message: { id: messageId, role: 'assistant', model: 'claude-mock-1', content: [{ type: 'text', text }] } }, parentToolUseId)),
  ];
}

/** Streamed thinking block, same framing contract as textLines. */
export function thinkingLines(
  messageId: string,
  thinking: string,
  parentToolUseId?: string,
): string[] {
  const p = parentToolUseId ? { parent_tool_use_id: parentToolUseId } : {};
  return [
    j({ type: 'stream_event', event: 'message_start', ...p, data: { type: 'message_start', message: { id: messageId, role: 'assistant' } } }),
    j({ type: 'stream_event', event: 'content_block_start', ...p, data: { type: 'content_block_start', index: 0, content_block: { type: 'thinking', thinking: '' } } }),
    j({ type: 'stream_event', event: 'content_block_delta', ...p, data: { type: 'content_block_delta', delta: { type: 'thinking_delta', thinking } } }),
    j({ type: 'stream_event', event: 'content_block_stop', ...p, data: { type: 'content_block_stop', index: 0 } }),
    j({ type: 'stream_event', event: 'message_stop', ...p, data: { type: 'message_stop' } }),
    j(withParent({ type: 'assistant', message: { id: messageId, role: 'assistant', model: 'claude-mock-1', content: [{ type: 'thinking', thinking }] } }, parentToolUseId)),
  ];
}

export function taskStartedLine(
  taskId: string,
  toolUseId: string,
  description: string,
  opts: { taskType?: string; ownedBySubagent?: boolean } = {},
): string {
  const envelope: Json = {
    type: 'system',
    subtype: 'task_started',
    task_id: taskId,
    tool_use_id: toolUseId,
    description,
    subagent_type: 'general-purpose',
    task_type: opts.taskType ?? 'local_agent',
  };
  if (opts.ownedBySubagent) envelope.owned_by_subagent = true;
  return j(envelope);
}

export function taskProgressLine(
  taskId: string,
  toolUseId: string,
  activity: string,
  usage: { total_tokens?: number; tool_uses?: number; duration_ms?: number },
  lastToolName: string,
): string {
  return j({
    type: 'system',
    subtype: 'task_progress',
    task_id: taskId,
    tool_use_id: toolUseId,
    description: activity,
    subagent_type: 'general-purpose',
    usage,
    last_tool_name: lastToolName,
  });
}

export function taskUpdatedLine(taskId: string, patch: Json): string {
  return j({ type: 'system', subtype: 'task_updated', task_id: taskId, patch });
}

export function backgroundTasksChangedLine(
  tasks: Array<{ task_id: string; task_type: string; description: string }>,
): string {
  return j({ type: 'system', subtype: 'background_tasks_changed', tasks });
}

export function taskNotificationLine(
  taskId: string,
  toolUseId: string,
  summary: string,
  opts: {
    outputFile?: string;
    usage?: { total_tokens?: number; tool_uses?: number; duration_ms?: number };
    uuid?: string;
  } = {},
): string {
  const envelope: Json = {
    type: 'system',
    subtype: 'task_notification',
    task_id: taskId,
    tool_use_id: toolUseId,
    status: 'completed',
    summary,
    uuid: opts.uuid ?? `notify-${taskId}`,
  };
  if (opts.outputFile) envelope.output_file = opts.outputFile;
  if (opts.usage) envelope.usage = opts.usage;
  return j(envelope);
}

/** A subagent's `can_use_tool`, scoped by `agent_id` and nothing else. */
export function canUseToolLine(
  requestId: string,
  toolName: string,
  input: Json,
  toolUseId: string,
  agentId: string,
): string {
  return j({
    type: 'control_request',
    request_id: requestId,
    request: {
      subtype: 'can_use_tool',
      tool_name: toolName,
      display_name: toolName,
      input,
      tool_use_id: toolUseId,
      agent_id: agentId,
    },
  });
}

/** `system/permission_denied` — a pre-ask refusal, scoped by agent_id. */
export function permissionDeniedLine(
  toolName: string,
  toolUseId: string,
  agentId: string,
  reason: string,
): string {
  return j({
    type: 'system',
    subtype: 'permission_denied',
    tool_name: toolName,
    tool_use_id: toolUseId,
    agent_id: agentId,
    decision_reason_type: 'rule',
    decision_reason: reason,
    message: `Permission to use ${toolName} has been denied.`,
    uuid: `pd-${toolUseId}`,
  });
}

export const RESULT_LINE = j({ type: 'result', subtype: 'success', is_error: false });

// ---------------------------------------------------------------------
// Scenario / thread setup
// ---------------------------------------------------------------------

export interface ScenarioStep {
  emit?: { lines: string[]; delayBetweenMs?: number };
  waitSignal?: { name: string };
  writeFile?: { path: string; content: string; append?: boolean };
  /**
   * `control_request/can_use_tool`, answered by the app. `toolUseId` and
   * `agentId` are the correlation fields the real CLI carries; `agentId`
   * (the subagent's task id) is what scopes the prompt to a card.
   */
  approval?: {
    toolName: string;
    input?: Json;
    toolUseId?: string;
    agentId?: string;
    onAllow?: ScenarioStep[];
    onDeny?: ScenarioStep[];
    timeoutMs?: number;
  };
  delayMs?: number;
}

export function claudeScenario(name: string, steps: ScenarioStep[]): unknown {
  return {
    version: 1,
    name,
    provider: 'claude',
    turns: [{ label: name, steps }],
    // These scenarios script exactly one turn; a stray second send must
    // not replay the whole agent fan-out on top of the first.
    afterTurns: 'silent',
  };
}

export function emit(lines: string[]): ScenarioStep {
  return { emit: { lines, delayBetweenMs: 5 } };
}

/**
 * A project + a thread with one turn of seeded history, so the thread is
 * sidebar-visible (drafts are hidden) before any live turn runs. Live
 * progress ticks are in-memory frontend state, so these specs open the UI
 * FIRST and only then start the session — a tick emitted before the page
 * subscribed is simply gone.
 */
export async function seedAgentThread(
  harness: HarnessApp,
  projectName: string,
  title: string,
  provider: 'claude' | 'codex' = 'claude',
): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: projectName,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
        threads: [
          {
            title,
            provider,
            turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
        ],
      },
    ],
  });
  return seed.projects[0].threadIds[0];
}

/** Start the provider session and wait for the mock to register. */
export async function startMock(harness: HarnessApp, threadId: string): Promise<string> {
  await harness.rpc('StartSession', threadId);
  const registered = await harness.waitForEvent<{ mockId: string }>(
    'harness:mock',
    (ev: any) => ev.report.kind === 'registered',
  );
  return registered.mockId;
}

export function advance(harness: HarnessApp, mockId: string, name: string): Promise<unknown> {
  return harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name });
}

export function waitForGate(harness: HarnessApp, name: string): Promise<unknown> {
  return harness.waitForEvent(
    'harness:mock',
    (ev: any) => ev.report.kind === 'waiting_signal' && ev.report.detail === name,
  );
}

// ---------------------------------------------------------------------
// Item reads
// ---------------------------------------------------------------------

export interface Item {
  id: string;
  threadId: string;
  kind: string;
  status: string;
  summary: string;
  toolName?: string;
  parentId?: string;
  isBackground?: boolean;
  completionOf?: string;
  meta?: string;
  payloadMeta?: string;
}

export function listItems(harness: HarnessApp, threadId: string): Promise<Item[]> {
  return harness.rpc<Item[]>('ListItems', threadId);
}

export function itemMeta(item: Item | undefined): Record<string, unknown> {
  if (!item?.meta) return {};
  try {
    return JSON.parse(item.meta) as Record<string, unknown>;
  } catch {
    return {};
  }
}

/**
 * A subagent's sidechain transcript, in the JSONL shape
 * `task_notification.output_file` names and
 * `claudeimport.ConvertSubagentTranscript` reads. Rows are `isSidechain`
 * and chained by `parentUuid`, exactly like `~/.claude/projects/**.jsonl`.
 */
export function sidechainTranscript(rows: Array<{ text?: string; tool?: { id: string; name: string; result: string } }>): string {
  const out: string[] = [];
  let prev = 's0';
  let seconds = 0;
  const stamp = () => `2026-08-22T00:00:${String(++seconds).padStart(2, '0')}.000Z`;
  out.push(
    JSON.stringify({
      type: 'user',
      uuid: 's0',
      parentUuid: null,
      isSidechain: true,
      timestamp: stamp(),
      message: { role: 'user', content: 'the task prompt' },
    }),
  );
  let n = 0;
  for (const row of rows) {
    n += 1;
    if (row.text !== undefined) {
      const uuid = `s-text-${n}`;
      out.push(
        JSON.stringify({
          type: 'assistant',
          uuid,
          parentUuid: prev,
          isSidechain: true,
          timestamp: stamp(),
          message: {
            role: 'assistant',
            id: `msg-backfill-${n}`,
            model: 'claude-mock-1',
            content: [{ type: 'text', text: row.text }],
          },
        }),
      );
      prev = uuid;
    }
    if (row.tool) {
      const useUuid = `s-tool-${n}`;
      out.push(
        JSON.stringify({
          type: 'assistant',
          uuid: useUuid,
          parentUuid: prev,
          isSidechain: true,
          timestamp: stamp(),
          message: {
            role: 'assistant',
            id: `msg-backfill-tool-${n}`,
            model: 'claude-mock-1',
            content: [{ type: 'tool_use', id: row.tool.id, name: row.tool.name, input: { file_path: 'README.md' } }],
          },
        }),
      );
      const resUuid = `s-tool-res-${n}`;
      out.push(
        JSON.stringify({
          type: 'user',
          uuid: resUuid,
          parentUuid: useUuid,
          isSidechain: true,
          timestamp: stamp(),
          message: {
            role: 'user',
            content: [{ type: 'tool_result', tool_use_id: row.tool.id, content: row.tool.result }],
          },
        }),
      );
      prev = resUuid;
    }
  }
  return out.join('\n') + '\n';
}
