// What a person sees when a paired computer's answer or the pairing itself
// ends before this computer hears of it: a late reply the other computer
// dropped before it was collected reads so on the caller's Agents row, with
// the first answer kept; a command running there when the person pairs
// with that computer again keeps running and is collected under the new
// pairing, named as that pairing names the computer; and a command running
// there when that computer revokes this one is told to its thread once,
// finished with its outcome unknown, naming that computer as the person
// knew it, and its transcript row says why.
// Specs: docs/specs/agent-thread-tools.md ("Wakes from another computer",
// "Reaching another computer"), docs/architecture/remote-commands.md.
import { randomUUID } from 'node:crypto';
import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp, type HarnessMockEventData } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { solePairedDevice } from './offhost-helpers.js';
import { openAgents, openPast, pastRow } from './agents-tray-helpers.js';
import { awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

test.describe.configure({ mode: 'serial' });

let home: HarnessApp;
let remote: HarnessApp;
let remoteID = '';

test.beforeAll(async () => {
  [home, remote] = await startTogether(launchHarness(), launchHarness());
  const pairing = await headlessPairing(remote);
  try {
    const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
    await pairing.confirm(attachment.verificationNumber);
    remoteID = attachment.id;
  } finally {
    pairing.close();
  }
  await home.rpc('RenameBackend', remoteID, 'GPU computer');
  await enableRemoteCommands();
});

/** Remote commands on the paired computer, which a pairing starts with off. */
async function enableRemoteCommands(): Promise<void> {
  await expect
    .poll(async () => {
      try {
        await home.rpc('SetAgentComputerEnabled', remoteID, true);
        return 'enabled';
      } catch (error) {
        return String(error);
      }
    }, { timeout: 30_000 })
    .toBe('enabled');
}

test.beforeEach(() => {
  home.clearEvents();
  remote.clearEvents();
});

test.afterAll(async () => {
  await home?.close();
  await remote?.close();
});

interface SeededProject {
  projectId: string;
  path: string;
  threadIds: string[];
}

async function seed(app: HarnessApp, name: string, titles: string[]): Promise<SeededProject> {
  const result = await app.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [{ name, repo: {}, threads: titles.map((title) => ({ title, provider: 'claude' })) }],
  });
  return result.projects[0];
}

function storePath(app: HarnessApp): string {
  return join(app.bootstrap.dataDir, 'agent-overflow.db');
}

/** The destination's record of one request: its revision and the one its source took. */
function receipt(app: HarnessApp, token: string): { revision: number; collected: number } | undefined {
  const db = new DatabaseSync(storePath(app), { readOnly: true });
  try {
    return db.prepare('SELECT revision, collected_revision AS collected FROM thread_request_receipts WHERE token = ?')
      .get(token) as { revision: number; collected: number } | undefined;
  } finally {
    db.close();
  }
}

test('a late reply the other computer dropped before it was collected reads as expired on the Agents row', async ({ page }) => {
  test.setTimeout(180_000);
  page.setDefaultTimeout(30_000);
  const caller = await seed(home, 'expired-late-caller', ['Expired late caller']);
  const there = await seed(remote, 'expired-late-target', ['Release notes']);
  await setScenario(remote, there.path, threadToolsScenario({
    name: 'expired-late-target-script', provider: 'claude',
    turns: [{ text: 'The notes are written.' }],
  }));
  await setScenario(home, caller.path, threadToolsScenario({
    name: 'expired-late-caller-script', provider: 'claude',
    turns: [{
      steps: [{ call: {
        tool: 'thread_send',
        args: { thread_id: there.threadIds[0], computer_id: remoteID, message: 'Write the release notes.', wait_seconds: 60, notify: true },
        timeoutMs: 120_000,
      } }],
      text: 'They are written.',
    }],
  }));
  await home.rpc('StartSession', caller.threadIds[0]);
  await home.rpc('SendMessage', caller.threadIds[0], 'have the release notes written over there', null);
  const sent = await awaitToolAnswer<{ token: string; state: string }>(home, { tool: 'thread_send', cwd: caller.path, timeoutMs: 120_000 });
  expect(sent.isError, sent.text).toBe(false);
  expect(sent.value!.state).toBe('finished');
  const token = sent.value!.token;
  await expect.poll(() => receipt(remote, token)?.collected, { timeout: 30_000 }).toBe(1);

  // A late reply arrived there an hour ago, while this computer was away,
  // and its hold ran out before this computer asked again: the late reply's
  // revision, then the retention pass's expiry (ExpireThreadRequestAnswers).
  const arrived = Date.now() - 3_600_000;
  const db = new DatabaseSync(storePath(remote));
  try {
    db.exec('PRAGMA busy_timeout = 5000');
    db.prepare(`UPDATE thread_request_receipts
      SET late_reply = NULL, late_reply_at = ?, revision = revision + 1, answer = NULL, answer_cause = '', answer_cause_arg = '',
          state = 'expired', updated_at = ?
      WHERE token = ?`).run(arrived, Date.now(), token);
  } finally {
    db.close();
  }

  // This computer records it, keeps the first answer, and only then tells
  // that computer it holds the revision.
  // Settled, a send with its wake armed is listed under Past.
  const listed = async () => (await home.rpc<{ rows: { token: string; state: string; answer?: string; lateReplyExpiredAt?: number }[] }>(
    'ListThreadPastAgentRows', caller.threadIds[0], { settledAt: 0, token: '' }, 10)).rows.find((row) => row.token === token);
  await expect.poll(async () => (await listed())?.lateReplyExpiredAt, { timeout: 60_000 }).toBe(arrived);
  expect(await listed()).toMatchObject({ state: 'finished', answer: 'The notes are written.' });
  await expect.poll(() => receipt(remote, token)?.collected, { timeout: 30_000 }).toBe(2);

  await home.open(page);
  await openAgents(page, 'Expired late caller');
  await openPast(page);
  const row = pastRow(page, token);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('finished, late reply expired');
  await row.getByTestId('agent-tray-row-details').click();
  await expect(row.getByTestId('agent-tray-row-digest-answer')).toHaveText('The notes are written.');
  await expect(row.getByTestId('agent-tray-row-digest-late-expired')).toContainText('expired there before it was collected');
  await row.screenshot({ path: test.info().outputPath('late-reply-expired.png'), animations: 'disabled' });
});

test('a command running when the computer is paired again finishes under the new pairing, named as it names the computer', async ({ page }) => {
  test.setTimeout(180_000);
  page.setDefaultTimeout(30_000);
  const builder = await seed(home, 'replaced-builder', ['Replaced builder']);
  const there = await seed(remote, 'replaced-build', []);
  const finish = join(there.path, 'build.release');
  await setScenario(home, builder.path, threadToolsScenario({
    name: 'replaced-builder-script', provider: 'claude',
    turns: [
      { steps: [{ call: {
        server: 'ao-remote-tools',
        tool: 'remote_run',
        args: {
          computer_id: remoteID, project_id: there.projectId, request_id: randomUUID(), label: 'release build',
          argv: ['sh', '-c', `while [ ! -f '${finish}' ]; do sleep 0.1; done`], wait_seconds: 0,
        },
      } }], text: 'The build runs over there.' },
      { text: 'It finished.' },
    ],
    afterTurns: 'silent',
  }));
  await home.rpc('StartSession', builder.threadIds[0]);
  await home.rpc('SendMessage', builder.threadIds[0], 'run the release build over there', null);
  const started = await home.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_run', cwd: builder.path, timeoutMs: 60_000 });
  expect(started.report.isError, started.report.result).toBeFalsy();
  expect(started.report.result).toContain('"backgrounded":true');
  await awaitTurnCompleted(home, builder.threadIds[0]);

  // The person pairs with that computer again. The new pairing carries no
  // nickname, so it is live under that computer's own name.
  const told = home.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === builder.path && (ev.report.input ?? '').includes('Remote job finished on'),
    90_000,
  );
  const pairing = await headlessPairing(remote);
  try {
    const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
    expect(attachment.id).toBe(remoteID);
    await pairing.confirm(attachment.verificationNumber);
  } finally {
    pairing.close();
  }
  const live = (await home.rpc<{ id: string; name: string; nickname?: string }[]>('ListBackends')).find((b) => b.id === remoteID)!;
  expect(live.nickname ?? '').toBe('');
  expect(live.name).not.toBe('GPU computer');

  // The command still runs there; once it finishes, the new pairing
  // collects it and its thread is told, naming the computer as now.
  await writeFile(finish, '');
  const input = (await told).report.input ?? '';
  expect(input).toContain(`Remote job finished on ${live.name}: release build`);
  expect(input).toContain('Result: succeeded; exit code: 0');
  expect(input).not.toContain('GPU computer');

  await home.open(page);
  await page.getByTestId('thread-row-title').getByText('Replaced builder', { exact: true }).click();
  const timeline = page.getByRole('log', { name: 'Message History' });
  await expect(timeline.getByTestId('agent-message-sender').getByText(`release build on ${live.name}`, { exact: true })).toBeVisible();
  await timeline.getByRole('button', { name: /Expand 1 activity row/ }).click();
  const card = timeline.getByTestId('tool-call-card').filter({ hasText: 'build.release' }).first();
  await expect(card.locator('[data-machine]').first()).toHaveText(`(${live.name})`);
  await expect(card.getByTestId('row-error-msg')).toHaveCount(0);
  await card.screenshot({ path: test.info().outputPath('repaired-command-name.png'), animations: 'disabled' });

  // The next case starts under the new pairing, named as before.
  await home.rpc('RenameBackend', remoteID, 'GPU computer');
  await enableRemoteCommands();
});

// Last: the pairing does not survive it.
test('a command running on a computer that revokes this one is told once with its outcome unknown', async ({ page }) => {
  test.setTimeout(180_000);
  page.setDefaultTimeout(30_000);
  const builder = await seed(home, 'revoked-builder', ['Revoked builder']);
  const there = await seed(remote, 'revoked-build', []);
  const finish = join(there.path, 'build.release');
  await setScenario(home, builder.path, threadToolsScenario({
    name: 'revoked-builder-script', provider: 'claude',
    turns: [
      { steps: [{ call: {
        server: 'ao-remote-tools',
        tool: 'remote_run',
        args: {
          computer_id: remoteID, project_id: there.projectId, request_id: randomUUID(), label: 'nightly build',
          argv: ['sh', '-c', `while [ ! -f '${finish}' ]; do sleep 0.1; done`], wait_seconds: 0,
        },
      } }], text: 'The build runs over there.' },
      { text: 'Its outcome is unknown.' },
    ],
    afterTurns: 'silent',
  }));
  await home.rpc('StartSession', builder.threadIds[0]);
  await home.rpc('SendMessage', builder.threadIds[0], 'run the nightly build over there', null);
  const started = await home.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_run', cwd: builder.path, timeoutMs: 60_000 });
  expect(started.report.isError, started.report.result).toBeFalsy();
  expect(started.report.result).toContain('"backgrounded":true');
  await awaitTurnCompleted(home, builder.threadIds[0]);

  // That computer revokes this one with the job still running there.
  const told = home.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === builder.path && (ev.report.input ?? '').includes('Result: unknown'),
    90_000,
  );
  await remote.rpc('RevokeAccessDevice', (await solePairedDevice(remote)).id);
  const input = (await told).report.input ?? '';
  expect(input).toContain('Result: unknown');
  // Named as the person knew it, though no pairing here names it now.
  expect(input).toContain('Remote job finished on GPU computer');
  expect(input).toContain("The pairing with this computer ended, so this command's outcome is unknown.");
  await writeFile(finish, '');

  await home.open(page);
  await page.getByTestId('thread-row-title').getByText('Revoked builder', { exact: true }).click();
  const timeline = page.getByRole('log', { name: 'Message History' });
  // The completion row reads the outcome on the computer as it was named,
  // and the folded call row says why.
  await expect(timeline.getByText(/nightly build on GPU computer.* unknown/)).toBeVisible();
  await timeline.getByRole('button', { name: /Expand 1 activity row/ }).click();
  const card = timeline.getByTestId('tool-call-card').filter({ hasText: 'build.release' }).first();
  await expect(card.getByTestId('row-error-msg')).toContainText("The pairing with this computer ended, so this command's outcome is unknown.");
  await expect(card.locator('[data-machine]').first()).toHaveText('(GPU computer)');
  await card.screenshot({ path: test.info().outputPath('command-outcome-unknown.png'), animations: 'disabled' });
});
