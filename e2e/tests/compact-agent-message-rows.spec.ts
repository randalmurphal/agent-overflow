// Agent message rows under the compact layout
// (docs/specs/agent-thread-tools.md, "How agent messages appear"): the
// flow of agent-message-rows.spec.ts on both mock providers, then each row
// kind folded and tapped open, its facts clear of the composer, and the
// sender opened from the header with a tap. The phone has no message
// navigator; the send-queue preview is the same component on both layouts
// and is checked on desktop.
import type { Locator, Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { captureAgentMessageInView as keep } from './wake-rows-helpers.js';
import { expectUncovered, showTextUncovered } from './standing-agent-home-contents-helpers.js';
import {
  AUDIT,
  BUSY,
  BUSY_TASK,
  INBOX,
  LEAD,
  LEAD_ASK,
  REPLY,
  REQUEST,
  SPAWN_PROMPT,
  runAgentFlow,
  seedInbox,
} from './agent-message-rows-helpers.js';

const openThread = async (page: Page, title: string) => {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').tap();
  }
  await page.getByTestId('thread-row').getByText(title, { exact: true }).tap();
  await expect(page.getByTestId('chat-header-title')).toHaveText(title);
};

const agentRow = (page: Page, kind: string) =>
  page.locator(`[data-testid="agent-message-row"][data-agent-message-kind="${kind}"]`);

/** Taps a folded row open and shows `text` of its body clear of the composer. */
async function open(page: Page, row: Locator, text: string): Promise<void> {
  const toggle = row.getByTestId('agent-message-toggle');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await toggle.tap();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await showTextUncovered(page, row.getByTestId('agent-message-body'), text);
}

for (const provider of ['claude', 'codex'] as const) {
  test(`compact ${provider}: spawn prompt, request and replies fit the phone`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    const flow = await runAgentFlow(harness, provider, `camr-${provider}`);
    await harness.open(page);
    await openThread(page, BUSY);
    await expect(page.getByTestId('user-message-bubble').filter({ hasText: BUSY_TASK })).toBeVisible();
    await flow.release();
    await flow.settled();

    const request = agentRow(page, 'request');
    await expect(request.getByTestId('agent-message-sender')).toHaveText(LEAD);
    await expect(request.getByTestId('agent-message-first-line')).toHaveText(REQUEST);
    await expectUncovered(request.getByTestId('agent-message-toggle'));
    await keep(page, `compact-${provider}-request-collapsed.png`, request);
    await open(page, request, flow.token);
    await expect(request.locator('[data-fact="User said"]')).toHaveText(LEAD_ASK);
    await keep(page, `compact-${provider}-request-expanded.png`);
    await request.getByTestId('agent-message-sender').tap();
    await expect(page.getByTestId('chat-header-title')).toHaveText(LEAD);

    const reply = agentRow(page, 'reply');
    await expect(reply.getByTestId('agent-message-sender')).toHaveText(BUSY);
    await expect(reply.getByTestId('agent-message-first-line')).toHaveText(REPLY);
    await expect(agentRow(page, 'finished').getByTestId('agent-message-sender')).toHaveText(AUDIT);
    await keep(page, `compact-${provider}-replies-collapsed.png`, reply);
    await open(page, reply, flow.token);
    await keep(page, `compact-${provider}-reply-expanded.png`);

    await openThread(page, AUDIT);
    const spawn = agentRow(page, 'spawn');
    await expect(spawn.getByTestId('agent-message-first-line')).toHaveText(SPAWN_PROMPT);
    await expect(page.getByTestId('user-message-bubble')).toHaveCount(0);
    await keep(page, `compact-${provider}-spawn-collapsed.png`, spawn);
    await open(page, spawn, LEAD_ASK);
    await keep(page, `compact-${provider}-spawn-expanded.png`);
  });

  test(`compact ${provider}: every other kind folds and opens on the phone`, async ({ harness, page }) => {
    const { rows } = await seedInbox(harness, provider, `camr-inbox-${provider}`);
    await harness.open(page);
    await openThread(page, INBOX);
    await expect(page.getByTestId('user-message-bubble')).toHaveCount(2);
    for (const row of rows) {
      const locator = agentRow(page, row.kind);
      await expect(locator.getByTestId('agent-message-kind')).toHaveText(row.label);
      await expect(locator.getByTestId('agent-message-sender')).toHaveText(row.sender);
      await expect(locator.getByTestId('agent-message-first-line')).toHaveText(row.firstLine);
    }
    await keep(page, `compact-${provider}-inbox-collapsed.png`);
    await open(page, agentRow(page, 'question'), 'Yes, for an answer');
    await keep(page, `compact-${provider}-question-expanded.png`);
    await open(page, agentRow(page, 'worker-question'), 'option: Yes');
    await open(page, agentRow(page, 'loop'), 'Check the overnight build.');
    await keep(page, `compact-${provider}-loop-expanded.png`);
  });
}
