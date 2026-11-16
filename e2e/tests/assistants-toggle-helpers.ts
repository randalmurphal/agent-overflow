// Shared body of assistants-toggle.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Turning assistants off"): an assistant
// with a short loop, the Assistants setting turned off and on through
// Settings, and what the window and the loop do meanwhile.
import type { Page } from '@playwright/test';

import { expect } from './fixtures.js';
import type { HarnessApp, HarnessMockEventData } from '../src/harness.js';
import { longPress } from './touch-helpers.js';
import { assistantRow, assistantsSection, type Layout } from './sidebar-sections-helpers.js';
import type { ProviderName } from './thread-tools-helpers.js';
import { capture, dbWake, registerThrough, seedWakeAgent, wakeInput } from './wake-rows-helpers.js';

const TITLE = 'Toggle agent';
const LOOP_FIRED = 'Loop "beat" fired.';
const EVERY_SECONDS = 3;
const NOTE = 'Assistants are turned off in Settings. Turn them on to continue this thread.';

async function press(page: Page, layout: Layout, target: ReturnType<Page['locator']>): Promise<void> {
  if (layout === 'desktop') await target.click();
  else await target.tap();
}

/** Turns the Assistants setting through its Settings page. */
async function setAssistants(page: Page, layout: Layout, on: boolean): Promise<void> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await press(page, layout, page.getByTestId('compact-back'));
  }
  await press(page, layout, page.getByTestId('sidebar-settings-button'));
  await press(page, layout, page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Assistants', exact: true }));
  await expect(page.getByTestId('settings-page-header')).toContainText('Assistants');
  const toggle = page.getByTestId('settings-assistants').getByRole('switch', { name: 'Toggle assistants' });
  await expect(toggle).toHaveAttribute('aria-checked', on ? 'false' : 'true');
  await press(page, layout, toggle);
  await expect(toggle).toHaveAttribute('aria-checked', on ? 'true' : 'false');
  await press(page, layout, page.getByRole('button', { name: 'Close Settings', exact: true }));
  await expect(page.getByTestId('settings-overlay')).toHaveCount(0);
}

/** How long the loop's next fire has been due, in milliseconds. */
function overdue(dataDir: string, thread: string): number {
  const wake = dbWake(dataDir, thread, 'beat');
  return wake?.next_at ? Date.now() - wake.next_at : 0;
}

/** The project's menu, opened as the layout opens it, and its item labels. */
async function projectMenuLabels(page: Page, layout: Layout, projectName: string): Promise<string[]> {
  const label = page.getByTestId('project-item').filter({ hasText: projectName }).getByTestId('project-item-label').first();
  const menu = page.getByRole('menuitem');
  if (layout === 'desktop') await label.click({ button: 'right' });
  else await longPress(page, label, menu.first());
  await expect(menu.first()).toBeVisible();
  const labels = await menu.evaluateAll((els) => els.map((el) => (el.textContent ?? '').trim()));
  await page.keyboard.press('Escape');
  await expect(menu).toHaveCount(0);
  return labels;
}

export async function runAssistantsToggle(harness: HarnessApp, page: Page, layout: Layout, provider: ProviderName): Promise<void> {
  const tag = `${layout === 'desktop' ? '' : 'compact-'}assistants-toggle`;
  const projectName = `${tag}-${provider}`;
  const agent = await seedWakeAgent(harness, provider, tag, TITLE);
  await registerThrough(harness, provider, agent, `${tag}-${provider}`, [
    { tool: 'thread_loop', args: { label: 'beat', note: 'check in', every_seconds: EVERY_SECONDS } },
  ]);
  // On: the loop fires, the section lists the assistant and its pane takes messages.
  await wakeInput(harness, agent.home, LOOP_FIRED);
  await harness.open(page);
  const row = assistantRow(page, agent.thread);
  await expect(row).toBeVisible();
  expect(await projectMenuLabels(page, layout, projectName)).toContain('New Assistant…');
  await press(page, layout, row);
  await expect(page.getByTestId('chat-header-title').filter({ visible: true })).toHaveText(TITLE);
  await expect(page.getByTestId('chat-header-next-thread').filter({ visible: true })).toBeVisible();
  await expect(page.getByTestId('assistants-off-note')).toHaveCount(0);

  // Off, from Settings: nothing of the assistant shows in the sidebar or
  // the project's menu.
  await setAssistants(page, layout, false);
  await expect(assistantsSection(page)).toHaveCount(0);
  await expect(page.locator(`[data-sidebar-thread-id="${agent.thread}"]`)).toHaveCount(0);
  expect(await projectMenuLabels(page, layout, projectName)).not.toContain('New Assistant…');
  await capture(page, `${provider}-${layout}-assistants-off-sidebar.png`, page.getByTestId('sidebar-sections'));

  // The loop does not fire: its next fire goes by, and by two more
  // intervals, with nothing sent, and its row is kept as it was.
  await expect.poll(() => overdue(agent.dataDir, agent.thread), { timeout: 30_000 }).toBeGreaterThan(EVERY_SECONDS * 1000);
  const fires = harness.countEvents<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === agent.home && (ev.report.input ?? '').includes(LOOP_FIRED),
  );
  await expect.poll(() => overdue(agent.dataDir, agent.thread), { timeout: 30_000 }).toBeGreaterThan(3 * EVERY_SECONDS * 1000);
  expect(harness.countEvents<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === agent.home && (ev.report.input ?? '').includes(LOOP_FIRED),
  )).toBe(fires);
  expect(dbWake(agent.dataDir, agent.thread, 'beat')?.state).toBe('armed');

  // On again, from Settings: the assistant and its thread are back, and the
  // loop serves the fires it missed once, saying how many.
  const resumed = harness.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === agent.home && /missed ([2-9]|\d{2,}) since the previous wake/.test(ev.report.input ?? ''),
    60_000,
  );
  await setAssistants(page, layout, true);
  await expect(row).toBeVisible();
  expect(await projectMenuLabels(page, layout, projectName)).toContain('New Assistant…');
  await resumed;
  await press(page, layout, row);
  await expect(page.getByTestId('chat-header-title').filter({ visible: true })).toHaveText(TITLE);
  await expect(page.getByTestId('chat-header-next-thread').filter({ visible: true })).toBeVisible();
  await expect(page.getByTestId('assistants-off-note')).toHaveCount(0);
}

/**
 * A pane showing the assistant's thread when the setting goes off stays
 * readable, saying why it takes nothing, and is as it was once it is on.
 * The setting is turned through the binding so the pane stays in front.
 */
export async function runAssistantsTogglePane(harness: HarnessApp, page: Page, layout: Layout, provider: ProviderName): Promise<void> {
  const tag = `${layout === 'desktop' ? '' : 'compact-'}assistants-toggle-pane`;
  const agent = await seedWakeAgent(harness, provider, tag, TITLE);
  await harness.open(page);
  await press(page, layout, assistantRow(page, agent.thread));
  const title = page.getByTestId('chat-header-title').filter({ visible: true });
  await expect(title).toHaveText(TITLE);
  const composer = page.getByTestId('composer-root');

  await harness.rpc('UpdateSettings', { assistantsEnabled: false });
  const note = page.getByTestId('assistants-off-note');
  await expect(note).toHaveText(NOTE);
  await expect(title).toHaveText(TITLE);
  await expect(composer).toBeHidden();
  await expect(page.getByTestId('chat-header-next-thread')).toHaveCount(0);
  await capture(page, `${provider}-${layout}-assistants-off-pane.png`, note);

  await harness.rpc('UpdateSettings', { assistantsEnabled: true });
  await expect(note).toHaveCount(0);
  await expect(composer.filter({ visible: true })).toBeVisible();
  await expect(page.getByTestId('chat-header-next-thread').filter({ visible: true })).toBeVisible();
}
