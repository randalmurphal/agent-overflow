// A standing agent's home contents (docs/specs/standing-agents.md, "Agent
// home contents"), end to end on both mock providers.
//
// Coverage (standing-agent-home-contents-helpers.ts): instructions.md and the
// part of MEMORY.md above its archive marker injected at a session start,
// beside the app guide; the same bytes on a second start over unchanged
// files; a hygiene wake through a thread_loop the agent registered, whose
// rewrite of MEMORY.md reaches the start after it and not the session it
// ran in; a worker spawned with home_files whose first message opens with
// the attached verification file and whose footer names the agent, its
// role and the route of its questions; the role line on a message from the
// agent to an ordinary thread and none on one back; each file past its cap
// cut at its head. Then the timeline: the injected block is system prompt and
// never a message, the worker's first message is an agent row whose body
// shows the attached file and whose facts name the agent's role and the
// route of its questions, and the hygiene wake is an agent row followed by
// the pass's outcome.
//
// No mock speaks claude-tui; its leg is proven at the argv level by
// TestStandingAgentSessionsCarryTheirHomeFiles in internal/app.
import type { Page } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import type { ProviderName } from './thread-tools-helpers.js';
import {
  AGENT_SENTENCE,
  ATTACHED_OPENING,
  HYGIENE_LABEL,
  HYGIENE_OUTCOME,
  ROLE,
  VERIFICATION,
  centerInTimeline,
  expectUncovered,
  runHomeContents,
  scrollTimelineToTop,
  showTextUncovered,
} from './standing-agent-home-contents-helpers.js';

async function openThread(page: Page, title: string): Promise<void> {
  await page.getByTestId('thread-row').getByText(title, { exact: true }).click();
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a standing agent's home reaches its sessions and its messages carry its context`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    await runHomeContents(harness, provider, 'home-contents');

    await harness.open(page);
    // Every project has a verified forge origin.
    await expect(page.getByLabel(/^Could not fully verify/)).toHaveCount(0);
    // The injected block is the session's system prompt: no message shows it.
    await openThread(page, 'Release lead');
    const wake = page
      .locator('[data-testid="agent-message-row"][data-agent-message-kind="loop"]')
      .filter({ has: page.getByTestId('agent-message-sender').getByText(HYGIENE_LABEL, { exact: true }) });
    await expect(wake).toHaveCount(1);
    await expect(page.getByText(HYGIENE_OUTCOME)).toBeVisible();
    await expect(page.getByText('Your standing instructions')).toHaveCount(0);
    const opening = page.getByTestId('user-message-bubble').filter({ hasText: 'Set up your home.' });
    await expect(opening).toHaveCount(1);
    await scrollTimelineToTop(page, opening);
    await expect(page.getByText('1 Thread loop')).toBeVisible();
    await capture(page, `${provider}-agent-session.png`, opening);
    await expect(page.getByText('Load the agent-home skill', { exact: false })).toHaveCount(0);
    await expect(page.getByText(ROLE, { exact: false })).toHaveCount(0);
    // The wake and the pass's outcome after it, clear of the composer.
    const outcome = page.getByText(HYGIENE_OUTCOME);
    await centerInTimeline(outcome);
    await expectUncovered(outcome);
    await expectUncovered(wake.getByTestId('agent-message-toggle'));
    await capture(page, `${provider}-hygiene-wake.png`, wake, outcome);

    // The worker's first message is the agent's spawn prompt, opening with
    // the attached file. Its answer to the agent marked it read, so it is in
    // the agent's done list.
    await page.getByTestId('thread-row-agent-done-toggle').click();
    await openThread(page, 'Parser review');
    const first = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="spawn"]');
    await expect(first).toHaveCount(1);
    await expect(first.getByTestId('agent-message-sender')).toHaveText('Release lead');
    await expect(first.getByTestId('agent-message-role')).toHaveText(ROLE);
    await expect(first.getByTestId('agent-message-first-line')).toHaveText(ATTACHED_OPENING);
    // Expanded: the attached file and the message, then the facts the
    // footer carried, never the footer itself.
    await first.getByTestId('agent-message-toggle').click();
    const text = first.getByTestId('agent-message-text');
    await expect(text).toContainText(VERIFICATION);
    await expect(text).toContainText('Review the parser change on this branch.');
    await expect(first).not.toContainText(AGENT_SENTENCE);
    await expect(first.locator('[data-fact="Role"]')).toHaveText(ROLE);
    await expect(first.locator('[data-fact="Prompts"]')).toHaveText('Go to the sender, which spawned this thread');
    await showTextUncovered(page, first, 'Go to the sender, which spawned this thread');
    // The whole expanded message, attached file to facts, in one shot.
    await capture(page, `${provider}-worker-first-message.png`, first);
  });
}
