// Deleting an assistant under the compact layout
// (docs/specs/standing-agents.md, "Deletion, archive and transfer"): opened
// from a long press on the agent row, the one confirm names the home folder
// and fits the phone, and Delete removes the home.
import { existsSync } from 'node:fs';

import { test, expect, type SeedResult } from './fixtures.js';
import { capture } from './wake-rows-helpers.js';
import { longPress } from './touch-helpers.js';

test('compact: the assistant delete confirm names the home and fits the phone', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'compact-agent-home', repo: {}, threads: [{ title: 'Release agent', assistant: true, turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }],
  });
  const [thread] = seed.projects[0].threadIds;
  const agent = await harness.rpc<{ home: string }>('GetStandingAgent', thread);
  await harness.open(page);
  const agentRow = page.locator(`[data-sidebar-thread-id="${thread}"]`);
  await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
  const deleteItem = page.getByRole('menuitem', { name: 'Delete', exact: true });
  await longPress(page, agentRow, deleteItem);
  await deleteItem.tap();
  const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
  await expect(dialog).toContainText(`its home folder ${agent.home} is removed with everything in it.`);
  const confirm = dialog.getByRole('button', { name: 'Delete', exact: true });
  await expect(confirm).toBeInViewport({ ratio: 1 });
  await capture(page, 'compact-assistant-delete-dialog.png', dialog);
  await confirm.tap();
  await expect(agentRow).toHaveCount(0);
  await expect.poll(() => existsSync(agent.home)).toBe(false);
});
