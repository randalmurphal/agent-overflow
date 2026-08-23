// Forking a thread WHILE it streams, against the real backend and the
// real SPA. The contract under test (app_thread_fork.go) is that a fork
// is a snapshot "as if interrupted right now": the fork's cloned
// running/streaming rows settle as errored with the " — interrupted"
// suffix and its open turn closes with stop_reason='interrupted', while
// the SOURCE is never touched and keeps streaming to completion.
//
// The `step-gated`-style scenarios below are what make "mid-stream" a
// stable state to fork from instead of a frame to race: the mock parks
// between deltas until the spec advances it, so every fork here is taken
// at a known point with a partial assistant row on both sides.
//
// Which Claude branch this exercises: the LIVE one. The mock persists
// each user echo to `<homeDir>/.claude/projects/mock/<sessionId>.jsonl`
// before writing it to stdout, and the app's leaf tracker ingests that
// same echo off the wire — so a mid-turn tail fork finds a registered
// session whose CanonicalLeafUUID is on disk and PINS the lazy
// --fork-session cut there (pendingForkRef + pendingForkResumeAt; the
// fork's first send passes --resume-session-at). The cold-scan fallback
// and the first-send pin repair are unit-tested
// (app_fork_midturn_test.go); what only this level proves is that the
// live tracker, the transcript the CLI actually wrote, and the settle
// agree with each other.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import { access, rm } from 'node:fs/promises';
import * as path from 'node:path';

// Per-turn text so a two-turn thread's assertions can name the turn they
// mean; the shipped `step-gated` scenario streams the same words every
// turn, which is ambiguous the moment a fork keeps one turn and drops
// another.
const GATED_CLAUDE = {
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
// still running — the row the fork's settle has to flip.
const GATED_CODEX = {
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

// Liveness is read off the ITEM rows, never `Thread.hasIncompleteTurn`:
// that flag is derived against `last_read_at`, so merely opening the
// thread in the UI flips it to false while the turn is still streaming.
// The row status is what the fork's settle actually rewrites, and it is
// the same on both surfaces.
interface Thread {
  id: string;
  title: string;
  sessionRef?: string;
  pendingForkRef?: string;
  pendingForkResumeAt?: string;
}

interface Item {
  kind: string;
  status: string;
  summary: string;
}

/** A live mock, plus the two verbs every case below drives it with. */
interface Driver {
  mockId: string;
  advance: (name: string) => Promise<unknown>;
  gate: (name: string) => Promise<HarnessMockEvent>;
}

type Harness = {
  rpc: <T>(method: string, ...args: unknown[]) => Promise<T>;
  waitForEvent: <T>(channel: string, predicate?: (data: T) => boolean) => Promise<T>;
  bootstrap: { homeDir?: string };
  url: string;
};

async function startThread(
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

// Thread titles appear twice — the sidebar row and the pane header — and
// one title is a prefix of its fork's, so both lookups are exact and
// scoped to the surface they mean.
const exactly = (title: string) => new RegExp(`^${title.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`);

/** Send and hold at the point where half the assistant text has streamed. */
async function sendAndHoldMidStream(
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

test('the per-message fork stays live mid-turn and snapshots without touching the source', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', { scenario: GATED_CLAUDE });
  const { threadId, driver } = await startThread(harness, {
    project: 'fork-midturn',
    title: 'Message fork',
  });

  // Turn 1 runs to completion so the fork has a real prefix to keep.
  await sendAndHoldMidStream(harness, threadId, driver, 'first prompt');
  await driver.advance('finish');
  await harness.waitForEvent('provider:turn_completed');
  // Turn 2 parks halfway through its assistant text.
  await sendAndHoldMidStream(harness, threadId, driver, 'second prompt');

  const sidebarRow = (title: string) =>
    page.getByTestId('thread-row-title').filter({ hasText: exactly(title) });

  await page.goto(harness.url);
  await sidebarRow('Message fork').click();
  await expect(page.getByText('Turn 1 first half. Turn 1 second half.')).toBeVisible();
  await expect(page.getByText('Turn 2 first half.', { exact: true })).toBeVisible();

  // The footer of the message that opened the STREAMING turn. Fork is
  // deliberately not turn-gated (the backend snapshots and leaves the
  // source alone); edit is, because edit-and-resend reverts the source.
  const liveRow = page
    .getByTestId('user-message-bubble')
    .filter({ hasText: 'second prompt' })
    .locator('xpath=..');
  await liveRow.hover();
  await expect(liveRow.getByLabel('Edit message and resend from here')).toBeDisabled();
  const forkButton = liveRow.getByLabel('Fork from this message');
  await expect(forkButton).toBeEnabled();
  await forkButton.click();

  // The fork opens in the pane it was taken from, and nothing failed.
  await expect(page.getByText('Forked from this message into a new thread.')).toBeVisible();
  await expect(page.getByText('Fork failed:')).toHaveCount(0);
  await expect(sidebarRow('Message fork (fork)')).toBeVisible();
  await expect(page.getByTestId('chat-header-title')).toHaveText('Message fork (fork)');

  // A message-keyed fork cuts BEFORE its anchor: turn 1 survives whole,
  // the anchor prompt is not copied (it is restored into the fork's
  // composer instead), and the turn it opened is nowhere in the fork.
  await expect(page.getByText('Turn 1 first half. Turn 1 second half.')).toBeVisible();
  await expect(
    page.getByTestId('user-message-bubble').filter({ hasText: 'second prompt' }),
  ).toHaveCount(0);
  await expect(page.getByText('Turn 2 first half.', { exact: true })).toHaveCount(0);

  // The source never stopped: its row is still STREAMING (not settled
  // as interrupted the way the fork's copy would be), and completing the
  // very same turn yields the whole reply rather than the half the fork
  // was taken at — which no interrupted session could produce.
  const midSource = await harness.rpc<Item[]>('ListItems', threadId);
  expect(midSource.map((i) => [i.kind, i.status, i.summary])).toEqual([
    ['user_text', 'completed', 'first prompt'],
    ['assistant_text', 'completed', 'Turn 1 first half. Turn 1 second half.'],
    ['user_text', 'completed', 'second prompt'],
    ['assistant_text', 'streaming', 'Turn 2 first half. '],
  ]);
  await driver.advance('finish');
  await harness.waitForEvent('provider:turn_completed');
  await sidebarRow('Message fork').click();
  await expect(page.getByText('Turn 2 first half. Turn 2 second half.')).toBeVisible();
  await expect(page.getByText('Turn 2 first half.', { exact: true })).toHaveCount(0);
});

test('a tail fork taken mid-stream renders the interrupted snapshot beside a still-running source', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', { scenario: GATED_CLAUDE });
  const { threadId, driver } = await startThread(harness, {
    project: 'fork-midturn-tail',
    title: 'Tail fork',
  });
  await sendAndHoldMidStream(harness, threadId, driver, 'run the long one');

  const sidebarRow = (title: string) =>
    page.getByTestId('thread-row-title').filter({ hasText: exactly(title) });

  await page.goto(harness.url);
  await sidebarRow('Tail fork').click();
  await expect(page.getByText('Turn 1 first half.', { exact: true })).toBeVisible();

  // "Fork Thread" from the sidebar row is the tail fork — the whole
  // timeline, including the row that is streaming right now.
  await sidebarRow('Tail fork').click({ button: 'right' });
  await page.getByText('Fork Thread', { exact: true }).click();
  await expect(page.getByText('Forked "Tail fork" into a new thread.')).toBeVisible();
  await expect(page.getByText('Failed to fork')).toHaveCount(0);

  // The cloned partial carries the interrupted treatment — the same row
  // shape a real interrupt or the boot crash sweep writes.
  await expect(page.getByText('Turn 1 first half. — interrupted')).toBeVisible();

  const [source, fork] = await Promise.all([
    harness.rpc<Thread>('GetThread', threadId),
    harness
      .rpc<Thread[]>('ListThreads')
      .then((threads) => threads.find((t) => t.title === 'Tail fork (fork)')!),
  ]);
  // The cut is PINNED, never sliced: the fork holds no session of its
  // own yet — its first send passes `--resume-session-at <pin>
  // --fork-session` against the SOURCE session, so the CLI's own fork
  // cuts at the leaf the live tracker had settled when Fork was
  // clicked, not wherever the source has grown to by then.
  expect(fork.sessionRef ?? '').toBe('');
  expect(fork.pendingForkRef).toBe(source.sessionRef);
  expect(fork.pendingForkResumeAt).toBeTruthy();

  // Only the fork's copy settled; the source's row is still streaming.
  expect(
    (await harness.rpc<Item[]>('ListItems', fork.id)).map((i) => [i.status, i.summary]),
  ).toEqual([
    ['completed', 'run the long one'],
    ['errored', 'Turn 1 first half. — interrupted'],
  ]);
  expect(
    (await harness.rpc<Item[]>('ListItems', threadId)).map((i) => [i.status, i.summary]),
  ).toEqual([
    ['completed', 'run the long one'],
    ['streaming', 'Turn 1 first half. '],
  ]);

  // And the source, untouched, finishes its turn normally.
  await driver.advance('finish');
  await harness.waitForEvent('provider:turn_completed');
  await sidebarRow('Tail fork').click();
  await expect(page.getByText('Turn 1 first half. Turn 1 second half.')).toBeVisible();
  await expect(page.getByText('Turn 1 first half. — interrupted')).toHaveCount(0);
});

test('a Claude fork with no transcript on disk yet starts a fresh provider thread', async ({
  harness,
}) => {
  await harness.rpc('HarnessSetScenario', { scenario: GATED_CLAUDE });
  const { threadId, driver } = await startThread(harness, {
    project: 'fork-midturn-degenerate',
    title: 'No transcript',
  });
  await sendAndHoldMidStream(harness, threadId, driver, 'answer before the file lands');

  // The sanctioned degenerate case is "the CLI has not written the
  // transcript yet", which the code keys on ErrSessionFileNotFound.
  // Removing the file the mock already wrote reproduces exactly that
  // state deterministically; the assertion that it EXISTED first is what
  // keeps this from passing vacuously if the mock stops persisting.
  // `projects/mock` is the mock's fixed project dir (ao-mockprovider's
  // claude.go), under the harness-pinned $HOME.
  const source = await harness.rpc<Thread>('GetThread', threadId);
  const transcript = path.join(
    harness.bootstrap.homeDir!,
    '.claude',
    'projects',
    'mock',
    `${source.sessionRef}.jsonl`,
  );
  await access(transcript);
  await rm(transcript);

  const fork = await harness.rpc<Thread>('ForkThread', threadId, null);
  expect(fork.sessionRef ?? '').toBe('');
  expect(fork.pendingForkRef ?? '').toBe('');
  expect(fork.pendingForkResumeAt ?? '').toBe('');
  // The clone still settled: the fork holds the prompt plus the partial
  // reply under the interrupted treatment, and starts a fresh provider
  // thread on its first send.
  const forkItems = await harness.rpc<Item[]>('ListItems', fork.id);
  expect(forkItems.map((i) => [i.kind, i.status, i.summary])).toEqual([
    ['user_text', 'completed', 'answer before the file lands'],
    ['assistant_text', 'errored', 'Turn 1 first half. — interrupted'],
  ]);
  // The source's own row is untouched — still streaming, not settled.
  expect(
    (await harness.rpc<Item[]>('ListItems', threadId)).map((i) => [i.status, i.summary]),
  ).toEqual([
    ['completed', 'answer before the file lands'],
    ['streaming', 'Turn 1 first half. '],
  ]);

  await driver.advance('finish');
  await harness.waitForEvent('provider:turn_completed');
  const sourceItems = await harness.rpc<Item[]>('ListItems', threadId);
  expect(sourceItems.map((i) => [i.kind, i.status, i.summary])).toEqual([
    ['user_text', 'completed', 'answer before the file lands'],
    ['assistant_text', 'completed', 'Turn 1 first half. Turn 1 second half.'],
  ]);
});

test('a Codex fork mid-turn forks the live thread with no boundary', async ({ harness }) => {
  await harness.rpc('HarnessSetScenario', { scenario: GATED_CODEX });
  const { threadId, driver } = await startThread(harness, {
    project: 'fork-midturn-codex',
    title: 'Codex fork',
    provider: 'codex',
  });
  await harness.rpc('SendMessage', threadId, 'run something slow', null);
  await driver.gate('hold');

  const source = await harness.rpc<Thread>('GetThread', threadId);
  const fork = await harness.rpc<Thread>('ForkThread', threadId, null);

  // The mock REFUSES a `lastTurnId` naming the in-progress turn, exactly
  // as codex 0.147.0 does (cmd/ao-mockprovider TestCodexThreadForkCutsAtTheAnchor
  // pins that refusal directly). A fork that succeeded here therefore
  // proves AO sent `thread/fork` with no boundary at all.
  expect(fork.sessionRef).toBeTruthy();
  expect(fork.sessionRef).not.toBe(source.sessionRef);

  const forkItems = await harness.rpc<Item[]>('ListItems', fork.id);
  expect(forkItems.map((i) => [i.kind, i.status, i.summary])).toEqual([
    ['user_text', 'completed', 'run something slow'],
    ['tool_call', 'errored', 'Bash: sleep 30 — interrupted'],
  ]);
  // The source's command row is still running under its own thread.
  expect(
    (await harness.rpc<Item[]>('ListItems', threadId)).map((i) => [i.status, i.summary]),
  ).toEqual([
    ['completed', 'run something slow'],
    ['running', 'Bash: sleep 30'],
  ]);

  await driver.advance('hold');
  await harness.waitForEvent('provider:turn_completed');
  const sourceItems = await harness.rpc<Item[]>('ListItems', threadId);
  expect(sourceItems.map((i) => [i.kind, i.status, i.summary])).toEqual([
    ['user_text', 'completed', 'run something slow'],
    ['tool_call', 'completed', 'Bash: sleep 30'],
    ['assistant_text', 'completed', 'Codex finished turn 1.'],
  ]);
});
