// Account auto-switch (docs/specs/account-auto-switch.md) through the
// shipped Settings page, the composer and the real backend: two Claude
// accounts signed in through the sign-in flow, the pool changed on their
// cards, the global settings changed in the Accounts section, and usage
// readings served by the isolated usage endpoint (HarnessSetClaudeUsage for
// the selected account, the card's refresh button for the other).
//
// Covered: the pool default, its toggle and hint; a threshold switch before
// a send; a reading-driven switch while a turn is in flight; a model-scoped
// limit that moves only a thread of that model; the card-order return to the
// first card and the manual pick that stops it; an account out of the pool
// left alone; the "Switch at" control and its range; a usage-limit refusal
// with "Continue threads after a usage limit" on and off. Each asserts the
// toast, the Active card, the thread notice and the backend listing.
//
// Owns its backend: it adopts provider accounts and keeps refusal marks in
// memory, process-wide state no other spec should inherit. The cases are one
// sequence on one machine; each pins the selection it starts from by
// switching on the card, which also rules out an order switch outside the
// case that tests one.
//
// AO_AUTO_SWITCH_SCREENSHOTS=<dir> saves the Accounts section with one card
// unticked and the refused thread after its continue, for review.

import { readFileSync } from 'node:fs';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import type { HarnessMockEventData } from '../src/harness-app.js';
import {
  RESULT_LINE,
  claudeTurnsScenario,
  emit,
  listItems,
  textLines,
  type ScenarioStep,
} from './agent-visibility-helpers.js';
import type { SeedResult } from './fixtures.js';
import {
  claudeAccountId,
  claudeAccounts,
  claudeAccountsSection,
  completeClaudeSignIn,
  openClaudeAccounts,
  seedNativeClaudeLogin,
} from './provider-account-helpers.js';

const A = 'pool-a@agent-overflow.test';
const B = 'pool-b@agent-overflow.test';
const CONTINUE_TEXT = 'Continue. The usage limit that stopped this thread no longer applies.';
const SCRATCH = process.env.AO_AUTO_SWITCH_SCREENSHOTS;

// The library's usage-limit refusal, so this spec replays the same wire
// lines the scenario library pins.
const here = path.dirname(fileURLToPath(import.meta.url));
const refusalSteps: ScenarioStep[] = JSON.parse(
  readFileSync(path.join(here, '../../internal/harness/scenario/library/usage-limit-claude.json'), 'utf8'),
).turns[0].steps;

interface Limit {
  kind: string;
  group: string;
  percent: number;
  resets_at?: string;
  model?: string;
}

const hoursAhead = (h: number) => new Date(Date.now() + h * 3600_000).toISOString();
const session = (percent: number): Limit => ({ kind: 'session', group: 'session', percent, resets_at: hoursAhead(3) });
const weeklyAll = (percent: number): Limit => ({ kind: 'weekly_all', group: 'weekly', percent, resets_at: hoursAhead(96) });

interface ThreadSpec {
  key: string;
  title: string;
  model: string;
}

const THREADS: ThreadSpec[] = [
  { key: 'threshold', title: 'Threshold send', model: 'claude-opus-5' },
  { key: 'midturn', title: 'Mid-turn reading', model: 'claude-opus-5' },
  { key: 'opus', title: 'Opus under a Fable limit', model: 'claude-opus-5' },
  { key: 'fable', title: 'Fable under a Fable limit', model: 'claude-fable-5' },
  { key: 'order', title: 'Card order', model: 'claude-opus-5' },
  { key: 'outside', title: 'Outside the pool', model: 'claude-opus-5' },
  { key: 'setting', title: 'Switch at setting', model: 'claude-opus-5' },
  { key: 'continue', title: 'Refused with continue', model: 'claude-opus-5' },
  { key: 'stopped', title: 'Refused without continue', model: 'claude-sonnet-5' },
];

interface SeededThread extends ThreadSpec {
  id: string;
  cwd: string;
}

interface SwitchEvent {
  provider: string;
  fromAccountId?: string;
  toAccountId?: string;
  fromLabel?: string;
  toLabel?: string;
  reason?: string;
  limitName?: string;
  usedPercent?: number;
  error?: string;
}

let harness: HarnessApp;
let settingsPage: Page;
let chatPage: Page;
const threads = new Map<string, SeededThread>();
const consoleErrors: string[] = [];

const thread = (key: string) => threads.get(key)!;
const section = () => claudeAccountsSection(settingsPage);
const idOf = (email: string) => claudeAccountId(harness, email);
const switchToasts = () => chatPage.getByTestId('toast').filter({ hasText: 'Switched Claude account' });
const switchEvents = () => harness.countEvents('provider:account_auto_switch');

async function selectedEmail(): Promise<string | undefined> {
  return (await claudeAccounts(harness)).find((account) => account.active)?.email;
}

async function autoSwitchOf(email: string): Promise<boolean | undefined> {
  const accounts = await harness.rpc<Array<{ email?: string; provider: string; autoSwitch?: boolean }>>('ListProviderAccounts');
  return accounts.find((account) => account.provider === 'claude' && account.email === email)?.autoSwitch;
}

async function percentOf(accountId: string, limitId: string): Promise<number | undefined> {
  const snapshots = await harness.rpc<Array<{ provider: string; accountId?: string; limits: Array<{ limitId: string; usedPercent: number }> }>>(
    'GetRateLimitsSnapshots',
  );
  const snapshot = snapshots.find((s) => s.provider === 'claude' && s.accountId === accountId);
  return snapshot?.limits.find((limit) => limit.limitId === limitId)?.usedPercent;
}

/** A person picks the selected account on its card; that pins it. */
async function selectOnCard(email: string): Promise<void> {
  if ((await selectedEmail()) === email) return;
  const id = await idOf(email);
  await section().getByTestId(`provider-account-${id}`).getByRole('button', { name: `Switch to ${email}` }).click();
  await expect.poll(selectedEmail).toBe(email);
  await expect(section().getByTestId(`provider-account-${id}`)).toContainText('Active');
}

/**
 * Serves `limits` for `email` and waits for the backend to cache them. The
 * selected account is read by the harness call itself; another account is
 * read the way a person would, with its card's refresh button.
 */
async function setReading(email: string, limits: Limit[], probe: { limitId: string; percent: number }): Promise<void> {
  await harness.rpc('HarnessSetClaudeUsage', { email, limits });
  const id = await idOf(email);
  if ((await selectedEmail()) !== email) {
    const refresh = section().getByRole('button', { name: `Refresh usage for ${email}` });
    await expect(refresh).toBeEnabled();
    await refresh.click();
  }
  await expect.poll(() => percentOf(id, probe.limitId), { timeout: 15_000 }).toBe(probe.percent);
}

function setScenario(key: string, turns: ScenarioStep[][]): Promise<unknown> {
  return harness.rpc('HarnessSetScenario', {
    cwd: thread(key).cwd,
    scenario: claudeTurnsScenario(`auto-switch-${key}`, turns),
  });
}

function reply(key: string, n: number, text: string): ScenarioStep[] {
  return [emit([...textLines(`msg-${key}-${n}`, text), RESULT_LINE])];
}

/**
 * The one switch toast on screen says `text`; it is then dismissed, so it
 * cannot cover the composer and the next case sees only its own.
 */
async function expectSwitchToast(text: string): Promise<void> {
  await expect(switchToasts()).toHaveText([text]);
  await switchToasts().getByRole('button', { name: 'Dismiss Notification' }).click();
  await expect(switchToasts()).toHaveCount(0);
}

async function openThread(key: string): Promise<void> {
  await chatPage.getByTestId('thread-row').filter({ hasText: thread(key).title }).click();
  await expect(chatPage.getByLabel('Message Input')).toBeVisible();
}

async function sendFromComposer(text: string): Promise<void> {
  await chatPage.getByLabel('Message Input').fill(text);
  await chatPage.getByTestId('composer-send').click();
}

function turnStarted(key: string): Promise<unknown> {
  return harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === thread(key).id, 30_000);
}

function turnCompleted(key: string): Promise<unknown> {
  return harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === thread(key).id, 30_000);
}

function mockInputs(key: string): string[] {
  const scenario = `auto-switch-${key}`;
  const inputs: string[] = [];
  harness.countEvents<HarnessMockEventData>('harness:mock', (ev) => {
    if (ev.scenario === scenario && ev.report.kind === 'user_input') inputs.push(ev.report.input ?? '');
    return false;
  });
  return inputs;
}

async function noticeItems(key: string): Promise<string[]> {
  const items = await listItems(harness, thread(key).id);
  return items
    .filter((item) => item.kind === 'notification' && /"account_auto_switch"/.test(item.meta ?? ''))
    .map((item) => item.summary);
}

const timeline = () => chatPage.getByTestId('message-timeline-scroll');

test.describe.serial('Claude account auto-switch', () => {
  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();
    await seedNativeClaudeLogin(harness, A, Date.now() + 20 * 24 * 3600_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: THREADS.map((spec) => ({
        name: `auto-switch-${spec.key}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
        threads: [{
          title: spec.title,
          provider: 'claude',
          model: spec.model,
          turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
        }],
      })),
    });
    THREADS.forEach((spec, i) => {
      threads.set(spec.key, { ...spec, id: seed.projects[i].threadIds[0], cwd: seed.projects[i].path });
    });

    settingsPage = await browser.newPage();
    chatPage = await browser.newPage();
    for (const page of [settingsPage, chatPage]) {
      page.on('console', (message) => {
        if (message.type() === 'error') consoleErrors.push(message.text());
      });
    }
    await harness.open(settingsPage);
    await openClaudeAccounts(settingsPage);
    // The first sign-in adopts the native login (A) as the selected account
    // and adds B below it.
    await section().getByRole('button', { name: 'Log in to another account' }).click();
    await completeClaudeSignIn(settingsPage, B);
    await expect.poll(async () => (await claudeAccounts(harness)).map((account) => account.email)).toEqual([A, B]);
    await harness.open(chatPage);
  });

  test.afterAll(async () => {
    await settingsPage?.close();
    await chatPage?.close();
    await harness?.close();
  });

  test('every saved account starts in the pool, and unticking one takes it out across a reload', async () => {
    const aId = await idOf(A);
    const bId = await idOf(B);
    const hint = section().getByTestId('provider-accounts-pool-hint-claude');
    const bBox = () => section().getByTestId(`provider-account-auto-switch-${bId}`);
    await expect(section().getByTestId(`provider-account-auto-switch-${aId}`)).toBeChecked();
    await expect(bBox()).toBeChecked();
    await expect(hint).toHaveCount(0);
    expect(await autoSwitchOf(A)).toBe(true);
    expect(await autoSwitchOf(B)).toBe(true);

    await bBox().uncheck();
    await expect.poll(() => autoSwitchOf(B)).toBe(false);
    expect(await autoSwitchOf(A)).toBe(true);
    await expect(hint).toHaveText('Auto-switch needs two or more accounts ticked to move between them when a usage limit is near.');

    await settingsPage.reload();
    await openClaudeAccounts(settingsPage);
    await expect(bBox()).not.toBeChecked();
    await expect(section().getByTestId(`provider-account-auto-switch-${aId}`)).toBeChecked();
    await expect(hint).toBeVisible();
    expect(await autoSwitchOf(B)).toBe(false);
    if (SCRATCH) {
      await section().scrollIntoViewIfNeeded();
      await section().screenshot({ path: path.join(SCRATCH, 'accounts-one-unticked.png') });
    }

    await bBox().check();
    await expect.poll(() => autoSwitchOf(B)).toBe(true);
    await expect(hint).toHaveCount(0);
  });

  test('a send from an account past the threshold switches to a clear one first', async () => {
    await selectOnCard(B);
    await setReading(A, [session(10), weeklyAll(10)], { limitId: 'session', percent: 10 });
    await setReading(B, [session(97), weeklyAll(40)], { limitId: 'session', percent: 97 });
    // A reading with no turn in flight demands nothing, so nothing moved.
    expect(await selectedEmail()).toBe(B);

    await setScenario('threshold', [reply('threshold', 1, 'Answered on the clear account')]);
    await openThread('threshold');
    const switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    const done = turnCompleted('threshold');
    await sendFromComposer('summarize the repo');

    const event = await switched;
    expect(event).toMatchObject({ provider: 'claude', fromLabel: B, toLabel: A, reason: 'threshold', limitName: 'Current session' });
    await expectSwitchToast(`Switched Claude account to ${A}. ${B} is at 97% of its Current session limit.`);
    expect(await selectedEmail()).toBe(A);
    await expect(section().getByTestId(`provider-account-${await idOf(A)}`)).toContainText('Active');
    await expect(section().getByTestId(`provider-account-${await idOf(B)}`)).not.toContainText('Active');
    const notice = `Switched Claude account from ${B} to ${A}: ${B} is at 97% of its Current session limit.`;
    await expect(timeline().getByTestId('notification-row').filter({ hasText: notice })).toBeVisible();
    expect(await noticeItems('threshold')).toEqual([notice]);

    await done;
    await expect(timeline().getByText('Answered on the clear account')).toBeVisible();
    expect(mockInputs('threshold')).toEqual([expect.stringContaining('summarize the repo')]);
  });

  test('a reading past the threshold moves the selection while a turn is running', async () => {
    await selectOnCard(B);
    await setReading(B, [session(20), weeklyAll(40)], { limitId: 'session', percent: 20 });
    await setScenario('midturn', [[
      emit(textLines('msg-midturn-1', 'Halfway through')),
      { waitSignal: { name: 'release' } },
      emit([...textLines('msg-midturn-2', 'Finished after the switch'), RESULT_LINE]),
    ]]);
    await openThread('midturn');
    const registered = harness.waitForEvent<HarnessMockEventData>(
      'harness:mock', (ev) => ev.scenario === 'auto-switch-midturn' && ev.report.kind === 'registered', 30_000);
    const gate = harness.waitForEvent<HarnessMockEventData>(
      'harness:mock', (ev) => ev.scenario === 'auto-switch-midturn' && ev.report.kind === 'waiting_signal', 30_000);
    const started = turnStarted('midturn');
    await sendFromComposer('run the long migration');
    const { mockId } = await registered;
    await started;
    await gate;
    await expect(timeline().getByText('Halfway through')).toBeVisible();
    // B was clear at the send.
    expect(await selectedEmail()).toBe(B);
    const before = switchEvents();

    const switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    await setReading(B, [session(96), weeklyAll(40)], { limitId: 'session', percent: 96 });
    const event = await switched;
    expect(event).toMatchObject({ fromLabel: B, toLabel: A, reason: 'threshold', usedPercent: 96 });
    expect(switchEvents()).toBe(before + 1);

    // The turn is still held at its gate: the selection moved under it.
    const mocks = await harness.rpc<Array<{ mockId: string; openGate?: string }>>('HarnessListMocks');
    expect(mocks.find((mock) => mock.mockId === mockId)?.openGate).toBe('release');
    expect(harness.countEvents('provider:turn_completed', (ev: any) => ev.threadId === thread('midturn').id)).toBe(0);
    expect(await selectedEmail()).toBe(A);
    await expectSwitchToast(`Switched Claude account to ${A}. ${B} is at 96% of its Current session limit.`);
    const notice = `Switched Claude account from ${B} to ${A}: ${B} is at 96% of its Current session limit.`;
    await expect(timeline().getByTestId('notification-row').filter({ hasText: notice })).toBeVisible();
    expect(await noticeItems('midturn')).toEqual([notice]);

    const done = turnCompleted('midturn');
    await harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name: 'release' });
    await done;
    await expect(timeline().getByText('Finished after the switch')).toBeVisible();
  });

  test('a model-scoped limit moves only a thread of that model', async () => {
    await selectOnCard(B);
    await setReading(B, [
      session(20),
      weeklyAll(30),
      { kind: 'weekly_scoped', group: 'weekly', percent: 100, resets_at: hoursAhead(96), model: 'Fable' },
    ], { limitId: 'weekly_scoped:fable', percent: 100 });
    const before = switchEvents();

    await setScenario('opus', [reply('opus', 1, 'Opus still runs on B')]);
    await openThread('opus');
    const opusDone = turnCompleted('opus');
    await sendFromComposer('refactor the parser');
    await opusDone;
    await expect(timeline().getByText('Opus still runs on B')).toBeVisible();
    // The send path decides synchronously before the turn starts.
    expect(switchEvents()).toBe(before);
    expect(await selectedEmail()).toBe(B);
    expect(await noticeItems('opus')).toEqual([]);

    await setScenario('fable', [reply('fable', 1, 'Fable runs on A')]);
    await openThread('fable');
    const switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    const fableDone = turnCompleted('fable');
    await sendFromComposer('write the release notes');
    const event = await switched;
    expect(event).toMatchObject({ fromLabel: B, toLabel: A, reason: 'exhausted', limitName: 'Fable' });
    await expectSwitchToast(`Switched Claude account to ${A}. ${B} has used all of its Fable limit.`);
    expect(await selectedEmail()).toBe(A);
    const notice = `Switched Claude account from ${B} to ${A}: ${B} is at 100% of its Fable limit.`;
    await expect(timeline().getByTestId('notification-row').filter({ hasText: notice })).toBeVisible();
    expect(await noticeItems('fable')).toEqual([notice]);
    await fableDone;
    await expect(timeline().getByText('Fable runs on A')).toBeVisible();
  });

  test('card order returns an idle send to the first card, unless a person picked another', async () => {
    const strategy = settingsPage.getByLabel('Use accounts in order of');
    const strategySetting = async () =>
      (await harness.rpc<{ accountAutoSwitchStrategy: string }>('GetSettings')).accountAutoSwitchStrategy;
    await strategy.selectOption({ label: 'Card order' });
    await expect.poll(strategySetting).toBe('account-order');
    // A is the first card.
    expect((await claudeAccounts(harness)).map((account) => account.email)).toEqual([A, B]);

    // A forced switch away from the first card unpins the selection.
    await selectOnCard(A);
    await setReading(B, [session(10), weeklyAll(10)], { limitId: 'session', percent: 10 });
    await setReading(A, [session(97), weeklyAll(10)], { limitId: 'session', percent: 97 });
    await setScenario('order', [
      reply('order', 1, 'First pass on B'),
      reply('order', 2, 'Second pass back on A'),
      reply('order', 3, 'Third pass stays on B'),
    ]);
    await openThread('order');
    let switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    let done = turnCompleted('order');
    await sendFromComposer('first pass');
    expect(await switched).toMatchObject({ fromLabel: A, toLabel: B, reason: 'threshold' });
    await expectSwitchToast(`Switched Claude account to ${B}. ${A} is at 97% of its Current session limit.`);
    await done;
    expect(await selectedEmail()).toBe(B);

    // The first card is clear again: the next idle send returns to it.
    await setReading(A, [session(5), weeklyAll(10)], { limitId: 'session', percent: 5 });
    expect(await selectedEmail()).toBe(B);
    switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    done = turnCompleted('order');
    await sendFromComposer('second pass');
    expect(await switched).toMatchObject({ fromLabel: B, toLabel: A, reason: 'order' });
    await expectSwitchToast(`Switched Claude account to ${A}, the first available in the auto-switch order.`);
    await done;
    expect(await selectedEmail()).toBe(A);
    await expect(section().getByTestId(`provider-account-${await idOf(A)}`)).toContainText('Active');
    const notice = `Switched Claude account to ${A}, the first available in the auto-switch order.`;
    await expect(timeline().getByTestId('notification-row').filter({ hasText: notice })).toBeVisible();
    expect(await noticeItems('order')).toEqual([
      `Switched Claude account from ${A} to ${B}: ${A} is at 97% of its Current session limit.`,
      notice,
    ]);
    await expect(timeline().getByText('Second pass back on A')).toBeVisible();

    // A person picks the second card: an idle send leaves it there.
    await selectOnCard(B);
    const before = switchEvents();
    done = turnCompleted('order');
    await sendFromComposer('third pass');
    await done;
    await expect(timeline().getByText('Third pass stays on B')).toBeVisible();
    expect(switchEvents()).toBe(before);
    expect(await selectedEmail()).toBe(B);
    await expect(switchToasts()).toHaveCount(0);

    await strategy.selectOption({ label: 'Soonest weekly reset' });
    await expect.poll(strategySetting).toBe('soonest-reset');
  });

  test('an account outside the pool is never switched away from', async () => {
    await selectOnCard(B);
    await setReading(B, [session(20), weeklyAll(30)], { limitId: 'session', percent: 20 });
    const bId = await idOf(B);
    await section().getByTestId(`provider-account-auto-switch-${bId}`).uncheck();
    await expect.poll(() => autoSwitchOf(B)).toBe(false);
    await expect(section().getByTestId('provider-accounts-pool-hint-claude')).toBeVisible();
    await setReading(B, [session(98), weeklyAll(30)], { limitId: 'session', percent: 98 });
    await chatPage.reload();
    const before = switchEvents();

    await setScenario('outside', [[
      emit(textLines('msg-outside-1', 'Working on B')),
      { waitSignal: { name: 'release' } },
      emit([...textLines('msg-outside-2', 'Done on B'), RESULT_LINE]),
    ]]);
    await openThread('outside');
    const registered = harness.waitForEvent<HarnessMockEventData>(
      'harness:mock', (ev) => ev.scenario === 'auto-switch-outside' && ev.report.kind === 'registered', 30_000);
    const gate = harness.waitForEvent<HarnessMockEventData>(
      'harness:mock', (ev) => ev.scenario === 'auto-switch-outside' && ev.report.kind === 'waiting_signal', 30_000);
    await sendFromComposer('keep going on this account');
    const { mockId } = await registered;
    await gate;
    // The send path, past the threshold: nothing moved.
    expect(switchEvents()).toBe(before);
    expect(await selectedEmail()).toBe(B);

    // A reading with the turn in flight: nothing moves either.
    await setReading(B, [session(99), weeklyAll(30)], { limitId: 'session', percent: 99 });
    // Positive control, and the barrier for the reading above: once B is
    // back in the pool the next reading switches, and the only switch is
    // that one.
    await section().getByTestId(`provider-account-auto-switch-${bId}`).check();
    await expect.poll(() => autoSwitchOf(B)).toBe(true);
    expect(switchEvents()).toBe(before);
    expect(await selectedEmail()).toBe(B);
    await expect(switchToasts()).toHaveCount(0);
    expect(await noticeItems('outside')).toEqual([]);

    const switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    await setReading(B, [session(100), weeklyAll(30)], { limitId: 'session', percent: 100 });
    expect(await switched).toMatchObject({ fromLabel: B, toLabel: A, reason: 'exhausted', usedPercent: 100 });
    expect(switchEvents()).toBe(before + 1);
    await expectSwitchToast(`Switched Claude account to ${A}. ${B} has used all of its Current session limit.`);

    const done = turnCompleted('outside');
    await harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name: 'release' });
    await done;
  });

  test('"Switch at" moves the threshold, and the field keeps to its range', async () => {
    const field = settingsPage.getByTestId('settings-auto-switch-threshold');
    const threshold = async () => (await harness.rpc<{ accountAutoSwitchThreshold: number }>('GetSettings')).accountAutoSwitchThreshold;
    await expect(field).toHaveValue('95');
    expect(await threshold()).toBe(95);

    await selectOnCard(B);
    await setReading(B, [session(85), weeklyAll(30)], { limitId: 'session', percent: 85 });
    await setScenario('setting', [
      reply('setting', 1, 'Stayed on B at 85%'),
      reply('setting', 2, 'Moved to A at 85%'),
    ]);
    await openThread('setting');
    const before = switchEvents();
    const firstDone = turnCompleted('setting');
    await sendFromComposer('first pass');
    await firstDone;
    await expect(timeline().getByText('Stayed on B at 85%')).toBeVisible();
    expect(switchEvents()).toBe(before);
    expect(await selectedEmail()).toBe(B);

    await field.fill('80');
    await field.press('Enter');
    await field.blur();
    await expect.poll(threshold).toBe(80);

    const switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    const secondDone = turnCompleted('setting');
    await sendFromComposer('second pass');
    expect(await switched).toMatchObject({ fromLabel: B, toLabel: A, reason: 'threshold', usedPercent: 85 });
    await expectSwitchToast(`Switched Claude account to ${A}. ${B} is at 85% of its Current session limit.`);
    expect(await selectedEmail()).toBe(A);
    await expect(timeline().getByTestId('notification-row')
      .filter({ hasText: `Switched Claude account from ${B} to ${A}: ${B} is at 85% of its Current session limit.` })).toBeVisible();
    await secondDone;
    await expect(timeline().getByText('Moved to A at 85%')).toBeVisible();

    // Out of range: the field offers 50 to 100 and pulls an entry back
    // inside it, so the stored value never leaves the range.
    await expect(field).toHaveAttribute('min', '50');
    await expect(field).toHaveAttribute('max', '100');
    await field.fill('150');
    await field.blur();
    await expect(field).toHaveValue('100');
    await expect.poll(threshold).toBe(100);
    await field.fill('30');
    await field.blur();
    await expect(field).toHaveValue('50');
    await expect.poll(threshold).toBe(50);
    // A reload shows what was stored.
    await settingsPage.reload();
    await openClaudeAccounts(settingsPage);
    await expect(settingsPage.getByTestId('settings-auto-switch-threshold')).toHaveValue('50');

    await settingsPage.getByTestId('settings-auto-switch-threshold').fill('95');
    await settingsPage.getByTestId('settings-auto-switch-threshold').blur();
    await expect.poll(threshold).toBe(95);
  });

  test('a refused thread continues on the new account when the setting is on', async () => {
    const toggle = settingsPage.getByRole('switch', { name: 'Continue threads after a usage limit' });
    await expect(toggle).toHaveAttribute('aria-checked', 'false');
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-checked', 'true');
    await expect.poll(async () => (await harness.rpc<{ accountAutoContinue: boolean }>('GetSettings')).accountAutoContinue).toBe(true);

    await selectOnCard(B);
    await setReading(B, [session(10), weeklyAll(10)], { limitId: 'session', percent: 10 });
    await setScenario('continue', [refusalSteps, reply('continue', 2, 'Picked up where the limit stopped it')]);
    await openThread('continue');
    const switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    const refused = turnCompleted('continue');
    const continued = harness.waitForEvent<HarnessMockEventData>(
      'harness:mock',
      (ev) => ev.scenario === 'auto-switch-continue' && ev.report.kind === 'user_input' && (ev.report.input ?? '').includes(CONTINUE_TEXT),
      30_000,
    );
    await sendFromComposer('start the long job');
    await refused;

    expect(await switched).toMatchObject({ fromLabel: B, toLabel: A, reason: 'refused' });
    await expectSwitchToast(`Switched Claude account to ${A}. ${B} was refused for a usage limit.`);
    expect(await selectedEmail()).toBe(A);
    await expect(timeline().getByTestId('notification-row').filter({ hasText: `Switched Claude account from ${B} to ${A}: ${B} reached a usage limit` })).toBeVisible();

    await continued;
    const secondDone = turnCompleted('continue');
    await secondDone;
    await expect(timeline().getByText('Picked up where the limit stopped it')).toBeVisible();
    const chips = timeline().getByTestId('user-message-app-origin');
    await expect(chips).toHaveCount(1);
    await expect(chips).toHaveText('Agent Overflow');
    await expect(timeline().getByText(CONTINUE_TEXT, { exact: true })).toHaveCount(1);
    const items = await listItems(harness, thread('continue').id);
    expect(items.filter((item) => item.kind === 'user_text' && item.summary === CONTINUE_TEXT)).toHaveLength(1);
    expect(mockInputs('continue')).toEqual([
      expect.stringContaining('start the long job'),
      expect.stringContaining(CONTINUE_TEXT),
    ]);
    if (SCRATCH) {
      await timeline().screenshot({ path: path.join(SCRATCH, 'thread-switch-and-continue.png') });
    }
  });

  test('a refused thread stays stopped when the setting is off', async () => {
    const toggle = settingsPage.getByRole('switch', { name: 'Continue threads after a usage limit' });
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-checked', 'false');
    await expect.poll(async () => (await harness.rpc<{ accountAutoContinue?: boolean }>('GetSettings')).accountAutoContinue ?? false).toBe(false);

    await selectOnCard(B);
    await setScenario('stopped', [refusalSteps, reply('stopped', 2, 'Resumed by the person')]);
    await openThread('stopped');
    const switched = harness.waitForEvent<SwitchEvent>('provider:account_auto_switch', undefined, 30_000);
    const refused = turnCompleted('stopped');
    await sendFromComposer('start the other job');
    await refused;
    expect(await switched).toMatchObject({ fromLabel: B, toLabel: A, reason: 'refused' });
    await expectSwitchToast(`Switched Claude account to ${A}. ${B} was refused for a usage limit.`);
    expect(await selectedEmail()).toBe(A);
    await expect(timeline().getByTestId('notification-row').filter({ hasText: `Switched Claude account from ${B} to ${A}: ${B} reached a usage limit` })).toBeVisible();
    await expect(timeline().getByTestId('user-message-app-origin')).toHaveCount(0);

    // A message from the person is the next input the mock reads: no
    // continue went out before it, and the thread runs only when asked.
    const done = turnCompleted('stopped');
    await sendFromComposer('carry on');
    await done;
    await expect(timeline().getByText('Resumed by the person')).toBeVisible();
    expect(mockInputs('stopped')).toEqual([
      expect.stringContaining('start the other job'),
      expect.stringContaining('carry on'),
    ]);
    await expect(timeline().getByTestId('user-message-app-origin')).toHaveCount(0);
    const items = await listItems(harness, thread('stopped').id);
    expect(items.filter((item) => item.kind === 'user_text' && item.summary === CONTINUE_TEXT)).toHaveLength(0);
  });

  test('nothing above surfaced an error', async () => {
    await expect(chatPage.getByTestId('toast').filter({ hasText: /Could not auto-switch|failed|Failed/ })).toHaveCount(0);
    expect(harness.countEvents<SwitchEvent>('provider:account_auto_switch', (ev) => !!ev.error)).toBe(0);
    expect(consoleErrors).toEqual([]);
  });
});
