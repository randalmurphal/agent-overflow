// Answers and remote completions given back to their owners
// (docs/specs/standing-agents.md, "Wake supervisor"), end to end on both
// mock providers.
//
// Coverage: an answer and a remote completion queued at quit, never taken
// in by a dying session, or handed to the provider when the app crashed,
// going back to their request and watch; a Stop or an archive giving back
// what the session never confirmed; a Codex completion the provider took in
// before its session died reaching the agent twice and the person once; a
// Codex answer queued while an archive waits behind a start going back to
// its request; a person's Stop giving a dropped answer back to its request,
// owed and disarmed, and a dropped remote completion back to its watch,
// which delivers it again; archiving just after a remote job finished,
// before the watcher read the finish, cancelling the job still running and
// keeping the finished one's result for the unarchive; and forgetting a
// computer naming the results it discards for archived threads and
// discarding them, and a copy of such a thread refused with the cause.
import { randomUUID } from 'node:crypto';
import { mkdtemp, rename, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

import { test, expect, type SeedResult } from './fixtures.js';
import {
  HARNESS_TIMING,
  launchHarness,
  type HarnessApp,
  type HarnessMockEventData,
} from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { pairBuildBox } from './build-box-helpers.js';
import {
  aoMessageOpening,
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  awaitTurnCompleted,
  FOOTER_TOKEN_PATTERN,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import {
  startBreaker,
  wakeInput,
  countInputs,
  completionRows,
  shoot,
  type NotificationSend,
} from './standing-agent-wake-helpers.js';

// Moves a workspace away so no session can start in it, on either
// provider: a start refuses a workspace that is gone. A relaunch loses the
// mock's scenario rules, so a spec keeps its caller away until it has set
// them again; otherwise a start at boot runs the default script, which never
// echoes, before the client is connected to see its input.
function workspaceAway(cwd: string) {
  const away = `${cwd}.away`;
  let moved = false;
  return {
    move: async () => {
      await rename(cwd, away);
      moved = true;
    },
    restore: async () => {
      if (!moved) return;
      await rename(away, cwd);
      moved = false;
    },
  };
}

// A thread request's answer and a remote job's completion still queued when
// the app quits are not the person's words: the next boot gives them back to
// their owners instead of the composer. The request is owed and disarmed, and
// thread_status returns the answer; the watch is pending again, and the
// watcher queues the completion again for the next start, which delivers it
// once. Both are held in the durable queue behind a start that failed, which
// leaves them there for the crash on both providers.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: an answer and a remote completion queued at quit go back to their owners at boot`, async ({ page }) => {
    test.setTimeout(240_000);
    const ANSWER = 'The release notes are ready: four fixes and a faster export.';
    const root = await mkdtemp(join(tmpdir(), `ao-give-back-boot-${provider}-`));
    let harness: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    let breaker: ReturnType<typeof startBreaker> | undefined;
    let away: ReturnType<typeof workspaceAway> | undefined;
    try {
      [harness, remote] = await startTogether(launchHarness({ dataDir: root }), launchHarness());
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const computer = await pairBuildBox(harness, remote);
      const remoteSeed = await remote.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: `give-back-boot-build-${provider}`, repo: {} }],
      });
      const { projectId: remoteProject, path: remotePath } = remoteSeed.projects[0];
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [
          { name: `give-back-boot-caller-${provider}`, repo: {}, threads: [{ title: 'Weekly planner', provider }] },
          { name: `give-back-boot-target-${provider}`, repo: {}, threads: [{ title: 'Release notes', provider: 'claude' }] },
        ],
      });
      const callerPath = seed.projects[0].path;
      const caller = seed.projects[0].threadIds[0];
      const targetPath = seed.projects[1].path;
      const target = seed.projects[1].threadIds[0];
      const requestId = randomUUID();
      const release = join(remotePath, 'build.release');
      await setScenario(
        harness,
        targetPath,
        threadToolsScenario({
          name: `give-back-boot-target-${provider}`,
          provider: 'claude',
          turns: [
            {
              steps: [
                { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
                { gate: 'answer' },
                { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: ANSWER } } },
              ],
              text: 'Replied.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await setScenario(
        harness,
        callerPath,
        threadToolsScenario({
          name: `give-back-boot-caller-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    tool: 'thread_send',
                    args: { thread_id: target, message: 'Draft the release notes.', wait_seconds: 0, notify: true },
                  },
                },
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
              text: 'Asked for the notes, and the build runs on Build box.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StartSession', caller);
      await harness.rpc('SendMessage', caller, 'ask for the release notes and run the nightly build', null);
      const sent = await awaitToolAnswer<{ token: string; outcome: string; notify: boolean }>(harness, {
        tool: 'thread_send',
        cwd: callerPath,
        timeoutMs: 60_000,
      });
      expect(sent.isError, sent.text).toBe(false);
      expect(sent.value!.outcome).toBe('backgrounded');
      const token = sent.value!.token;
      const started = await harness.awaitMcpResult({
        server: 'ao-remote-tools',
        tool: 'remote_run',
        cwd: callerPath,
        timeoutMs: 60_000,
      });
      expect(started.report.isError, started.report.result).toBeFalsy();
      expect(started.report.result).toContain('"backgrounded":true');
      await awaitTurnCompleted(harness, caller);
      const responder = await awaitGate(harness, 'answer', targetPath);

      const db = () => new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
      const queued = () => {
        const conn = db();
        try {
          return (
            conn.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ? ORDER BY enqueued_at, rowid`).all(caller) as {
              send_id: string;
            }[]
          ).map((row) => row.send_id.split(':')[0]);
        } finally {
          conn.close();
        }
      };
      const owners = () => {
        const conn = db();
        try {
          const request = conn
            .prepare(`SELECT delivered_at IS NOT NULL AS delivered, delivered_how AS how, notify FROM thread_requests WHERE token = ?`)
            .get(token) as { delivered: number; how: string; notify: number };
          const watch = (
            conn.prepare(`SELECT notification FROM remote_watches WHERE request_id = ?`).get(requestId) as { notification: string }
          ).notification;
          return { request, watch };
        } finally {
          conn.close();
        }
      };
      const composer = () => {
        const conn = db();
        try {
          const draft = conn.prepare(`SELECT content FROM thread_drafts WHERE thread_id = ?`).get(caller) as
            | { content: string }
            | undefined;
          const bubbles = (
            conn
              .prepare(
                `SELECT COUNT(*) AS n FROM items WHERE thread_id = ? AND kind = 'user_text' AND summary LIKE ?`,
              )
              .get(caller, `%${ANSWER}%`) as { n: number }
          ).n;
          return { draft: draft?.content ?? '', bubbles };
        } finally {
          conn.close();
        }
      };
      const completion = `request_id: ${requestId}`;

      // The answer arrives and the job finishes while the caller's start
      // fails: both are held in the durable queue, and the app crashes.
      await harness.rpc('StopSession', caller);
      const b = startBreaker(harness, provider, callerPath);
      breaker = b;
      await b.brk();
      await advanceGate(harness, responder.mockId, 'answer');
      await harness.waitForEvent<NotificationSend>(
        'notification:send',
        (data) => data.id === `session-start:${caller}` && !data.retract,
        60_000,
      );
      await expect.poll(queued, { timeout: 15_000 }).toEqual(['thread-wake']);
      await writeFile(release, '');
      await expect.poll(queued, { timeout: 30_000 }).toEqual(['thread-wake', 'remote-completion']);
      expect(owners()).toEqual({ request: { delivered: 1, how: 'queued', notify: 1 }, watch: 'queued' });
      expect(await harness.crash()).toBe(true);
      harness = undefined;
      await b.mend();
      away = workspaceAway(callerPath);
      await away.move();

      // The boot sweep gives both back. The scenario rules went with the
      // crashed process, so the caller stays away until its own is set
      // again.
      harness = await launchHarness({ dataDir: root });
      await setScenario(
        harness,
        callerPath,
        threadToolsScenario({
          name: `give-back-boot-relaunched-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [{ text: 'Build 812 is green.' }],
          afterTurns: 'silent',
        }),
      );
      // The request is owed and disarmed, and the watch, given its
      // completion back, has queued it again behind a start that fails.
      await expect.poll(queued, { timeout: 60_000 }).toEqual(['remote-completion']);
      expect(owners()).toEqual({ request: { delivered: 0, how: '', notify: 0 }, watch: 'queued' });
      expect(composer()).toEqual({ draft: '', bubbles: 0 });

      // The next start delivers the completion once; the answer never
      // reaches the agent as a message.
      await away.restore();
      const relaunched = harness;
      const deliveries = () => countInputs(relaunched, callerPath, completion);
      await harness.rpc('StartSession', caller);
      const input = await wakeInput(harness, callerPath, completion, 60_000);
      expect(input.report.input).toContain(`${aoMessageOpening('the remote job "nightly build" on "Build box"')}Remote job finished on Build box: nightly build`);
      expect(input.report.input).toContain('build 812 finished: green');
      await awaitTurnCompleted(harness, caller);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(deliveries()).toBe(1);
      expect(countInputs(harness, callerPath, ANSWER)).toBe(0);
      expect(queued()).toEqual([]);
      expect(owners()).toEqual({ request: { delivered: 0, how: '', notify: 0 }, watch: 'queued' });
      expect(composer().draft).toBe('');

      // The agent reads the answer with thread_status. The mock numbers
      // tool calls per process, so this one names its own id.
      await setScenario(
        harness,
        callerPath,
        threadToolsScenario({
          name: `give-back-boot-status-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [{ call: { tool: 'thread_status', args: { tokens: [token] }, toolUseId: 'status-after-boot' } }],
              text: 'Read the notes.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StopSession', caller);
      await harness.rpc('StartSession', caller);
      await harness.rpc('SendMessage', caller, 'did the notes come back?', null);
      const status = await awaitToolAnswer<{ requests?: { token: string; answer?: string }[] }>(harness, {
        tool: 'thread_status',
        cwd: callerPath,
        timeoutMs: 60_000,
      });
      expect(status.isError, status.text).toBe(false);
      expect(status.value!.requests![0].token).toBe(token);
      expect(status.value!.requests![0].answer).toContain(ANSWER);
      await awaitTurnCompleted(harness, caller);
      expect(countInputs(harness, callerPath, ANSWER)).toBe(0);

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Weekly planner', { exact: true }).click();
      await shoot(
        page,
        `${provider}-boot-returns-a-completion-to-its-watch.png`,
        completionRows(page).last(),
      );
      await shoot(page, `${provider}-boot-returns-an-answer-to-its-request.png`, page.getByText('Read the notes.').last());
    } finally {
      await breaker?.cleanup();
      await away?.restore().catch(() => undefined);
      await Promise.allSettled([harness?.close(), remote?.close()]);
      await rm(root, { recursive: true, force: true });
    }
  });
}

// giveBackCaller sets up the round the give-back cases share: a caller
// thread on `provider` asks a Claude thread for release notes and starts a
// nightly build on a paired Build box, then rests with both outstanding.
// The target holds its reply at the `answer` gate, and the build runs until
// `release` exists. Each helper acts on app(), the running process, which a
// relaunch replaces. state() reads the request, the watch, the composer and
// the caller's queue rows from the database.
async function giveBackCaller(app: () => HarnessApp, remote: HarnessApp, provider: ProviderName, name: string, answer: string) {
  await app().rpc('UpdateSettings', { notifyQuietWhen: 'never' });
  const dataDir = app().bootstrap.dataDir;
  const computer = await pairBuildBox(app(), remote);
  const remoteSeed = await remote.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `${name}-build-${provider}`, repo: {} }],
  });
  const { projectId: remoteProject, path: remotePath } = remoteSeed.projects[0];
  const seed = await app().rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${name}-caller-${provider}`, repo: {}, threads: [{ title: 'Weekly planner', provider }] },
      { name: `${name}-target-${provider}`, repo: {}, threads: [{ title: 'Release notes', provider: 'claude' }] },
    ],
  });
  const callerPath = seed.projects[0].path;
  const caller = seed.projects[0].threadIds[0];
  const targetPath = seed.projects[1].path;
  const target = seed.projects[1].threadIds[0];
  const requestId = randomUUID();
  const release = join(remotePath, 'build.release');
  await setScenario(
    app(),
    targetPath,
    threadToolsScenario({
      name: `${name}-target-${provider}`,
      provider: 'claude',
      turns: [
        {
          steps: [
            { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
            { gate: 'answer' },
            { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: answer } } },
          ],
          text: 'Replied.',
        },
      ],
      afterTurns: 'silent',
    }),
  );
  await setScenario(
    app(),
    callerPath,
    threadToolsScenario({
      name: `${name}-caller-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_send',
                args: { thread_id: target, message: 'Draft the release notes.', wait_seconds: 0, notify: true },
              },
            },
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
          text: 'Asked for the notes, and the build runs on Build box.',
        },
      ],
      afterTurns: 'silent',
    }),
  );
  await app().rpc('StartSession', caller);
  await app().rpc('SendMessage', caller, 'ask for the release notes and run the nightly build', null);
  const sent = await awaitToolAnswer<{ token: string; outcome: string }>(app(), {
    tool: 'thread_send',
    cwd: callerPath,
    timeoutMs: 60_000,
  });
  expect(sent.isError, sent.text).toBe(false);
  expect(sent.value!.outcome).toBe('backgrounded');
  const token = sent.value!.token;
  const started = await app().awaitMcpResult({
    server: 'ao-remote-tools',
    tool: 'remote_run',
    cwd: callerPath,
    timeoutMs: 60_000,
  });
  expect(started.report.result).toContain('"backgrounded":true');
  await awaitTurnCompleted(app(), caller);
  const responder = await awaitGate(app(), 'answer', targetPath);

  const state = () => {
    const conn = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
    try {
      const request = conn
        .prepare(`SELECT delivered_at IS NOT NULL AS delivered, delivered_how AS how, notify FROM thread_requests WHERE token = ?`)
        .get(token) as { delivered: number; how: string; notify: number };
      const watch = (
        conn.prepare(`SELECT notification FROM remote_watches WHERE request_id = ?`).get(requestId) as { notification: string }
      ).notification;
      const draft = conn.prepare(`SELECT content FROM thread_drafts WHERE thread_id = ?`).get(caller) as
        | { content: string }
        | undefined;
      const rows = (conn.prepare(`SELECT COUNT(*) AS n FROM flush_queue_items WHERE thread_id = ?`).get(caller) as { n: number }).n;
      return { request, watch, draft: draft?.content ?? '', rows };
    } finally {
      conn.close();
    }
  };
  // The second phase of each delivery mark, and the watch's redelivery
  // pace (wake_attempts, next_check).
  const marks = () => {
    const conn = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
    try {
      const request = conn.prepare(`SELECT confirmed_at IS NOT NULL AS confirmed FROM thread_requests WHERE token = ?`).get(token) as {
        confirmed: number;
      };
      const watch = conn
        .prepare(
          `SELECT confirmed_at IS NOT NULL AS confirmed, wake_attempts AS attempts, next_check AS nextCheck FROM remote_watches WHERE request_id = ?`,
        )
        .get(requestId) as { confirmed: number; attempts: number; nextCheck: number };
      return { answer: request.confirmed, completion: watch.confirmed, attempts: watch.attempts, nextCheck: watch.nextCheck };
    } finally {
      conn.close();
    }
  };
  // Each message starts a session that reads it and holds, never echoing
  // it: Claude's still starting, Codex's in a turn held at its first step.
  const holdNextSession = async (scenario: string) => {
    await app().rpc('StopSession', caller);
    await setScenario(
      app(),
      callerPath,
      provider === 'claude'
        ? { ...threadToolsScenario({ name: scenario, provider, turns: [{ text: 'unreached' }] }), startupDelayMs: 30_000 }
        : threadToolsScenario({
            name: scenario,
            provider,
            echoUserMessage: true,
            turns: [{ steps: [{ gate: 'hold' }], text: 'unreached', echo: false }],
            afterTurns: 'silent',
          }),
    );
  };
  // The mock can hold before the app settles the write that reached it, so
  // the queue row a delivery removes is polled for, not read once.
  const heldMock = async (input: HarnessMockEventData) =>
    provider === 'claude' ? input.mockId : (await awaitGate(app(), 'hold', callerPath)).mockId;
  const healthy = (scenario: string) =>
    setScenario(
      app(),
      callerPath,
      threadToolsScenario({ name: scenario, provider, echoUserMessage: true, turns: [{ text: 'Build 812 is green.' }], afterTurns: 'silent' }),
    );
  return {
    dataDir,
    caller,
    callerPath,
    token,
    requestId,
    release,
    responder,
    completion: `request_id: ${requestId}`,
    state,
    marks,
    holdNextSession,
    heldMock,
    healthy,
  };
}

// A thread request's answer and a remote job's completion that a session
// read and died before taking in go back to their owners, not to the
// composer. The provider was handed each one, so no queue row is left: the
// request is owed and disarmed, and the watch is pending again. A session
// that keeps dying would be restarted for the completion every few seconds,
// so the watcher delivers it again on the ladder a request's undelivered
// wake is retried on: the first redelivery is half a minute out. Each goes
// to a session that reads it and holds: Claude's still starting, Codex's in
// a turn that never echoes it.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: an answer and a remote completion a dying session never took in go back to their owners`, async ({
    page,
  }) => {
    test.setTimeout(300_000);
    const ANSWER = 'The release notes are ready: five fixes.';
    const [harness, remote] = await startTogether(launchHarness(), launchHarness());
    try {
      const g = await giveBackCaller(() => harness, remote, provider, 'give-back-death', ANSWER);

      // ---- The answer.
      await g.holdNextSession(`give-back-death-answer-held-${provider}`);
      await advanceGate(harness, g.responder.mockId, 'answer');
      const answerInput = await wakeInput(harness, g.callerPath, ANSWER, 60_000);
      const answerMock = await g.heldMock(answerInput);
      await expect.poll(() => g.state()).toMatchObject({ request: { delivered: 1, how: 'queued', notify: 1 }, rows: 0 });
      expect(g.marks().answer).toBe(0);
      await g.healthy(`give-back-death-answer-after-${provider}`);
      const answerDeath = harness.waitForEvent<NotificationSend>(
        'notification:send',
        (data) => !data.retract && data.kind === 'error' && data.id === `thread:${g.caller}`,
        30_000,
      );
      await harness.rpc('HarnessMockCommand', answerMock, { type: 'exit', code: 1 });
      expect((await answerDeath).body).toBe('The provider stopped. Open the thread to see why.');
      await expect
        .poll(() => g.state().request, { timeout: 30_000 })
        .toEqual({ delivered: 0, how: '', notify: 0 });
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(g.state()).toMatchObject({ request: { delivered: 0, how: '', notify: 0 }, draft: '', rows: 0 });
      expect(countInputs(harness, g.callerPath, ANSWER)).toBe(1);

      // ---- The completion.
      await g.holdNextSession(`give-back-death-completion-held-${provider}`);
      await writeFile(g.release, '');
      const completionInput = await wakeInput(harness, g.callerPath, g.completion, 60_000);
      const completionMock = await g.heldMock(completionInput);
      await expect.poll(() => g.state()).toMatchObject({ watch: 'queued', rows: 0 });
      await g.healthy(`give-back-death-completion-after-${provider}`);
      // The first death's notification may still stand (Claude's held start
      // never retracts it), so this one is not waited for.
      await harness.rpc('HarnessMockCommand', completionMock, { type: 'exit', code: 1 });
      // The watch is pending again with its first redelivery booked half a
      // minute out, and the watcher, which polls every two seconds, leaves it
      // alone until then.
      await expect.poll(() => ({ watch: g.state().watch, attempts: g.marks().attempts }), { timeout: 30_000 }).toEqual({
        watch: 'pending',
        attempts: 1,
      });
      const booked = g.marks().nextCheck;
      const out = booked - Date.now();
      expect(out).toBeGreaterThan(20_000);
      expect(out).toBeLessThanOrEqual(30_000);
      await new Promise((resolve) => setTimeout(resolve, 10_000));
      expect(countInputs(harness, g.callerPath, g.completion)).toBe(1);
      expect(g.state().watch).toBe('pending');
      // Then it is delivered once to the session the watcher starts, and the
      // echo confirms it.
      await expect.poll(() => countInputs(harness, g.callerPath, g.completion), { timeout: 60_000 }).toBe(2);
      expect(Date.now()).toBeGreaterThanOrEqual(booked);
      await awaitTurnCompleted(harness, g.caller);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(countInputs(harness, g.callerPath, g.completion)).toBe(2);
      expect(g.state()).toEqual({ request: { delivered: 0, how: '', notify: 0 }, watch: 'queued', draft: '', rows: 0 });
      expect(g.marks().completion).toBe(1);

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Weekly planner', { exact: true }).click();
      await shoot(
        page,
        `${provider}-death-returns-a-completion-to-its-watch.png`,
        completionRows(page).last(),
      );
    } finally {
      await Promise.allSettled([harness.close(), remote.close()]);
    }
  });
}

// A person's Stop or an archive that ends a session holding an answer or a
// completion it was handed and never confirmed gives it back: the request is
// owed and disarmed, and the watch is pending. Neither goes to the composer.
// A Stop or an archive is not a session dying on its own, so the completion
// is due at once: the watcher delivers it at its next poll once the thread
// is unarchived, well inside the half minute a session end would book.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a Stop or an archive gives back an answer and a completion the session never confirmed`, async ({ page }) => {
    test.setTimeout(240_000);
    const ANSWER = 'The release notes are ready: six fixes.';
    const [harness, remote] = await startTogether(launchHarness(), launchHarness());
    try {
      const g = await giveBackCaller(() => harness, remote, provider, 'give-back-stop', ANSWER);

      // ---- The answer, and a Stop.
      await g.holdNextSession(`give-back-stop-answer-held-${provider}`);
      await advanceGate(harness, g.responder.mockId, 'answer');
      await g.heldMock(await wakeInput(harness, g.callerPath, ANSWER, 60_000));
      await expect.poll(() => g.state()).toMatchObject({ request: { delivered: 1, how: 'queued', notify: 1 }, rows: 0 });
      expect(g.marks().answer).toBe(0);
      await harness.rpc('StopSession', g.caller);
      expect(g.state()).toMatchObject({ request: { delivered: 0, how: '', notify: 0 }, draft: '', rows: 0 });
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(countInputs(harness, g.callerPath, ANSWER)).toBe(1);
      expect(g.state()).toMatchObject({ request: { delivered: 0, how: '', notify: 0 }, draft: '', rows: 0 });

      // ---- The completion, and an archive.
      await g.holdNextSession(`give-back-stop-completion-held-${provider}`);
      await writeFile(g.release, '');
      await g.heldMock(await wakeInput(harness, g.callerPath, g.completion, 60_000));
      await expect.poll(() => g.state()).toMatchObject({ watch: 'queued', rows: 0 });
      expect(g.marks().completion).toBe(0);
      await g.healthy(`give-back-stop-completion-after-${provider}`);
      await harness.rpc('ArchiveThread', g.caller);
      expect(g.state()).toMatchObject({ watch: 'pending', draft: '', rows: 0 });
      expect(g.marks().attempts).toBe(0);
      // The watcher skips an archived thread's watches.
      await new Promise((resolve) => setTimeout(resolve, 6_000));
      expect(countInputs(harness, g.callerPath, g.completion)).toBe(1);
      expect(g.state().watch).toBe('pending');

      await harness.rpc('UnarchiveThread', g.caller);
      await expect.poll(() => countInputs(harness, g.callerPath, g.completion), { timeout: 20_000 }).toBe(2);
      await awaitTurnCompleted(harness, g.caller);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(countInputs(harness, g.callerPath, g.completion)).toBe(2);
      expect(g.state()).toEqual({ request: { delivered: 0, how: '', notify: 0 }, watch: 'queued', draft: '', rows: 0 });
      expect(g.marks()).toMatchObject({ completion: 1, attempts: 0 });

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Weekly planner', { exact: true }).click();
      await shoot(
        page,
        `${provider}-archive-returns-an-unconfirmed-completion-to-its-watch.png`,
        completionRows(page).last(),
      );
    } finally {
      await Promise.allSettled([harness.close(), remote.close()]);
    }
  });
}

// An answer and a completion the app handed a provider that had not
// confirmed them when it crashed have no queue row left, so only their
// owners' delivery marks say they may not have arrived: queued, and never
// confirmed by an echo. The boot sweep gives back every such owner, as it
// gives back a queued row. Each kind is handed to a session that holds, and
// the app crashes; Codex echoes a message steered into a running turn at
// once, so each needs its own session.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: an answer and a completion handed to the provider when the app crashed go back to their owners at boot`, async ({
    page,
  }) => {
    test.setTimeout(300_000);
    const ANSWER = 'The release notes are ready: seven fixes.';
    const root = await mkdtemp(join(tmpdir(), `ao-give-back-crash-${provider}-`));
    let harness: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    let away: ReturnType<typeof workspaceAway> | undefined;
    try {
      [harness, remote] = await startTogether(launchHarness({ dataDir: root }), launchHarness());
      const g = await giveBackCaller(() => harness!, remote, provider, 'give-back-crash', ANSWER);
      away = workspaceAway(g.callerPath);
      // The scenario rules go with the crashed process, so the caller stays
      // away until its own is set again; the caller restores it.
      const relaunch = async () => {
        expect(await harness!.crash()).toBe(true);
        harness = undefined;
        await away!.move();
        harness = await launchHarness({ dataDir: root });
        await g.healthy(`give-back-crash-relaunched-${provider}`);
        return harness;
      };

      // ---- The answer.
      await g.holdNextSession(`give-back-crash-answer-held-${provider}`);
      await advanceGate(harness, g.responder.mockId, 'answer');
      await g.heldMock(await wakeInput(harness, g.callerPath, ANSWER, 60_000));
      await expect.poll(() => g.state()).toMatchObject({ request: { delivered: 1, how: 'queued', notify: 1 }, rows: 0 });
      expect(g.marks().answer).toBe(0);
      let app = await relaunch();
      expect(g.state()).toMatchObject({ request: { delivered: 0, how: '', notify: 0 }, draft: '', rows: 0 });
      await away.restore();

      // ---- The completion.
      await g.holdNextSession(`give-back-crash-completion-held-${provider}`);
      await writeFile(g.release, '');
      await g.heldMock(await wakeInput(app, g.callerPath, g.completion, 60_000));
      await expect.poll(() => g.state()).toMatchObject({ watch: 'queued', rows: 0 });
      expect(g.marks().completion).toBe(0);
      app = await relaunch();
      const relaunched = app;
      const deliveries = () => countInputs(relaunched, g.callerPath, g.completion);
      // The watch, given its completion back, queues it again behind a
      // start that fails; the next start delivers it.
      await expect.poll(() => g.state(), { timeout: 60_000 }).toMatchObject({ watch: 'queued', rows: 1 });
      expect(g.state()).toMatchObject({ request: { delivered: 0, how: '', notify: 0 }, draft: '' });
      await away.restore();
      await app.rpc('StartSession', g.caller);
      const input = await wakeInput(app, g.callerPath, g.completion, 60_000);
      expect(input.report.input).toContain('build 812 finished: green');
      await awaitTurnCompleted(app, g.caller);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(deliveries()).toBe(1);
      expect(countInputs(app, g.callerPath, ANSWER)).toBe(0);
      expect(g.state()).toEqual({ request: { delivered: 0, how: '', notify: 0 }, watch: 'queued', draft: '', rows: 0 });

      // The agent reads the answer with thread_status. The mock numbers tool
      // calls per process, so this one names its own id.
      await setScenario(
        app,
        g.callerPath,
        threadToolsScenario({
          name: `give-back-crash-status-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [{ call: { tool: 'thread_status', args: { tokens: [g.token] }, toolUseId: 'status-after-crash' } }],
              text: 'Read the notes.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await app.rpc('StopSession', g.caller);
      await app.rpc('StartSession', g.caller);
      await app.rpc('SendMessage', g.caller, 'did the notes come back?', null);
      const status = await awaitToolAnswer<{ requests?: { token: string; answer?: string }[] }>(app, {
        tool: 'thread_status',
        cwd: g.callerPath,
        timeoutMs: 60_000,
      });
      expect(status.isError, status.text).toBe(false);
      expect(status.value!.requests![0].token).toBe(g.token);
      expect(status.value!.requests![0].answer).toContain(ANSWER);
      await awaitTurnCompleted(app, g.caller);

      await app.open(page);
      await page.getByTestId('thread-row').getByText('Weekly planner', { exact: true }).click();
      await shoot(
        page,
        `${provider}-crash-returns-an-unconfirmed-completion-to-its-watch.png`,
        completionRows(page).last(),
      );
      await shoot(page, `${provider}-crash-returns-an-unconfirmed-answer-to-its-request.png`, page.getByText('Read the notes.').last());
    } finally {
      await away?.restore().catch(() => undefined);
      await Promise.allSettled([harness?.close(), remote?.close()]);
      await rm(root, { recursive: true, force: true });
    }
  });
}

// The cost the give-back accepts, shown on Codex. The provider records a
// message in its thread when it takes it in, and the session died before
// the echo confirmed the completion, so it goes back to its watch and the
// watcher delivers it again into the thread the next session resumes. The
// agent then holds it twice; the person sees one row for it and an empty
// composer. A crash leaves the same state, but its relaunch resumes the
// thread during boot, before a client is connected to see the resume.
test('codex: a completion the provider took in before its session died reaches the agent twice and the person once', async ({ page }) => {
  test.setTimeout(300_000);
  const provider = 'codex';
  const [harness, remote] = await startTogether(launchHarness(), launchHarness());
  try {
    const g = await giveBackCaller(() => harness, remote, provider, 'give-back-duplicate', 'Unused.');
    // A session that records each message in its thread as it takes it in,
    // as Codex writes the user message to the rollout, then holds the turn
    // without echoing it.
    await harness.rpc('StopSession', g.caller);
    await setScenario(harness, g.callerPath, {
      ...threadToolsScenario({
        name: 'give-back-duplicate-held',
        provider,
        echoUserMessage: true,
        turns: [{ steps: [{ gate: 'hold' }], text: 'unreached', echo: false }],
        afterTurns: 'silent',
      }),
      codex: { keepHistory: true },
    });
    await writeFile(g.release, '');
    const held = await g.heldMock(await wakeInput(harness, g.callerPath, g.completion, 60_000));
    await expect.poll(() => g.state()).toMatchObject({ watch: 'queued', rows: 0 });
    expect(g.marks().completion).toBe(0);
    await g.healthy('give-back-duplicate-after');
    await harness.rpc('HarnessMockCommand', held, { type: 'exit', code: 1 });
    await expect.poll(() => g.state().watch, { timeout: 30_000 }).toBe('pending');

    // The redelivery resumes the thread, which already holds the completion.
    const resumed = () =>
      harness.countEvents<HarnessMockEventData>(
        'harness:mock',
        (ev) => ev.report.kind === 'history_resumed' && ev.cwd === g.callerPath && (ev.report.input ?? '').includes(g.completion),
      );
    await expect.poll(() => countInputs(harness, g.callerPath, g.completion), { timeout: 90_000 }).toBe(2);
    await awaitTurnCompleted(harness, g.caller);
    await new Promise((resolve) => setTimeout(resolve, 4_000));
    expect(
      { resumed: resumed(), redelivered: countInputs(harness, g.callerPath, g.completion) - 1 },
      'the resumed session holds the completion from its thread and from the redelivery',
    ).toEqual({ resumed: 1, redelivered: 1 });
    const conn = new DatabaseSync(join(g.dataDir, 'agent-overflow.db'), { readOnly: true });
    let rows: number;
    try {
      rows = (
        conn
          .prepare(`SELECT COUNT(*) AS n FROM items WHERE thread_id = ? AND kind = 'user_text' AND summary LIKE ?`)
          .get(g.caller, `%${g.requestId}%`) as { n: number }
      ).n;
    } finally {
      conn.close();
    }
    expect(rows, 'the person sees the completion once').toBe(1);
    expect(g.state()).toMatchObject({ watch: 'queued', draft: '', rows: 0 });
    expect(g.marks().completion).toBe(1);

    await harness.open(page);
    await page.getByTestId('thread-row').getByText('Weekly planner', { exact: true }).click();
    const completion = completionRows(page);
    await expect(completion).toHaveCount(1);
    await expect(page.getByTestId('composer-root').locator('textarea')).toHaveValue('');
    await shoot(page, 'codex-duplicate-after-death-shows-one-completion.png', completion);
  } finally {
    await Promise.allSettled([harness.close(), remote.close()]);
  }
});

// A thread request's answer that arrives while a person's archive waits
// behind a start is written before the archive's flip, and the start's own
// flush hands it to a dispatch queued behind the archive too. The archive
// stops the session the start brought up and takes the answer back to its
// request: owed, disarmed and still readable with thread_status. Nothing of
// it reaches the agent, then or once the thread is unarchived and started.
// Codex only: Claude's start ends at the spawn and does not wait on the CLI,
// so the mock cannot hold it, and the archive never waits behind one.
test('codex: an answer queued while an archive waits behind a start goes back to its request', async ({ page }) => {
  test.setTimeout(180_000);
  const HOLD_MS = 20_000;
  const ANSWER = 'The release notes are ready: three fixes and one feature.';
  const harness = await launchHarness();
  try {
    const dataDir = harness.bootstrap.dataDir;
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: 'wakes-archive-answer-caller', repo: {}, threads: [{ title: 'Weekly planner', provider: 'codex' }] },
        { name: 'wakes-archive-answer-target', repo: {}, threads: [{ title: 'Release notes', provider: 'claude' }] },
      ],
    });
    const callerPath = seed.projects[0].path;
    const caller = seed.projects[0].threadIds[0];
    const targetPath = seed.projects[1].path;
    const target = seed.projects[1].threadIds[0];
    await setScenario(
      harness,
      targetPath,
      threadToolsScenario({
        name: 'wakes-archive-answer-target',
        provider: 'claude',
        turns: [
          {
            steps: [
              { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
              { gate: 'answer' },
              { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: ANSWER } } },
            ],
            text: 'Replied.',
          },
        ],
        afterTurns: 'silent',
      }),
    );
    await setScenario(
      harness,
      callerPath,
      threadToolsScenario({
        name: 'wakes-archive-answer-caller',
        provider: 'codex',
        echoUserMessage: true,
        turns: [
          {
            steps: [
              {
                call: {
                  tool: 'thread_send',
                  args: { thread_id: target, message: 'Draft the release notes.', wait_seconds: 0, notify: true },
                },
              },
            ],
            text: 'Asked for the notes.',
          },
        ],
        afterTurns: 'silent',
      }),
    );
    await harness.rpc('StartSession', caller);
    const first = await harness.waitForEvent<HarnessMockEventData>(
      'harness:mock',
      (ev) => ev.report.kind === 'registered' && ev.cwd === callerPath,
    );
    await harness.rpc('SendMessage', caller, 'ask for the release notes', null);
    const sent = await awaitToolAnswer<{ token: string; outcome: string; notify: boolean }>(harness, {
      tool: 'thread_send',
      cwd: callerPath,
      timeoutMs: 60_000,
    });
    expect(sent.isError, sent.text).toBe(false);
    expect(sent.value!.outcome).toBe('backgrounded');
    expect(sent.value!.notify).toBe(true);
    const token = sent.value!.token;
    await awaitTurnCompleted(harness, caller);
    const responder = await awaitGate(harness, 'answer', targetPath);
    await harness.rpc('StopSession', caller);

    const db = () => new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
    const queued = () => {
      const conn = db();
      try {
        return (
          conn.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ? ORDER BY enqueued_at, rowid`).all(caller) as {
            send_id: string;
          }[]
        ).map((row) => row.send_id);
      } finally {
        conn.close();
      }
    };
    const request = () => {
      const conn = db();
      try {
        return conn
          .prepare(`SELECT delivered_at IS NOT NULL AS delivered, delivered_how AS how, notify FROM thread_requests WHERE token = ?`)
          .get(token) as { delivered: number; how: string; notify: number };
      } finally {
        conn.close();
      }
    };
    const archived = () => {
      const conn = db();
      try {
        return (conn.prepare(`SELECT archived FROM threads WHERE id = ?`).get(caller) as { archived: number }).archived;
      } finally {
        conn.close();
      }
    };

    // The person's start is held at the handshake, holding the thread's
    // action lock, and the person archives the thread while it is.
    await setScenario(harness, callerPath, {
      ...threadToolsScenario({
        name: 'wakes-archive-answer-held',
        provider: 'codex',
        echoUserMessage: true,
        turns: [{ text: 'Started.' }],
        afterTurns: 'silent',
      }),
      startupDelayMs: HOLD_MS,
    });
    const started = Date.now();
    let settled = '';
    const starting = harness.rpcWithin(HOLD_MS + 30_000, 'StartSession', caller).then(() => (settled ||= 'start'));
    const held = await harness.waitForEvent<HarnessMockEventData>(
      'harness:mock',
      (ev) => ev.report.kind === 'registered' && ev.cwd === callerPath && ev.mockId !== first.mockId,
      15_000,
    );
    expect(held.mockId).not.toBe('');
    const archiving = harness.rpcWithin(HOLD_MS + 30_000, 'ArchiveThread', caller).then(() => (settled ||= 'archive'));

    // The answer arrives: its wake is written while the thread is still
    // unarchived, before the archive's flip.
    await advanceGate(harness, responder.mockId, 'answer');
    await expect.poll(queued, { timeout: 15_000 }).toEqual([`thread-wake:${token}`]);
    expect(request()).toEqual({ delivered: 1, how: 'queued', notify: 1 });
    expect(archived()).toBe(0);
    expect(settled, 'the held start and the archive were both still waiting when the answer was queued').toBe('');

    await Promise.all([starting, archiving]);
    expect(Date.now() - started).toBeGreaterThanOrEqual(HOLD_MS);
    const owedAndDisarmed = () => ({ queued: queued(), request: request(), archived: archived() });
    const want = { queued: [], request: { delivered: 0, how: '', notify: 0 }, archived: 1 };
    expect(owedAndDisarmed()).toEqual(want);
    // The dispatch the start's flush queued behind the archive finds nothing
    // to send, and nothing is left for the next start.
    await new Promise((resolve) => setTimeout(resolve, 3_000));
    expect(owedAndDisarmed()).toEqual(want);
    expect((await harness.rpc<unknown[] | null>('GetQueueState', caller)) ?? []).toEqual([]);
    expect(countInputs(harness, callerPath, ANSWER)).toBe(0);

    // Unarchived and started, the thread gets nothing it did not ask for; the
    // agent still reads the answer with thread_status.
    await setScenario(
      harness,
      callerPath,
      threadToolsScenario({
        name: 'wakes-archive-answer-unarchived',
        provider: 'codex',
        echoUserMessage: true,
        // The mock numbers tool calls per process, so this one names its own
        // id rather than take the first process's thread_send row.
        turns: [
          {
            steps: [{ call: { tool: 'thread_status', args: { tokens: [token] }, toolUseId: 'status-after-unarchive' } }],
            text: 'Read the notes.',
          },
        ],
        afterTurns: 'silent',
      }),
    );
    await harness.rpc('UnarchiveThread', caller);
    await harness.rpc('StartSession', caller);
    await harness.rpc('SendMessage', caller, 'did the notes come back?', null);
    const status = await awaitToolAnswer<{ requests?: { token: string; answer?: string; delivered?: string }[] }>(harness, {
      tool: 'thread_status',
      cwd: callerPath,
      timeoutMs: 60_000,
    });
    expect(status.isError, status.text).toBe(false);
    expect(status.value!.requests![0].token).toBe(token);
    expect(status.value!.requests![0].answer).toContain(ANSWER);
    await awaitTurnCompleted(harness, caller);
    expect(countInputs(harness, callerPath, ANSWER)).toBe(0);
    expect(queued()).toEqual([]);

    await harness.open(page);
    await page.getByTestId('thread-row').getByText('Weekly planner', { exact: true }).click();
    await shoot(page, 'codex-archive-takes-back-a-queued-answer.png', page.getByText('Read the notes.').last());
  } finally {
    await harness.close();
  }
});

// A person's Stop drops the thread's whole queue. An answer it dropped goes
// back to its request, owed and disarmed: nothing delivers it again or
// starts the session for it, and thread_status returns it. A wake that
// reaches a running session leaves the queue as it is sent, so the answer
// is held in the queue behind a start that failed.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a Stop gives a dropped answer back to its request`, async ({ page }) => {
    test.setTimeout(150_000);
    const ANSWER = 'The release notes are ready: two fixes and a new export.';
    const harness = await launchHarness();
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [
          { name: 'wakes-stop-answer-caller', repo: {}, threads: [{ title: 'Weekly planner', provider }] },
          { name: 'wakes-stop-answer-target', repo: {}, threads: [{ title: 'Release notes', provider: 'claude' }] },
        ],
      });
      const callerPath = seed.projects[0].path;
      const caller = seed.projects[0].threadIds[0];
      const targetPath = seed.projects[1].path;
      const target = seed.projects[1].threadIds[0];
      await setScenario(
        harness,
        targetPath,
        threadToolsScenario({
          name: 'wakes-stop-answer-target',
          provider: 'claude',
          turns: [
            {
              steps: [
                { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
                { gate: 'answer' },
                { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: ANSWER } } },
              ],
              text: 'Replied.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await setScenario(
        harness,
        callerPath,
        threadToolsScenario({
          name: `wakes-stop-answer-caller-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    tool: 'thread_send',
                    args: { thread_id: target, message: 'Draft the release notes.', wait_seconds: 0, notify: true },
                  },
                },
              ],
              text: 'Asked for the notes.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StartSession', caller);
      await harness.rpc('SendMessage', caller, 'ask for the release notes', null);
      const sent = await awaitToolAnswer<{ token: string; outcome: string; notify: boolean }>(harness, {
        tool: 'thread_send',
        cwd: callerPath,
        timeoutMs: 60_000,
      });
      expect(sent.isError, sent.text).toBe(false);
      expect(sent.value!.outcome).toBe('backgrounded');
      expect(sent.value!.notify).toBe(true);
      const token = sent.value!.token;
      await awaitTurnCompleted(harness, caller);
      const responder = await awaitGate(harness, 'answer', targetPath);

      const db = () => new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
      const queued = () => {
        const conn = db();
        try {
          return (
            conn.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ? ORDER BY enqueued_at, rowid`).all(caller) as {
              send_id: string;
            }[]
          ).map((row) => row.send_id);
        } finally {
          conn.close();
        }
      };
      const request = () => {
        const conn = db();
        try {
          return conn
            .prepare(`SELECT delivered_at IS NOT NULL AS delivered, delivered_how AS how, notify FROM thread_requests WHERE token = ?`)
            .get(token) as { delivered: number; how: string; notify: number };
        } finally {
          conn.close();
        }
      };

      // The answer arrives while the caller's start fails: its wake is held
      // in the queue.
      await harness.rpc('StopSession', caller);
      const b = startBreaker(harness, provider, callerPath);
      breaker = b;
      await b.brk();
      await advanceGate(harness, responder.mockId, 'answer');
      await harness.waitForEvent<NotificationSend>(
        'notification:send',
        (data) => data.id === `session-start:${caller}` && !data.retract,
        60_000,
      );
      await expect.poll(queued, { timeout: 15_000 }).toEqual([`thread-wake:${token}`]);
      expect(request()).toEqual({ delivered: 1, how: 'queued', notify: 1 });

      // Mended first, so a start the Stop made would deliver the wake to the
      // agent, and the agent then reads the answer with thread_status. The
      // mock numbers tool calls per process, so this one names its own id
      // rather than take the first process's thread_send row.
      await b.mend();
      await setScenario(
        harness,
        callerPath,
        threadToolsScenario({
          name: `wakes-stop-answer-mended-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [{ call: { tool: 'thread_status', args: { tokens: [token] }, toolUseId: 'status-after-stop' } }],
              text: 'Read the notes.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StopSession', caller);
      const owedAndDisarmed = () => ({ queued: queued(), request: request() });
      const want = { queued: [], request: { delivered: 0, how: '', notify: 0 } };
      expect(owedAndDisarmed()).toEqual(want);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(owedAndDisarmed()).toEqual(want);
      expect((await harness.rpc<unknown[] | null>('GetQueueState', caller)) ?? []).toEqual([]);
      expect(countInputs(harness, callerPath, ANSWER)).toBe(0);

      await harness.rpc('StartSession', caller);
      await harness.rpc('SendMessage', caller, 'did the notes come back?', null);
      const status = await awaitToolAnswer<{ requests?: { token: string; answer?: string }[] }>(harness, {
        tool: 'thread_status',
        cwd: callerPath,
        timeoutMs: 60_000,
      });
      expect(status.isError, status.text).toBe(false);
      expect(status.value!.requests![0].token).toBe(token);
      expect(status.value!.requests![0].answer).toContain(ANSWER);
      await awaitTurnCompleted(harness, caller);
      expect(countInputs(harness, callerPath, ANSWER)).toBe(0);
      expect(queued()).toEqual([]);

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Weekly planner', { exact: true }).click();
      await shoot(page, `${provider}-stop-returns-a-dropped-answer.png`, page.getByText('Read the notes.').last());
    } finally {
      await breaker?.cleanup();
      await harness.close();
    }
  });
}

// A remote job's completion a Stop dropped goes back to its watch, and the
// watcher delivers it again and starts the session for it, as it does for a
// completion that finishes after the Stop. The completion is held in the
// queue behind a start that failed.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a Stop gives a dropped remote completion back to its watch, which delivers it again`, async ({ page }) => {
    test.setTimeout(180_000);
    const [harness, remote] = await startTogether(launchHarness(), launchHarness());
    let breaker: ReturnType<typeof startBreaker> | undefined;
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const computer = await pairBuildBox(harness, remote);
      const remoteSeed = await remote.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'remote-stop-build', repo: {} }] });
      const { projectId: remoteProject, path: remotePath } = remoteSeed.projects[0];
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: 'remote-stop', repo: {}, threads: [{ title: 'Remote builder', provider }] }],
      });
      const { path: cwd, threadIds } = seed.projects[0];
      const thread = threadIds[0];
      const requestId = randomUUID();
      const release = join(remotePath, 'build.release');
      await setScenario(
        harness,
        cwd,
        threadToolsScenario({
          name: `remote-stop-${provider}`,
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
          return (db.prepare(`SELECT notification FROM remote_watches WHERE request_id = ?`).get(requestId) as { notification: string })
            .notification;
        } finally {
          db.close();
        }
      };
      const completion = `request_id: ${requestId}`;

      // The job finishes while the thread's start fails: its completion is
      // held in the queue.
      await harness.rpc('StopSession', thread);
      const b = startBreaker(harness, provider, cwd);
      breaker = b;
      await b.brk();
      await writeFile(release, '');
      await harness.waitForEvent<NotificationSend>(
        'notification:send',
        (data) => data.id === `session-start:${thread}` && !data.retract,
        60_000,
      );
      await expect.poll(queued, { timeout: 15_000 }).toEqual(['remote-completion']);
      expect(watch()).toBe('queued');

      // Mended first, so the start the watcher makes delivers the completion.
      await b.mend();
      await setScenario(
        harness,
        cwd,
        threadToolsScenario({
          name: `remote-stop-mended-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [{ text: 'Build 812 is green.' }],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('StopSession', thread);
      // Back at its watch, or already queued again by the watcher's next
      // pass; never queued with nothing left to deliver it.
      const watched = watch();
      const rows = queued();
      const inputs = countInputs(harness, cwd, completion);
      const truthful =
        (watched === 'pending' && rows.length === 0) || (watched === 'queued' && (rows.length === 1 || inputs === 1));
      expect(truthful, `after the Stop: watch ${watched}, queue ${JSON.stringify(rows)}, ${inputs} deliveries`).toBe(true);

      // The watcher delivers it again and starts the session for it, once.
      const input = await wakeInput(harness, cwd, completion, 30_000);
      expect(input.report.input).toContain('Remote job finished on Build box: nightly build');
      expect(input.report.input).toContain('build 812 finished: green');
      await awaitTurnCompleted(harness, thread);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(countInputs(harness, cwd, completion)).toBe(1);
      expect(queued()).toEqual([]);
      expect(watch()).toBe('queued');

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Remote builder', { exact: true }).click();
      await shoot(page, `${provider}-stop-returns-a-dropped-remote-completion.png`, completionRows(page).last());
    } finally {
      await breaker?.cleanup();
      await Promise.allSettled([harness.close(), remote.close()]);
    }
  });
}

/**
 * Pairs `remote` as Build box, starts a build and a soak test on it from a
 * thread, and archives the thread just after the build finishes there,
 * before the watcher's next check reads the finish: the build's job is
 * released right after a check, and `harness` was launched with
 * WATCHER_PAUSED, so the next check is minutes away. The archive cancels
 * the soak test and keeps the build's result, read from the cancel's
 * reply, for the unarchive.
 */
/** Harness timing whose remote-job watcher checks a running job every two minutes. */
const WATCHER_PAUSED = { env: { AO_HARNESS_TIMING: `${HARNESS_TIMING},remote-watch-poll=120s` } };

async function archiveJustAfterARemoteFinish(provider: ProviderName, harness: HarnessApp, remote: HarnessApp, title: string) {
  await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
  const computer = await pairBuildBox(harness, remote);
  const remoteSeed = await remote.rpc<SeedResult>('HarnessSeed', { projects: [{ name: `remote-finish-${provider}`, repo: {} }] });
  const { projectId: remoteProject, path: remotePath } = remoteSeed.projects[0];
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `archive-finish-${provider}`, repo: {}, threads: [{ title, provider }] }],
  });
  const { path: cwd, threadIds } = seed.projects[0];
  const thread = threadIds[0];
  const build = randomUUID();
  const soak = randomUUID();
  const job = (requestId: string, label: string, release: string, says: string) => ({
    call: {
      server: 'ao-remote-tools',
      tool: 'remote_run',
      args: {
        computer_id: computer,
        project_id: remoteProject,
        request_id: requestId,
        label,
        argv: ['sh', '-c', `while [ ! -f '${join(remotePath, release)}' ]; do sleep 0.1; done; echo '${says}'`],
        wait_seconds: 0,
      },
    },
  });
  await setScenario(
    harness,
    cwd,
    threadToolsScenario({
      name: `archive-finish-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [
        {
          steps: [job(build, 'nightly build', 'build.release', 'build 812 finished: green'), job(soak, 'soak test', 'soak.release', 'soak passed')],
          text: 'The build and the soak test run on Build box.',
        },
      ],
      afterTurns: 'silent',
    }),
  );
  await harness.rpc('StartSession', thread);
  await harness.rpc('SendMessage', thread, 'run the nightly build and the soak test on the build box', null);
  await awaitTurnCompleted(harness, thread);

  const read = <T>(root: string, sql: string, ...params: string[]) => {
    const db = new DatabaseSync(join(root, 'agent-overflow.db'), { readOnly: true });
    try {
      return db.prepare(sql).get(...params) as T;
    } finally {
      db.close();
    }
  };
  const watch = (requestId: string) =>
    read<{ notification: string; state: string; next_check: number }>(
      harness.bootstrap.dataDir,
      `SELECT notification, coalesce(json_extract(receipt,'$.state'),'') AS state, next_check FROM remote_watches WHERE request_id = ?`,
      requestId,
    );
  const destinationState = (requestId: string) =>
    read<{ state: string } | undefined>(remote.bootstrap.dataDir, `SELECT state FROM remote_jobs WHERE id = ?`, requestId)?.state;
  const queued = () => {
    const db = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
    try {
      return (db.prepare(`SELECT send_id FROM flush_queue_items WHERE thread_id = ?`).all(thread) as { send_id: string }[]).map(
        (row) => row.send_id.split(':')[0],
      );
    } finally {
      db.close();
    }
  };
  expect(watch(build).notification).toBe('pending');
  expect(watch(soak).notification).toBe('pending');

  // Released just after a check, the build finishes there and the archive
  // lands while this computer still reads it as running.
  const checked = watch(build).next_check;
  await expect.poll(() => watch(build).next_check, { timeout: 15_000, intervals: [50] }).not.toBe(checked);
  await writeFile(join(remotePath, 'build.release'), '');
  await expect.poll(() => destinationState(build), { timeout: 30_000, intervals: [50] }).toBe('succeeded');
  const observed = watch(build);
  expect(observed, 'the archive lands before the watcher reads the finish').toMatchObject({ notification: 'pending', state: 'running' });
  await harness.rpc('ArchiveThread', thread);

  expect(watch(build)).toMatchObject({ notification: 'pending', state: 'succeeded' });
  expect(watch(soak).notification).toBe('dismissed');
  await expect.poll(() => destinationState(soak), { timeout: 15_000 }).toBe('canceled');

  // The watcher leaves an archived thread's result where it is.
  await new Promise((resolve) => setTimeout(resolve, 6_000));
  expect(watch(build).notification).toBe('pending');
  expect(queued()).toEqual([]);
  expect(countInputs(harness, cwd, `request_id: ${build}`)).toBe(0);
  return { computer, thread, cwd, build, soak, watch, queued };
}

// A job that finished before the watcher read it is a result the thread
// keeps: archiving cancels only the job still running, and the finished
// one's completion arrives once after the unarchive.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: archiving just after a remote job finishes keeps its result for the unarchive`, async ({ page }) => {
    test.setTimeout(180_000);
    const [harness, remote] = await startTogether(launchHarness(WATCHER_PAUSED), launchHarness());
    try {
      const { thread, cwd, build, soak, watch, queued } = await archiveJustAfterARemoteFinish(provider, harness, remote, 'Nightly checks');
      await setScenario(
        harness,
        cwd,
        threadToolsScenario({
          name: `archive-finish-mended-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [{ text: 'Build 812 is green.' }],
          afterTurns: 'silent',
        }),
      );
      await harness.rpc('UnarchiveThread', thread);
      const input = await wakeInput(harness, cwd, `request_id: ${build}`);
      expect(input.report.input).toContain('Remote job finished on Build box: nightly build');
      expect(input.report.input).toContain('build 812 finished: green');
      await awaitTurnCompleted(harness, thread);
      await new Promise((resolve) => setTimeout(resolve, 4_000));
      expect(countInputs(harness, cwd, `request_id: ${build}`)).toBe(1);
      expect(countInputs(harness, cwd, `request_id: ${soak}`)).toBe(0);
      expect(queued()).toEqual([]);
      expect(watch(build).notification).toBe('queued');

      await harness.open(page);
      await page.getByTestId('thread-row').getByText('Nightly checks', { exact: true }).click();
      await shoot(page, `${provider}-archive-keeps-finished-remote-job.png`, completionRows(page).last());
    } finally {
      await Promise.allSettled([harness.close(), remote.close()]);
    }
  });
}

// A finished job's result held for an archived thread does not block
// forgetting its computer, which says what it discards and then discards
// it. Copying the thread is refused with the cause.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: forgetting a computer discards the result held for an archived thread, and copy names it`, async ({ page }) => {
    test.setTimeout(180_000);
    const [harness, remote] = await startTogether(launchHarness(WATCHER_PAUSED), launchHarness());
    try {
      const { computer, thread, cwd, build, watch, queued } = await archiveJustAfterARemoteFinish(provider, harness, remote, 'Release notes');

      const copy = await harness
        .rpc<{ operationId: string }>('BeginThreadTransfer', thread, randomUUID(), computer, 'copy', false)
        .then((intent) => `admitted ${intent.operationId}`, (error: Error) => error.message);
      expect(copy).toContain('"Release notes" is archived and holds a finished remote job; unarchive it to receive the result first.');

      await harness.open(page);
      await page.getByRole('button', { name: 'Settings', exact: true }).click();
      await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
      const row = page.getByTestId('attached-system').filter({ hasText: 'Build box' });
      await row.getByRole('button', { name: 'Remove', exact: true }).click();
      await row.getByRole('button', { name: 'Confirm remove', exact: true }).click();
      const toast = page
        .getByTestId('toast')
        .filter({ hasText: '1 finished job held for an archived conversation will be discarded. Confirm to forget the computer anyway.' });
      await expect(toast).toBeVisible();
      const forget = row.getByRole('button', { name: 'Forget anyway', exact: true });
      await expect(forget).toBeVisible();
      expect(await harness.rpc<Array<{ id: string }>>('ListBackends')).toEqual([expect.objectContaining({ id: computer })]);
      expect(watch(build).notification).toBe('pending');
      await shoot(page, `${provider}-forget-names-held-remote-result.png`, toast, forget);

      await forget.click();
      await expect(page.getByTestId('attached-system')).toHaveCount(0);
      expect(await harness.rpc<Array<{ id: string }>>('ListBackends')).toHaveLength(0);
      expect(watch(build).notification).toBe('dismissed');

      await harness.rpc('UnarchiveThread', thread);
      await new Promise((resolve) => setTimeout(resolve, 6_000));
      expect(countInputs(harness, cwd, `request_id: ${build}`)).toBe(0);
      expect(queued()).toEqual([]);
    } finally {
      await Promise.allSettled([harness.close(), remote.close()]);
    }
  });
}
