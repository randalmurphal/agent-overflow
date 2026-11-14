// Tool panes under the compact layout: Workflows opens from the list's tools
// row as a full-width pane on the thread screen, and Back is one step at a
// time. Inside a run detail the browser's Back (the same ladder as Android's,
// utils/stepBack.ts) steps to the Workflows home first, then to the list; the
// header's Back does the same. Reopening the tool reveals the same pane.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  doneResult,
  humanGateWorkflow,
  seedWorkflow,
  setClaudeScenario,
  startWorkflow,
  waitForWorkflowState,
} from './workflows-helpers.js';

function screen(page: Page) {
  return page.locator('html');
}

test('Workflows is a full-width pane and Back steps detail, home, then list', async ({ harness, page }, testInfo) => {
  await setClaudeScenario(harness, 'compact-tool-gate', [{ steps: [{ emit: { lines: [doneResult({ complete: true })] } }] }]);
  const project = await seedWorkflow(harness, 'compact-tool-flow', 'gate-flow', humanGateWorkflow('gate-flow'));
  const item = await startWorkflow(harness, project.projectId, 'gate-flow', 'Parked on a phone');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');

  await harness.open(page);
  await page.emulateMedia({ colorScheme: 'dark' });
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'list');
  await expect(page.getByTestId('sidebar-tool-workflows-badge')).toHaveText('1');

  await page.getByTestId('sidebar-tool-workflows').click();
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'thread');
  const pane = page.getByTestId('workflows-pane');
  await expect(pane).toBeVisible();
  const section = page.locator('section[data-pane-kind="workflows"]');
  const host = await page.getByTestId('pane-host').boundingBox();
  expect(Math.round((await section.boundingBox())!.width)).toBe(Math.round(host!.width));
  // No close control on compact; the header's Back returns to the list.
  await expect(section.getByTestId('pane-close')).toHaveCount(0);

  await page.getByTestId('workflow-run-row').filter({ hasText: 'Parked on a phone' }).click();
  await expect(page.getByTestId('workflow-run-detail')).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('compact-workflows-run-dark.png') });

  // Browser Back: the run detail first, the pane second.
  await page.goBack();
  await expect(page.getByTestId('workflow-run-detail')).toHaveCount(0);
  await expect(page.getByTestId('workflow-project-group')).toBeVisible();
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'thread');
  await page.goBack();
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'list');

  // The tools row reveals the pane it left, still mounted.
  await page.getByTestId('sidebar-tool-workflows').click();
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'thread');
  await expect(page.locator('section[data-pane-kind="workflows"]')).toHaveCount(1);

  // The header's Back: one step at a time, run → home → list.
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Parked on a phone' }).click();
  await expect(section.getByTestId('compact-back')).toHaveCount(0);
  await section.getByTestId('workflows-back').click();
  await expect(page.getByTestId('workflow-project-group')).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('compact-workflows-home-dark.png') });
  await section.getByTestId('compact-back').click();
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'list');
  await page.screenshot({ path: testInfo.outputPath('compact-list-tools-row-dark.png') });

  // Pull requests, whose list has no detail step yet: Back goes straight to
  // the list.
  await page.getByTestId('sidebar-tool-pulls').click();
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'thread');
  const pulls = page.locator('section[data-pane-kind="pulls"]');
  await expect(pulls.getByTestId('pulls-pane')).toBeVisible();
  await expect(pulls.getByTestId('pane-close')).toHaveCount(0);
  await pulls.getByTestId('compact-back').click();
  await expect(screen(page)).toHaveAttribute('data-compact-screen', 'list');
});
