// One Assistants state per computer (docs/specs/standing-agents.md,
// "Turning assistants off"), with real backends each holding its own
// assistant. A window paired with a second computer shows and runs each
// computer's assistant by that computer's setting: with this computer's
// off and the other's on, the other's assistant is listed and its loop
// fires while this one's is hidden and does not fire. The Settings page
// turns the setting of the computer its picker shows. A standalone frontend
// attached to two computers does the same and has no switch of its own.
// With this computer's assistants off, the approval of an assistant's worker
// on the other computer stays the assistant's: the person there is not
// asked, the assistant is woken for it once assistants are on, and the
// person there can answer it in the worker throughout
// (docs/specs/standing-agents.md, "Owned workers").
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { launchHarness, type HarnessApp, type HarnessMockEventData } from '../src/harness.js';
import { launchFrontendClient } from './frontend-client-helpers.js';
import { connectedAfterConfirm, pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { assistantRow, assistantsSection } from './sidebar-sections-helpers.js';
import { advanceGate, awaitGate, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { dbWake, registerThrough, seedWakeAgent, wakeInput, type WakeAgent } from './wake-rows-helpers.js';

const LOOP_FIRED = 'Loop "beat" fired.';
const EVERY_SECONDS = 3;

/** Turns the Assistants setting of the computer picked at the top of its Settings page. */
async function setAssistants(page: Page, computer: string, on: boolean): Promise<void> {
  await page.getByTestId('sidebar-settings-button').click();
  const allSettings = page.getByRole('button', { name: 'All settings', exact: true });
  if (await allSettings.isVisible()) await allSettings.click();
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Assistants', exact: true }).click();
  await page.getByRole('combobox', { name: 'Computer', exact: true }).selectOption(computer);
  const toggle = page.getByTestId('settings-assistants').getByRole('switch', { name: 'Toggle assistants' });
  await expect(toggle).toBeEnabled();
  await expect(toggle).toHaveAttribute('aria-checked', on ? 'false' : 'true');
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', on ? 'true' : 'false');
  await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
  await expect(page.getByTestId('settings-overlay')).toHaveCount(0);
}

async function assistantsEnabled(harness: HarnessApp): Promise<boolean> {
  return (await harness.rpc<{ assistantsEnabled: boolean }>('GetSettings')).assistantsEnabled;
}

function fires(harness: HarnessApp, agent: WakeAgent): number {
  return harness.countEvents<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === agent.home && (ev.report.input ?? '').includes(LOOP_FIRED),
  );
}

/** How long the loop's next fire has been due, in milliseconds. */
function overdue(agent: WakeAgent): number {
  const wake = dbWake(agent.dataDir, agent.thread, 'beat');
  return wake?.next_at ? Date.now() - wake.next_at : 0;
}

async function withLoop(harness: HarnessApp, tag: string, title: string): Promise<WakeAgent> {
  const agent = await seedWakeAgent(harness, 'claude', tag, title);
  await registerThrough(harness, 'claude', agent, tag, [
    { tool: 'thread_loop', args: { label: 'beat', note: 'check in', every_seconds: EVERY_SECONDS } },
  ]);
  return agent;
}

test('a window shows and runs each computer\'s assistants by that computer\'s setting', async ({ page }) => {
  test.setTimeout(240_000);
  const [here, there] = await startTogether(launchHarness(), launchHarness());
  try {
    const thereID = await pairOwnDevice(here, there, 'GPU computer');
    const mine = await withLoop(here, 'paired-toggle-here', 'Here agent');
    const theirs = await withLoop(there, 'paired-toggle-there', 'There agent');
    await here.open(page);
    await expect(assistantRow(page, mine.thread)).toBeVisible();
    await connectedAfterConfirm({ here, there }, (wait) => expect(assistantRow(page, theirs.thread)).toBeVisible(wait));

    // This computer off, the other on: only the other's assistant shows,
    // and only its loop fires.
    await setAssistants(page, '', false);
    expect(await assistantsEnabled(here)).toBe(false);
    expect(await assistantsEnabled(there)).toBe(true);
    await expect(page.locator(`[data-sidebar-thread-id="${mine.thread}"]`)).toHaveCount(0);
    await expect(assistantRow(page, theirs.thread)).toBeVisible();
    const theirFires = fires(there, theirs);
    await expect.poll(() => fires(there, theirs), { timeout: 30_000 }).toBeGreaterThan(theirFires);
    await expect.poll(() => overdue(mine), { timeout: 30_000 }).toBeGreaterThan(EVERY_SECONDS * 1000);
    const myFires = fires(here, mine);
    await expect.poll(() => overdue(mine), { timeout: 30_000 }).toBeGreaterThan(3 * EVERY_SECONDS * 1000);
    expect(fires(here, mine)).toBe(myFires);

    // The other computer off from this window's Settings: neither shows.
    await setAssistants(page, thereID, false);
    expect(await assistantsEnabled(there)).toBe(false);
    await expect(assistantsSection(page)).toHaveCount(0);
    await expect(page.locator(`[data-sidebar-thread-id="${theirs.thread}"]`)).toHaveCount(0);

    // This computer on again: its assistant is back, the other's stays hidden.
    await setAssistants(page, '', true);
    expect(await assistantsEnabled(there)).toBe(false);
    await expect(assistantRow(page, mine.thread)).toBeVisible();
    await expect(page.locator(`[data-sidebar-thread-id="${theirs.thread}"]`)).toHaveCount(0);
  } finally {
    await page.close();
    await here.close();
    await there.close();
  }
});

test('a standalone frontend turns the setting of the computer it picks and has none of its own', async ({ page }) => {
  test.setTimeout(150_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-assistants-frontend-'));
  let desktop: HarnessApp | undefined;
  let first: HarnessApp | undefined;
  let second: HarnessApp | undefined;
  let frontend: Awaited<ReturnType<typeof launchFrontendClient>> | undefined;
  try {
    [desktop, first, second] = await startTogether(
      launchHarness({ dataDir: join(root, 'desktop') }), launchHarness(), launchHarness(),
    );
    const firstID = await pairOwnDevice(desktop, first, 'First computer');
    const secondID = await pairOwnDevice(desktop, second, 'Second computer');
    const firstAgent = await seedWakeAgent(first, 'claude', 'frontend-toggle-first', 'First agent');
    const secondAgent = await seedWakeAgent(second, 'claude', 'frontend-toggle-second', 'Second agent');
    // As `--connect <computer>` does once the desktop window has closed.
    const profiles = join(desktop.bootstrap.dataDir, 'device');
    await desktop.close();
    desktop = undefined;
    frontend = await launchFrontendClient(profiles, join(root, 'frontend'), firstID);
    await frontend.open(page);
    const sides = { frontend, first, second };
    await connectedAfterConfirm(sides, (wait) => expect(assistantRow(page, firstAgent.thread)).toBeVisible(wait));
    await connectedAfterConfirm(sides, (wait) => expect(assistantRow(page, secondAgent.thread)).toBeVisible(wait));

    // The picker offers the two computers and not the frontend itself.
    await page.getByTestId('sidebar-settings-button').click();
    const allSettings = page.getByRole('button', { name: 'All settings', exact: true });
    if (await allSettings.isVisible()) await allSettings.click();
    await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Assistants', exact: true }).click();
    const options = await page.getByRole('combobox', { name: 'Computer', exact: true }).locator('option')
      .evaluateAll((els) => els.map((el) => (el as HTMLOptionElement).value).sort());
    expect(options).toEqual([firstID, secondID].sort());
    await page.getByRole('button', { name: 'Close Settings', exact: true }).click();

    await setAssistants(page, secondID, false);
    expect(await assistantsEnabled(second)).toBe(false);
    expect(await assistantsEnabled(first)).toBe(true);
    await expect(page.locator(`[data-sidebar-thread-id="${secondAgent.thread}"]`)).toHaveCount(0);
    await expect(assistantRow(page, firstAgent.thread)).toBeVisible();

    await setAssistants(page, secondID, true);
    await expect(assistantRow(page, secondAgent.thread)).toBeVisible();
  } finally {
    try {
      await page.close();
      await frontend?.close();
      await desktop?.close();
      await first?.close();
      await second?.close();
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }
});

test('with this computer\'s assistants off, an assistant\'s worker on the other computer waits for the assistant, which takes its approval once assistants are on', async () => {
  test.setTimeout(240_000);
  const [here, there] = await startTogether(launchHarness(), launchHarness());
  try {
    const thereID = await pairOwnDevice(here, there, 'GPU computer');
    const work = (await there.rpc<{ projects: Array<{ projectId: string; path: string }> }>('HarnessSeed', {
      projects: [{ name: 'paused-owner-work', repo: {}, threads: [] }],
    })).projects[0];
    await setScenario(there, work.path, threadToolsScenario({
      name: 'paused-owner-worker', provider: 'claude',
      turns: [{ steps: [
        { gate: 'paused-owner-hold' },
        { approval: { toolName: 'Bash', input: { command: 'make soak' }, onAllow: [], onDeny: [] } },
      ], text: 'Soaked.' }],
      afterTurns: 'repeatLast',
    }));
    const assistant = await seedWakeAgent(here, 'claude', 'paused-owner', 'Paused owner');
    const [spawn] = await registerThrough(here, 'claude', assistant, 'paused-owner', [{
      tool: 'thread_spawn',
      args: { prompt: 'Run the soak.', title: 'Paused owner worker', computer_id: thereID, project_id: work.projectId, notify: true },
    }]);
    const worker = String(spawn.thread_id);
    const held = await awaitGate(there, 'paused-owner-hold', work.path);

    // Assistants off here: the worker's approval stays the assistant's. It
    // waits in the assistant's queue, the person there is not asked, and
    // the assistant is not woken while the setting is off.
    await here.rpc('UpdateSettings', { assistantsEnabled: false });
    const wokenWhileOff = wakeInput(here, assistant.home, 'Paused owner worker', 20_000).then(() => true, () => false);
    await advanceGate(there, held.mockId, 'paused-owner-hold');
    type Pending = { requestId: string; routedTo?: { threadId: string } | null };
    const approvals = async () => (await there.rpc<{ approvals: Pending[] }>('ListPendingInteractiveRequests', worker)).approvals;
    const holder = async () => {
      const [approval] = await approvals();
      return approval ? (approval.routedTo?.threadId ?? 'the person') : 'none';
    };
    await expect.poll(holder, { timeout: 30_000 }).toBe(assistant.thread);
    expect(await wokenWhileOff, 'the paused assistant was woken for its worker\'s approval').toBe(false);
    expect(await holder(), 'the approval left the paused assistant').toBe(assistant.thread);

    // Assistants on: the assistant is woken for the approval that waited.
    const woken = wakeInput(here, assistant.home, 'Paused owner worker', 60_000);
    await here.rpc('UpdateSettings', { assistantsEnabled: true });
    await woken;
    expect(await holder(), 'the approval left the assistant that was woken for it').toBe(assistant.thread);

    // The person there can answer it in the worker.
    const done = awaitTurnCompleted(there, worker);
    await there.rpc('RespondToApproval', worker, { requestId: (await approvals())[0].requestId, decision: 'allow' });
    await done;
  } finally {
    await here.close();
    await there.close();
  }
});
