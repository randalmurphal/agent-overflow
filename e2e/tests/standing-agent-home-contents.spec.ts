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
// agent to an ordinary thread and none on one back; nothing injected after
// demotion. Then the timeline: the injected block is system prompt and
// never a message, the worker's first message shows the attached file, and
// the hygiene wake shows with the pass's outcome.
//
// No mock speaks claude-tui; its leg is proven at the argv level by
// TestStandingAgentSessionsCarryTheirHomeFiles in internal/app.
import type { Page } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import type { ProviderName } from './thread-tools-helpers.js';
import { ATTACHED_OPENING, HYGIENE_LABEL, HYGIENE_OUTCOME, ROLE, VERIFICATION, runHomeContents } from './standing-agent-home-contents-helpers.js';

async function openThread(page: Page, title: string): Promise<void> {
  await page.getByTestId('thread-row').getByText(title, { exact: true }).click();
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a standing agent's home reaches its sessions and its messages carry its context`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    await runHomeContents(harness, provider, 'home-contents');

    await harness.open(page);
    // The injected block is the session's system prompt: no message shows it.
    await openThread(page, 'Release lead');
    const wake = page
      .getByTestId('user-message-bubble')
      .filter({ has: page.getByTestId('user-message-wake-origin').getByText(`Loop · ${HYGIENE_LABEL}`, { exact: true }) });
    await expect(wake).toHaveCount(1);
    await expect(page.getByText(HYGIENE_OUTCOME)).toBeVisible();
    await expect(page.getByText('Your standing instructions')).toHaveCount(0);
    const opening = page.getByTestId('user-message-bubble').filter({ hasText: 'Set up your home.' });
    await expect(opening).toHaveCount(1);
    await opening.scrollIntoViewIfNeeded();
    await expect(page.getByText('1 Thread loop')).toBeVisible();
    await capture(page, `${provider}-agent-session.png`, opening);
    await expect(page.getByText('Load the agent-home skill', { exact: false })).toHaveCount(0);
    await expect(page.getByText(ROLE, { exact: false })).toHaveCount(0);
    await wake.scrollIntoViewIfNeeded();
    await capture(page, `${provider}-hygiene-wake.png`, wake);

    // The worker's first message opens with the attached file.
    await openThread(page, 'Parser review');
    const first = page.getByTestId('user-message-bubble').filter({ hasText: ATTACHED_OPENING });
    await expect(first).toHaveCount(1);
    await expect(first).toContainText(VERIFICATION);
    await expect(first).toContainText('Review the parser change on this branch.');
    await first.scrollIntoViewIfNeeded();
    await capture(page, `${provider}-worker-first-message.png`, first);
  });
}
