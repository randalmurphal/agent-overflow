// Gated mock scenarios that park a turn mid-stream until the spec advances
// it, and the thread starter that hands back the mock's driver. A turn
// parked this way is a stable state to act on instead of a frame to race:
// fork-midturn forks from it, session-replace-midturn replaces its session.
import type { HarnessMockEvent, SeedResult } from './fixtures.js';

// Per-turn text so a two-turn thread's assertions can name the turn they
// mean; the shipped `step-gated` scenario streams the same words every
// turn, which is ambiguous the moment a fork keeps one turn and drops
// another.
export const GATED_CLAUDE = {
  version: 1,
  name: 'fork-midturn-gated',
  provider: 'claude',
  turns: [
    {
      label: 'gated-frames',
      steps: [
        {
          emit: {
            lines: [
              '{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-${TURN}","role":"assistant"}}}',
              '{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}',
            ],
          },
        },
        { waitSignal: { name: 'half' } },
        {
          emit: {
            lines: [
              '{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Turn ${TURN} first half. "}}}',
            ],
          },
        },
        { waitSignal: { name: 'finish' } },
        {
          emit: {
            lines: [
              '{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Turn ${TURN} second half."}}}',
              '{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}',
              '{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}',
              '{"type":"assistant","message":{"id":"msg-${TURN}","role":"assistant","content":[{"type":"text","text":"Turn ${TURN} first half. Turn ${TURN} second half."}]}}',
              '{"type":"result","subtype":"success","is_error":false}',
            ],
          },
        },
      ],
    },
  ],
  afterTurns: 'repeatLast',
};

// One Codex turn that opens a commandExecution and then parks with it
// still running: the row the fork's settle has to flip.
export const GATED_CODEX = {
  version: 1,
  name: 'fork-midturn-codex',
  provider: 'codex',
  codex: { threadId: 'mock-codex-thread' },
  turns: [
    {
      steps: [
        {
          emit: {
            lines: [
              '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}',
              '{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"type":"commandExecution","id":"cmd-${TURN}","status":"inProgress","command":"sleep 30"}}}',
            ],
          },
        },
        { waitSignal: { name: 'hold' } },
        {
          emit: {
            lines: [
              '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"type":"commandExecution","id":"cmd-${TURN}","status":"completed","command":"sleep 30","aggregatedOutput":"done"}}}',
              '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"type":"agentMessage","id":"msg-${TURN}","status":"completed","text":"Codex finished turn ${TURN}."}}}',
              '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}',
            ],
          },
        },
      ],
    },
  ],
  afterTurns: 'repeatLast',
};

export interface Item {
  kind: string;
  status: string;
  summary: string;
}

/** A live mock, plus the two verbs every case below drives it with. */
export interface Driver {
  mockId: string;
  advance: (name: string) => Promise<unknown>;
  gate: (name: string) => Promise<HarnessMockEvent>;
}

export type Harness = {
  rpc: <T>(method: string, ...args: unknown[]) => Promise<T>;
  waitForEvent: <T>(channel: string, predicate?: (data: T) => boolean) => Promise<T>;
  bootstrap: { homeDir?: string };
  url: string;
};

export async function startThread(
  harness: Harness,
  opts: { project: string; title: string; provider?: string },
): Promise<{ threadId: string; driver: Driver }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: opts.project,
        repo: {},
        threads: [{ title: opts.title, provider: opts.provider ?? 'claude' }],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  return {
    threadId,
    driver: {
      mockId: registered.mockId,
      advance: (name) =>
        harness.rpc('HarnessMockCommand', registered.mockId, { type: 'advance', name }),
      gate: (name) =>
        harness.waitForEvent<HarnessMockEvent>(
          'harness:mock',
          (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === name,
        ),
    },
  };
}

/** Send and hold at the point where half the assistant text has streamed. */
export async function sendAndHoldMidStream(
  harness: Harness,
  threadId: string,
  driver: Driver,
  text: string,
): Promise<void> {
  await harness.rpc('SendMessage', threadId, text, null);
  await driver.gate('half');
  await driver.advance('half');
  await driver.gate('finish');
}
