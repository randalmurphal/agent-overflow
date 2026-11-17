// Wakes whose session is slow to start or will not start
// (docs/specs/standing-agents.md, "Wake supervisor"), end to end on both
// mock providers.
//
// Coverage: a wake and an outside send accepted while the Codex session they
// need is held at its start, the send returning at once with its id, and
// both arriving after; a wake whose session will not start (Codex's
// handshake refused, Claude's workspace gone) leaving one error row naming
// the cause and a notification that the next start withdraws; a later fire
// retrying a start that failed; a start-failure notification raised before a
// restart withdrawn by the first start after it; archiving a thread whose
// start failed withdrawing its held wake and send, and returning a finished
// remote job's held completion to its watch for delivery after the
// unarchive; a Stop on a held remote completion's incoming row
// dismissing it for good; and a watch whose script keeps exiting nonzero
// notifying once until a fixed script withdraws it (compact:
// compact-standing-agent-broken-watch.spec.ts).
import { randomUUID } from 'node:crypto';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

import { test, expect, type SeedResult } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { pairBuildBox } from './build-box-helpers.js';
import { agentsBody, openAgents } from './agents-tray-helpers.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import {
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import {
  run,
  runHere,
  SAY_SCRIPT,
  sayControls,
  startBreaker,
  dbWake,
  wakeInput,
  countInputs,
  threadSend,
  wakeBubble,
  completionRows,
  shoot,
  dbWakes,
  brokenWatchNotifiesUntilFixed,
  type NotificationSend,
} from './standing-agent-wake-helpers.js';

// A wake and an outside send are accepted once they are queued, not once the
// session they need is up. Codex holds this start at its handshake for longer
// than the ten seconds a delivery once waited for the thread lock; both still
// arrive, and the send returns at once with its id. Claude's start does not
// wait on the CLI, so the mock cannot make one slow.
test('codex: a wake and an outside send are accepted while the session they need is slow to start', async ({ page }) => {
  test.setTimeout(150_000);
  const provider = 'codex';
  const HOLD_MS = 20_000;
  const harness = await launchHarness();
  try {
    const dataDir = harness.bootstrap.dataDir;
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `wakes-slow-start-${provider}`, repo: {}, threads: [{ title: 'Slow starter', provider }] }],
    });
    const thread = seed.projects[0].threadIds[0];
    const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', thread);
    await writeFile(
      join(home, 'once.sh'),
      `[ -f "$AO_AGENT_HOME/once.done" ] && exec sleep 3600
while [ ! -f "$AO_AGENT_HOME/once.release" ]; do sleep 0.1; done
: > "$AO_AGENT_HOME/once.done"
echo "the report while the start was held"
`,
    );
    await setScenario(
      harness,
      home,
      threadToolsScenario({
        name: `wakes-slow-start-${provider}`,
        provider,
        echoUserMessage: true,
        turns: [
          {
            steps: [{ call: { tool: 'thread_watch', args: { label: 'once', command: run('once.sh'), every_seconds: 1 } } }],
            text: 'Watching once.',
          },
        ],
        afterTurns: 'silent',
      }),
    );
    await harness.rpc('StartSession', thread);
    await harness.rpc('SendMessage', thread, 'report once', null);
    const ack = await awaitToolAnswer(harness, { tool: 'thread_watch', cwd: home, timeoutMs: 60_000 });
    expect(ack.isError, ack.text).toBe(false);
    await awaitTurnCompleted(harness, thread);
    await harness.rpc('StopSession', thread);

    // The next start is held at the handshake. The send may join the
    // wake's turn or take one of its own, so every reply reads the same.
    await setScenario(harness, home, {
      ...threadToolsScenario({
        name: `wakes-slow-start-held-${provider}`,
        provider,
        echoUserMessage: true,
        turns: [{ text: 'Noted.' }, { text: 'Noted.' }, { text: 'Noted.' }],
        afterTurns: 'silent',
      }),
      startupDelayMs: HOLD_MS,
    });
    const queued = () => {
      const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
      try {
        return (
          db.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ? ORDER BY enqueued_at, rowid`).all(thread) as {
            send_id: string;
          }[]
        ).map((row) => row.send_id);
      } finally {
        db.close();
      }
    };
    const released = Date.now();
    await writeFile(join(home, 'once.release'), '');
    // The wake is recorded delivered when it is queued, while its start is
    // still held.
    await expect.poll(() => dbWake(dataDir, thread, 'once')?.last_outcome, { timeout: 15_000 }).toBe('delivered');

    const sending = Date.now();
    const sent = await threadSend(dataDir, [thread, 'deploy 10 is done']);
    const sendMs = Date.now() - sending;
    expect(sent.stderr).toBe('');
    expect(sent.code).toBe(0);
    const sendID = /^Queued for thread \S+ as (agent-send:\S+)\.\n$/.exec(sent.stdout)?.[1];
    expect(sendID, sent.stdout).toBeTruthy();
    expect(sendMs).toBeLessThan(8_000);
    // Both are queued and neither has reached the provider: the start is
    // still held.
    const rows = queued();
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatch(/^agent-wake:/);
    expect(rows[1]).toBe(sendID);
    expect(countInputs(harness, home, 'Watch "once" reported.')).toBe(0);
    expect(countInputs(harness, home, 'Message sent from outside Agent Overflow')).toBe(0);

    const wake = await wakeInput(harness, home, 'Watch "once" reported.', 90_000);
    expect(wake.report.input).toContain('"the report while the start was held"');
    const outside = await wakeInput(harness, home, 'Message sent from outside Agent Overflow', 90_000);
    expect(outside.report.input).toContain('"deploy 10 is done"');
    expect(Date.now() - released).toBeGreaterThanOrEqual(HOLD_MS);
    await expect.poll(queued, { timeout: 30_000 }).toEqual([]);
    expect(dbWake(dataDir, thread, 'once')!.last_outcome).toBe('delivered');

    await harness.open(page);
    const report = await wakeBubble(page, 'Slow starter', 'Watch · once', 'the report while the start was held');
    const send = await wakeBubble(page, 'Slow starter', 'Sent by a script', 'deploy 10 is done');
    await shoot(page, `${provider}-slow-start-wake-and-send.png`, report, send);
  } finally {
    await harness.close();
  }
});

// A wake whose session will not start leaves an error row nobody is looking
// at, so it also notifies, and the thread's next start takes the
// notification back. Codex's mock refuses the handshake. Claude's start ends
// at the spawn and does not wait on the CLI, so the mock cannot fail it; its
// start fails here because its workspace, the assistant's home, is gone,
// which the row names and nothing else in the start reports. The watch runs
// outside the home, so it still reports.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a wake whose session will not start notifies until the thread starts`, async ({ page }) => {
    test.setTimeout(150_000);
    const harness = await launchHarness();
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `wakes-start-fails-${provider}`, repo: {}, threads: [{ title: 'Broken starter', provider }] }],
      });
      const { path: project, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', thread);
      await writeFile(
        join(project, 'once.sh'),
        `[ -f once.done ] && exec sleep 3600
while [ ! -f once.release ]; do sleep 0.1; done
: > once.done
echo "the report nobody could read"
`,
      );
      await setScenario(
        harness,
        home,
        threadToolsScenario({
          name: `wakes-start-fails-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    tool: 'thread_watch',
                    args: { label: 'once', command: runHere('once.sh'), every_seconds: 1, cwd: project },
                  },
                },
              ],
              text: 'Watching once.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StartSession', thread);
      await harness.rpc('SendMessage', thread, 'report once', null);
      const ack = await awaitToolAnswer(harness, { tool: 'thread_watch', cwd: home, timeoutMs: 60_000 });
      expect(ack.isError, ack.text).toBe(false);
      await awaitTurnCompleted(harness, thread);
      await harness.rpc('StopSession', thread);

      const b = startBreaker(harness, provider, home);
      breaker = b;
      await b.brk();
      const startFailure = (retract: boolean) => (data: NotificationSend) =>
        data.id === `session-start:${thread}` && Boolean(data.retract) === retract;
      const raised = harness.waitForEvent<NotificationSend>('notification:send', startFailure(false), 60_000);
      await writeFile(join(project, 'once.release'), '');
      const failure = await raised;
      expect(failure).toMatchObject({
        kind: 'error',
        title: 'Broken starter',
        body: 'The agent could not start. Open the thread to see why.',
        target: { kind: 'thread', threadId: thread },
      });
      const rowPrefix = 'The watch "once" woke this thread and its message is queued, but the agent could not start: ';
      const errorRows = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db.prepare(`SELECT summary FROM items WHERE thread_id = ? AND kind = 'error'`).all(thread) as {
              summary: string;
            }[]
          ).map((row) => row.summary);
        } finally {
          db.close();
        }
      };
      await expect.poll(() => errorRows().some((row) => row.startsWith(rowPrefix)), { timeout: 15_000 }).toBe(true);
      // The start's one row, naming what failed.
      expect(errorRows()).toEqual([
        rowPrefix +
          (provider === 'codex'
            ? 'start session: codex: initialize handshake failed: codex: initialize: mock: the app-server refused to start (code -32603)'
            : `start session: workspace ${home} no longer exists`),
      ]);
      const queued = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ?`).all(thread) as { send_id: string }[]
          ).map((row) => row.send_id);
        } finally {
          db.close();
        }
      };
      expect(queued()).toHaveLength(1);
      expect(queued()[0]).toMatch(/^agent-wake:/);
      expect(countInputs(harness, home, 'Watch "once" reported.')).toBe(0);
      expect(harness.countEvents<NotificationSend>('notification:send', startFailure(false))).toBe(1);

      // Mended, the person's start takes the notification back and the
      // queued wake arrives.
      await b.mend();
      await setScenario(
        harness,
        home,
        threadToolsScenario({
          name: `wakes-start-mended-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [{ text: 'Noted.' }],
          afterTurns: 'silent',
        }),
      );
      const withdrawn = harness.waitForEvent<NotificationSend>('notification:send', startFailure(true), 60_000);
      await harness.rpc('StartSession', thread);
      await withdrawn;
      const wake = await wakeInput(harness, home, 'Watch "once" reported.');
      expect(wake.report.input).toContain('"the report nobody could read"');
      await awaitTurnCompleted(harness, thread);
      expect(harness.countEvents<NotificationSend>('notification:send', startFailure(false))).toBe(1);
      expect(harness.countEvents<NotificationSend>('notification:send', startFailure(true))).toBe(1);

      await harness.open(page);
      const report = await wakeBubble(page, 'Broken starter', 'Watch · once', 'the report nobody could read');
      const row = page.getByText(rowPrefix);
      await shoot(page, `${provider}-start-failure-notifies.png`, row, report);
    } finally {
      await breaker?.cleanup();
      await harness.close();
    }
  });
}

// A watch's own failure is the person's to hear about too: its script
// keeps exiting nonzero, which wakes nothing.
test('a watch whose script keeps failing notifies once until it is fixed', async ({ page }) => {
  test.setTimeout(150_000);
  await brokenWatchNotifiesUntilFixed(page, 'desktop');
});

// A failed start does not stall the agent. Each later run of the watch
// tries the start again, and the one whose start works delivers the held
// wake and its own, with nobody starting the thread. The notification is
// raised once per run of failures and withdrawn by the start that ends it;
// the second run here is ended by a run with nothing to say. A held run
// (SAY_SCRIPT) makes the mend land between two runs.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a later fire retries a start that failed`, async ({ page }) => {
    test.setTimeout(180_000);
    const harness = await launchHarness();
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `wakes-start-retry-${provider}`, repo: {}, threads: [{ title: 'Retrying starter', provider }] }],
      });
      const { path: project, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', thread);
      await writeFile(join(project, 'say.sh'), SAY_SCRIPT);
      const { say, hold, release } = sayControls(project);
      const b = startBreaker(harness, provider, home);
      breaker = b;
      await setScenario(
        harness,
        home,
        threadToolsScenario({
          name: `wakes-start-retry-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    tool: 'thread_watch',
                    args: { label: 'tick', command: runHere('say.sh'), every_seconds: 1, cwd: project },
                  },
                },
              ],
              text: 'Watching tick.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StartSession', thread);
      await harness.rpc('SendMessage', thread, 'watch tick', null);
      const ack = await awaitToolAnswer(harness, { tool: 'thread_watch', cwd: home, timeoutMs: 60_000 });
      expect(ack.isError, ack.text).toBe(false);
      await awaitTurnCompleted(harness, thread);

      const brk = async () => {
        await harness.rpc('StopSession', thread);
        await b.brk();
      };
      const mend = async (name: string) => {
        await b.mend();
        await setScenario(
          harness,
          home,
          threadToolsScenario({ name, provider, echoUserMessage: true, turns: [{ text: 'Noted.' }], afterTurns: 'repeatLast' }),
        );
      };
      const startFailure = (retract: boolean) => (data: NotificationSend) =>
        data.id === `session-start:${thread}` && Boolean(data.retract) === retract;
      const raises = () => harness.countEvents<NotificationSend>('notification:send', startFailure(false));
      const retracts = () => harness.countEvents<NotificationSend>('notification:send', startFailure(true));
      const errorRows = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db.prepare(`SELECT summary FROM items WHERE thread_id = ? AND kind = 'error'`).all(thread) as {
              summary: string;
            }[]
          ).map((row) => row.summary);
        } finally {
          db.close();
        }
      };
      const lastOutput = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db.prepare(`SELECT last_output FROM agent_wakes WHERE thread_id = ? AND label = 'tick'`).get(thread) as {
              last_output: string;
            }
          ).last_output;
        } finally {
          db.close();
        }
      };
      const queuedWakes = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db
              .prepare(`SELECT COUNT(*) AS n FROM flush_queue_items WHERE thread_id = ? AND send_id LIKE 'agent-wake:%'`)
              .get(thread) as { n: number }
          ).n;
        } finally {
          db.close();
        }
      };
      const reported = (text: string) => countInputs(harness, home, JSON.stringify(text));
      // No turn is running and nothing waits to go in. Codex may steer the
      // second wake into the first one's turn, so inputs and turns do not
      // pair up.
      const settled = () =>
        expect
          .poll(
            async () => {
              const live = await harness.rpc<{ activeTurn?: unknown; queueItems: unknown[]; flushedItems: unknown[] }>(
                'GetThreadLiveState',
                thread,
              );
              return !live.activeTurn && live.queueItems.length === 0 && live.flushedItems.length === 0;
            },
            { timeout: 60_000 },
          )
          .toBe(true);

      // The first run of failures: one wake held, one notification, one
      // row, while later runs try the start and skip their own wakes.
      await brk();
      await say('report 1');
      await harness.waitForEvent<NotificationSend>('notification:send', startFailure(false), 60_000);
      // The run that reads report 2 finds report 1 still queued and skips:
      // its output stays on the row, where the quiet runs after it leave it,
      // and the queue still holds the one wake.
      await say('report 2');
      await expect.poll(lastOutput, { timeout: 30_000 }).toContain('"report 2"');
      expect(queuedWakes()).toBe(1);
      // The held wake waits on a start that keeps failing. Nothing is
      // working on it, so the thread reads as idle on every surface.
      const live = await harness.rpc<{ threadId: string; sendsPending?: boolean }[]>('ListThreadLiveActivity');
      expect(live.find((row) => row.threadId === thread)?.sendsPending ?? false).toBe(false);
      await harness.open(page);
      await page.getByTestId('thread-row-title').filter({ hasText: 'Retrying starter' }).first().click();
      await expect(page.getByTestId('chat-header-title').filter({ hasText: 'Retrying starter' })).toBeVisible();
      await expect(
        page.getByTestId('thread-row').filter({ hasText: 'Retrying starter' }).locator('[data-testid="thread-row-status-dot"][aria-label="Working"]'),
      ).toHaveCount(0);
      await expect(page.getByTestId('activity-rail-hairline')).toHaveCount(0);
      await captureAgentMessage(
        page,
        `${provider}-idle-with-a-queued-wake.png`,
        page.getByTestId('thread-row').filter({ hasText: 'Retrying starter' }).first(),
        page.getByLabel('Message Input'),
      );
      expect(errorRows()).toHaveLength(1);
      expect(raises()).toBe(1);
      expect(retracts()).toBe(0);
      expect(reported('report 1')).toBe(0);

      // Mended between two runs: the next run's start works, and the held
      // wake and the run's own both arrive.
      await hold();
      await mend(`wakes-start-retry-mended-${provider}`);
      await say('report 3');
      await release();
      await harness.waitForEvent<NotificationSend>('notification:send', startFailure(true), 60_000);
      await expect.poll(() => reported('report 1') > 0 && reported('report 3') > 0, { timeout: 60_000 }).toBe(true);
      await settled();
      expect(reported('report 2')).toBe(0);
      expect(raises()).toBe(1);
      expect(retracts()).toBe(1);

      // A second run of failures raises the notification again, and a run
      // with nothing to say ends it.
      await brk();
      await say('report 4');
      await harness.waitForEvent<NotificationSend>('notification:send', startFailure(false), 60_000);
      expect(raises()).toBe(2);
      await hold();
      await mend(`wakes-start-retry-quiet-${provider}`);
      await release();
      await harness.waitForEvent<NotificationSend>('notification:send', startFailure(true), 60_000);
      await expect.poll(() => reported('report 4'), { timeout: 60_000 }).toBeGreaterThan(0);
      await settled();
      expect(raises()).toBe(2);
      expect(retracts()).toBe(2);

      await harness.open(page);
      const held = await wakeBubble(page, 'Retrying starter', 'Watch · tick', 'report 1');
      const own = await wakeBubble(page, 'Retrying starter', 'Watch · tick', 'report 3');
      await shoot(page, `${provider}-start-retried-by-a-later-fire.png`, held, own);
    } finally {
      await breaker?.cleanup();
      await harness.close();
    }
  });
}

// The start-failure notification outlives a restart, and so does what
// withdraws it: the first start that succeeds after the restart takes it
// back, once.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a start failure raised before a restart is withdrawn by the first start after it`, async () => {
    test.setTimeout(150_000);
    const root = await mkdtemp(join(tmpdir(), `ao-wake-restart-failure-${provider}-`));
    let harness: HarnessApp | undefined;
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      harness = await launchHarness({ dataDir: root });
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `wakes-restart-failure-${provider}`, repo: {}, threads: [{ title: 'Restarted starter', provider }] }],
      });
      const { path: cwd, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const b = startBreaker(harness, provider, cwd);
      breaker = b;
      await b.brk();
      const startFailure = (retract: boolean) => (data: NotificationSend) =>
        data.id === `session-start:${thread}` && Boolean(data.retract) === retract;
      const raised = harness.waitForEvent<NotificationSend>('notification:send', startFailure(false), 60_000);
      const sent = await threadSend(dataDir, [thread, 'deploy 21 is done']);
      expect(sent.code, sent.stderr).toBe(0);
      await raised;

      await harness.close();
      harness = undefined;
      harness = await launchHarness({ dataDir: root });
      const h = harness;
      await b.mend();
      await setScenario(
        h,
        cwd,
        threadToolsScenario({ name: `wakes-restart-mended-${provider}`, provider, turns: [{ text: 'Noted.' }], afterTurns: 'silent' }),
      );
      const withdrawn = h.waitForEvent<NotificationSend>('notification:send', startFailure(true), 60_000);
      await h.rpc('StartSession', thread);
      await withdrawn;
      await h.rpc('StopSession', thread);
      await h.rpc('StartSession', thread);
      await h.rpc('StopSession', thread);
      expect(h.countEvents<NotificationSend>('notification:send', startFailure(true))).toBe(1);
      expect(h.countEvents<NotificationSend>('notification:send', startFailure(false))).toBe(0);
    } finally {
      await breaker?.cleanup();
      if (harness) await harness.close();
      await rm(root, { recursive: true, force: true });
    }
  });
}

// Archiving a thread takes back what the app queued for it whether or not a
// session is live: a wake and an outside send held behind a failed start do
// not reach the agent when the person unarchives the thread and starts it.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: archiving a thread whose start failed withdraws its held wake and send`, async ({ page }) => {
    test.setTimeout(150_000);
    const harness = await launchHarness();
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `wakes-archive-held-${provider}`, repo: {}, threads: [{ title: 'Shelved starter', provider }] }],
      });
      const { path: project, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', thread);
      await writeFile(join(project, 'say.sh'), SAY_SCRIPT);
      const { say } = sayControls(project);
      const b = startBreaker(harness, provider, home);
      breaker = b;
      await setScenario(
        harness,
        home,
        threadToolsScenario({
          name: `wakes-archive-held-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    tool: 'thread_watch',
                    args: { label: 'tick', command: runHere('say.sh'), every_seconds: 1, cwd: project },
                  },
                },
              ],
              text: 'Watching tick.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StartSession', thread);
      await harness.rpc('SendMessage', thread, 'watch tick', null);
      const ack = await awaitToolAnswer(harness, { tool: 'thread_watch', cwd: home, timeoutMs: 60_000 });
      expect(ack.isError, ack.text).toBe(false);
      await awaitTurnCompleted(harness, thread);

      const startFailure = (retract: boolean) => (data: NotificationSend) =>
        data.id === `session-start:${thread}` && Boolean(data.retract) === retract;
      const queued = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ? ORDER BY enqueued_at, rowid`).all(thread) as {
              send_id: string;
            }[]
          ).map((row) => row.send_id.split(':')[0]);
        } finally {
          db.close();
        }
      };
      const notice =
        'A message sent with agent-overflow thread send was not delivered: the thread was archived before the agent took it in. The script that sent it can send it again.';
      const notices = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db
              .prepare(`SELECT COUNT(*) AS n FROM items WHERE thread_id = ? AND kind = 'notification' AND summary = ?`)
              .get(thread, notice) as { n: number }
          ).n;
        } finally {
          db.close();
        }
      };

      // A wake and an outside send held behind a failed start.
      await harness.rpc('StopSession', thread);
      await b.brk();
      await say('report 1');
      await harness.waitForEvent<NotificationSend>('notification:send', startFailure(false), 60_000);
      const sent = await threadSend(dataDir, [thread, 'deploy 13 is done']);
      expect(sent.stderr).toBe('');
      expect(sent.code).toBe(0);
      await expect.poll(queued, { timeout: 15_000 }).toEqual(['agent-wake', 'agent-send']);

      // Archived while stopped: both come back out of the queue, the watch
      // is cancelled and the send leaves its notice.
      await harness.rpc('ArchiveThread', thread);
      await expect.poll(queued, { timeout: 15_000 }).toEqual([]);
      await expect.poll(notices, { timeout: 15_000 }).toBe(1);
      expect(dbWakes(dataDir, thread)).toEqual([
        { kind: 'watch', label: 'tick', state: 'cancelled', last_outcome: 'cancelled: the thread was archived' },
      ]);

      // Unarchived and mended, the person's start delivers neither.
      await harness.rpc('UnarchiveThread', thread);
      await b.mend();
      await setScenario(
        harness,
        home,
        threadToolsScenario({
          name: `wakes-archive-held-mended-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [{ text: 'Nothing waiting.' }],
          afterTurns: 'silent',
        }),
      );
      const withdrawn = harness.waitForEvent<NotificationSend>('notification:send', startFailure(true), 60_000);
      await harness.rpc('StartSession', thread);
      await withdrawn;
      await harness.rpc('SendMessage', thread, 'anything waiting?', null);
      await wakeInput(harness, home, 'anything waiting?');
      await awaitTurnCompleted(harness, thread);
      expect(countInputs(harness, home, 'Watch "tick" reported.')).toBe(0);
      expect(countInputs(harness, home, 'deploy 13 is done')).toBe(0);
      expect(queued()).toEqual([]);
      expect(notices()).toBe(1);

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Shelved starter', { exact: true }).click();
      await shoot(page, `${provider}-archive-withdraws-held-wake-and-send.png`, page.getByText(notice));
    } finally {
      await breaker?.cleanup();
      await harness.close();
    }
  });
}

// A finished remote job's completion held behind a failed start is the
// thread's result, not the app's chatter: archiving takes it back out of the
// queue and returns it to the job's watch, nothing delivers it while the
// thread is archived, and it arrives once after the thread is unarchived.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: archiving a thread whose start failed keeps its held remote completion for the unarchive`, async ({
    page,
  }) => {
    test.setTimeout(180_000);
    const [harness, remote] = await startTogether(launchHarness(), launchHarness());
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const computer = await pairBuildBox(harness, remote);
      const remoteSeed = await remote.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `remote-build-${provider}`, repo: {} }],
      });
      const { projectId: remoteProject, path: remotePath } = remoteSeed.projects[0];
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `remote-held-${provider}`, repo: {}, threads: [{ title: 'Remote builder', provider }] }],
      });
      const { path: cwd, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const b = startBreaker(harness, provider, cwd);
      breaker = b;
      const requestId = randomUUID();
      const release = join(remotePath, 'build.release');
      await setScenario(
        harness,
        cwd,
        threadToolsScenario({
          name: `remote-held-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    server: 'ao-remote-tools',
                    tool: 'remote_run',
                    args: {
                      computer_id: computer,
                      project_id: remoteProject,
                      request_id: requestId,
                      label: 'nightly build',
                      argv: ['sh', '-c', `while [ ! -f '${release}' ]; do sleep 0.1; done; echo 'build 812 finished: green'`],
                      wait_seconds: 0,
                    },
                  },
                },
              ],
              text: 'The build runs on Build box.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StartSession', thread);
      await harness.rpc('SendMessage', thread, 'run the nightly build on the build box', null);
      const started = await harness.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_run', cwd, timeoutMs: 60_000 });
      expect(started.report.isError, started.report.result).toBeFalsy();
      expect(started.report.result).toContain('"backgrounded":true');
      await awaitTurnCompleted(harness, thread);

      const startFailure = (retract: boolean) => (data: NotificationSend) =>
        data.id === `session-start:${thread}` && Boolean(data.retract) === retract;
      const queued = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ? ORDER BY enqueued_at, rowid`).all(thread) as {
              send_id: string;
            }[]
          ).map((row) => row.send_id.split(':')[0]);
        } finally {
          db.close();
        }
      };
      const watch = () => {
        const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return (
            db.prepare(`SELECT notification FROM remote_watches WHERE request_id = ?`).get(requestId) as {
              notification: string;
            }
          ).notification;
        } finally {
          db.close();
        }
      };
      const completion = `request_id: ${requestId}`;

      // The job finishes while the thread's start fails: its completion is
      // held in the queue.
      await harness.rpc('StopSession', thread);
      await b.brk();
      await writeFile(release, '');
      await harness.waitForEvent<NotificationSend>('notification:send', startFailure(false), 60_000);
      await expect.poll(queued, { timeout: 15_000 }).toEqual(['remote-completion']);
      expect(watch()).toBe('queued');

      // Archived: the completion goes back to its watch, and the watcher
      // leaves it there while the thread stays archived.
      await harness.rpc('ArchiveThread', thread);
      await expect.poll(queued, { timeout: 15_000 }).toEqual([]);
      expect(watch()).toBe('pending');
      // Six seconds of the watcher's normal pace, then the same state. No
      // signal marks a watcher pass over this row, so this shows nothing came
      // back in that time, not that a given number of passes saw the row.
      await new Promise((resolve) => setTimeout(resolve, 6_000));
      expect(queued()).toEqual([]);
      expect(watch()).toBe('pending');
      expect(countInputs(harness, cwd, completion)).toBe(0);

      // Unarchived and mended, the watcher delivers it, once.
      await setScenario(
        harness,
        cwd,
        threadToolsScenario({
          name: `remote-held-mended-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [{ text: 'Build 812 is green.' }],
          afterTurns: 'silent',
        }),
      );
      await b.mend();
      await harness.rpc('UnarchiveThread', thread);
      const input = await wakeInput(harness, cwd, completion);
      expect(input.report.input).toContain('Remote job finished on Build box: nightly build');
      expect(input.report.input).toContain('build 812 finished: green');
      await awaitTurnCompleted(harness, thread);
      expect(countInputs(harness, cwd, completion)).toBe(1);
      expect(queued()).toEqual([]);
      expect(watch()).toBe('queued');

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Remote builder', { exact: true }).click();
      await shoot(
        page,
        `${provider}-archive-keeps-held-remote-completion.png`,
        completionRows(page).last(),
      );
    } finally {
      await breaker?.cleanup();
      await Promise.allSettled([harness.close(), remote.close()]);
    }
  });

  test(`${provider}: a Stop on a held remote completion's incoming row dismisses it for good`, async ({ page }) => {
    test.setTimeout(180_000);
    const [harness, remote] = await startTogether(launchHarness(), launchHarness());
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const computer = await pairBuildBox(harness, remote);
      const remoteSeed = await remote.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `remote-build-${provider}`, repo: {} }],
      });
      const { projectId: remoteProject, path: remotePath } = remoteSeed.projects[0];
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `remote-stopped-${provider}`, repo: {}, threads: [{ title: 'Remote builder', provider }] }],
      });
      const { path: cwd, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const b = startBreaker(harness, provider, cwd);
      breaker = b;
      const requestId = randomUUID();
      const release = join(remotePath, 'build.release');
      await setScenario(
        harness,
        cwd,
        threadToolsScenario({
          name: `remote-stopped-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    server: 'ao-remote-tools',
                    tool: 'remote_run',
                    args: {
                      computer_id: computer,
                      project_id: remoteProject,
                      request_id: requestId,
                      label: 'nightly build',
                      argv: ['sh', '-c', `while [ ! -f '${release}' ]; do sleep 0.1; done; echo 'build 812 finished: green'`],
                      wait_seconds: 0,
                    },
                  },
                },
              ],
              text: 'The build runs on Build box.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StartSession', thread);
      await harness.rpc('SendMessage', thread, 'run the nightly build on the build box', null);
      const started = await harness.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_run', cwd, timeoutMs: 60_000 });
      expect(started.report.isError, started.report.result).toBeFalsy();
      await awaitTurnCompleted(harness, thread);

      const startFailure = (data: NotificationSend) => data.id === `session-start:${thread}` && !data.retract;
      const db = <T,>(read: (conn: DatabaseSync) => T): T => {
        const conn = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
        try {
          return read(conn);
        } finally {
          conn.close();
        }
      };
      const queued = () => db((conn) => (conn.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ?`).all(thread) as {
        send_id: string;
      }[]).map((row) => row.send_id.split(':')[0]));
      const watch = () => db((conn) => conn.prepare(`SELECT notification, wake_issue FROM remote_watches WHERE request_id = ?`).get(requestId) as {
        notification: string;
        wake_issue: string;
      });
      const completion = `request_id: ${requestId}`;

      // The job finishes while the thread's start fails: its completion is
      // held in the queue, an incoming row in the Agents tray.
      await harness.rpc('StopSession', thread);
      await b.brk();
      await writeFile(release, '');
      await harness.waitForEvent<NotificationSend>('notification:send', startFailure, 60_000);
      await expect.poll(queued, { timeout: 15_000 }).toEqual(['remote-completion']);
      await harness.open(page);
      await openAgents(page, 'Remote builder');
      const incoming = agentsBody(page).getByTestId('agent-incoming-row');
      await expect(incoming).toHaveCount(1);
      await expect(incoming).toHaveAttribute('data-kind', 'wake');

      // Stopped, it is dismissed on its watch, not handed back: nothing is
      // queued and the row stays gone six seconds later. No signal marks a
      // watcher pass over this row, so the wait shows nothing came back in
      // that time, not that a given number of passes saw the row.
      await incoming.getByTestId('agent-incoming-row-stop').click();
      await expect(page.getByTestId('toast').filter({ hasText: /Withdrew .* before this thread's agent read it\./ })).toBeVisible({ timeout: 30_000 });
      await expect(incoming).toHaveCount(0);
      expect(queued()).toEqual([]);
      expect(watch()).toEqual({ notification: 'dismissed', wake_issue: 'The user withdrew it from the queue before the agent read it.' });
      await new Promise((resolve) => setTimeout(resolve, 6_000));
      expect(queued()).toEqual([]);
      expect(watch().notification).toBe('dismissed');
      await expect(incoming).toHaveCount(0);

      // A mended start still never receives it, and remote_jobs keeps the result.
      await b.mend();
      await setScenario(harness, cwd, threadToolsScenario({
        name: `remote-stopped-mended-${provider}`, provider, echoUserMessage: true, turns: [{ text: 'Nothing to read.' }], afterTurns: 'silent',
      }));
      await harness.rpc('StartSession', thread);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(queued()).toEqual([]);
      expect(countInputs(harness, cwd, completion)).toBe(0);
      const jobs = await harness.rpc<{ requestId: string; notification: string; receipt: { state: string } }[]>('ListThreadRemoteCommands', thread);
      const job = jobs.find((j) => j.requestId === requestId);
      expect(job?.notification).toBe('dismissed');
      expect(job?.receipt.state).not.toBe('running');
    } finally {
      await breaker?.cleanup();
      await Promise.allSettled([harness.close(), remote.close()]);
    }
  });
}
