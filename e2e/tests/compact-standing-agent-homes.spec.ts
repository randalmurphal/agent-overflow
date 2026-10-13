// The Demote confirm of a standing agent under the compact layout
// (docs/specs/standing-agents.md, "Standing agent"): opened from a long
// press on the agent row, its text and both choices fit the phone, and
// Demote and Delete Home removes the home.
import { existsSync } from 'node:fs';

import { test, expect, type SeedResult } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import { longPress } from './touch-helpers.js';

test('compact: the Demote confirm offers Demote and Delete Home and fits the phone', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'compact-agent-home', repo: {}, threads: [{ title: 'Release agent', turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }],
  });
  const [thread] = seed.projects[0].threadIds;
  const agent = await harness.rpc<{ home: string }>('PromoteStandingAgent', thread);
  await harness.open(page);
  const agentRow = page.locator(`[data-sidebar-thread-id="${thread}"]`);
  await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
  const demoteItem = page.getByRole('menuitem', { name: 'Demote Standing Agent' });
  await longPress(page, agentRow, demoteItem);
  await demoteItem.tap();
  const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
  await expect(dialog).toContainText('Demote keeps its home directory; Demote and Delete Home removes it.');
  const both = [dialog.getByRole('button', { name: 'Demote', exact: true }), dialog.getByRole('button', { name: 'Demote and Delete Home' })];
  for (const button of both) await expect(button).toBeInViewport({ ratio: 1 });
  await capture(page, 'compact-demote-dialog.png', dialog);
  await both[1].tap();
  await expect(agentRow).not.toHaveAttribute('data-sidebar-agent', '');
  await expect.poll(() => existsSync(agent.home)).toBe(false);
});
