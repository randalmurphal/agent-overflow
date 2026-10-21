// Tool panes in the strip (docs/specs/pull-requests.md, "Tools row"): the
// sidebar's tools row opens Pull requests or Workflows in the focused pane's
// place like a thread, or beside it with ctrl/cmd; a thread opened while a
// tool pane is focused takes its place; the layout, tool slots included,
// survives a reload with focus; Escape at the Workflows home leaves the pane
// alone; the chord and the close control take a tool pane down. The last case
// captures the tools row and a workflows pane beside a thread for the design
// pass.
import type { Locator, Page, TestInfo } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import {
  doneResult,
  humanGateWorkflow,
  seedWorkflow,
  setClaudeScenario,
  startWorkflow,
  waitForWorkflowState,
} from './workflows-helpers.js';

function sections(page: Page): Locator {
  return page.locator('[data-testid="pane-host"] > section[data-pane-id]');
}

async function stripKinds(page: Page): Promise<string[]> {
  return await sections(page).evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.paneKind ?? ''));
}

async function focusedKind(page: Page): Promise<string> {
  return await page.locator('section[data-pane-focused="true"]').getAttribute('data-pane-kind') ?? '';
}

async function seedThreads(harness: { rpc: <T>(method: string, ...args: unknown[]) => Promise<T> }): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'tool-panes',
      repo: {},
      threads: [
        { title: 'Alpha task', provider: 'claude', turns: [{ userText: 'alpha', items: [{ kind: 'assistant_text', summary: 'Alpha answer.' }] }] },
        { title: 'Beta task', provider: 'claude', turns: [{ userText: 'beta', items: [{ kind: 'assistant_text', summary: 'Beta answer.' }] }] },
      ],
    }],
  });
}

/** Drag the sidebar's resize edge until the sidebar is `width` px wide. */
async function dragSidebarTo(page: Page, width: number): Promise<void> {
  const box = await page.getByTestId('sidebar-resizer').boundingBox();
  const sidebar = await page.getByTestId('sidebar').boundingBox();
  if (!box || !sidebar) throw new Error('expected the sidebar and its resizer');
  const startX = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  await page.mouse.move(startX, y);
  await page.mouse.down();
  await page.mouse.move(startX + (width - sidebar.width), y, { steps: 8 });
  await page.mouse.up();
}

async function useDarkTheme(page: Page): Promise<void> {
  await page.emulateMedia({ colorScheme: 'dark' });
  await expect(page.locator('html')).toHaveClass(/\bdark\b/);
  await page.evaluate(() => Promise.all(
    document.getAnimations().filter((a) => a instanceof CSSTransition).map((a) => a.finished.catch(() => undefined)),
  ));
}

async function shoot(target: Page | Locator, testInfo: TestInfo, name: string): Promise<void> {
  await target.screenshot({ path: testInfo.outputPath(`${name}.png`) });
}

test('the tools row opens a tool in the focused pane\'s place, or beside it with ctrl or cmd', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Alpha task' }).click();
  await expect(page.getByText('Alpha answer.')).toBeVisible();
  expect(await stripKinds(page)).toEqual(['thread']);

  // A plain click replaces the focused thread pane, as a thread open does.
  await page.getByTestId('sidebar-tool-workflows').click();
  await expect(page.getByTestId('workflows-pane')).toBeVisible();
  expect(await stripKinds(page)).toEqual(['workflows']);
  await expect(page.getByTestId('sidebar-tool-workflows')).toHaveText('Workflows');
  await expect(page.getByTestId('sidebar-tool-workflows')).toHaveAttribute('aria-pressed', 'true');

  // The reverse: a thread opened over the focused tool pane takes its slot.
  await page.getByTestId('thread-row').filter({ hasText: 'Beta task' }).click();
  await expect(page.getByText('Beta answer.')).toBeVisible();
  await expect(page.getByTestId('workflows-pane')).toHaveCount(0);
  expect(await stripKinds(page)).toEqual(['thread']);

  // ctrl/cmd+click inserts beside the focused pane and replaces nothing.
  await page.getByTestId('sidebar-tool-pulls').click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByTestId('pulls-pane')).toBeVisible();
  await page.getByTestId('sidebar-tool-workflows').click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByTestId('workflows-pane')).toBeVisible();
  expect(await stripKinds(page)).toEqual(['thread', 'pulls', 'workflows']);
  expect(await focusedKind(page)).toBe('workflows');
  // The label follows the focused tool pane; the other tool is its icon.
  await expect(page.getByTestId('sidebar-tool-label')).toHaveText('Workflows');
  await expect(page.getByTestId('sidebar-tool-pulls')).toHaveAttribute('title', 'Pull requests');
});

test('tool panes keep their slots and focus across a reload, and Escape at home keeps the pane', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Alpha task' }).click();
  await expect(page.getByText('Alpha answer.')).toBeVisible();
  await page.getByTestId('sidebar-tool-pulls').click({ modifiers: ['ControlOrMeta'] });
  await page.getByTestId('thread-row').filter({ hasText: 'Beta task' }).click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByText('Beta answer.')).toBeVisible();
  await page.getByTestId('sidebar-tool-workflows').click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByTestId('workflows-pane')).toBeVisible();
  const before = await stripKinds(page);
  expect(before).toEqual(['thread', 'pulls', 'thread', 'workflows']);

  // §2.2: at home there is nothing for Escape to step back from, and a pane
  // is never closed by Escape.
  await page.locator('section[data-pane-kind="workflows"]').getByTestId('tool-pane-title').click();
  expect(await focusedKind(page)).toBe('workflows');
  await page.keyboard.press('Escape');
  await expect(page.getByTestId('workflows-pane')).toBeVisible();

  await page.reload();
  await expect(page.getByTestId('workflows-pane')).toBeVisible();
  await expect(page.getByTestId('pulls-pane')).toBeVisible();
  expect(await stripKinds(page)).toEqual(before);
  expect(await focusedKind(page)).toBe('workflows');
  // The label survives the reload with the focus it follows.
  await expect(page.getByTestId('sidebar-tool-label')).toHaveText('Workflows');

  // The chord closes the focused workflows pane; focus moves to its neighbour.
  await page.keyboard.press('ControlOrMeta+Shift+KeyW');
  await expect(page.getByTestId('workflows-pane')).toHaveCount(0);
  expect(await stripKinds(page)).toEqual(['thread', 'pulls', 'thread']);

  // The pane close control takes a tool pane down like any pane.
  await page.locator('section[data-pane-kind="pulls"]').getByTestId('pane-close').click();
  expect(await stripKinds(page)).toEqual(['thread', 'thread']);

  // And the chord reopens Workflows in the focused pane's place.
  await sections(page).nth(1).getByTestId('chat-header-title').click();
  expect(await focusedKind(page)).toBe('thread');
  await page.keyboard.press('ControlOrMeta+Shift+KeyW');
  await expect(page.getByTestId('workflows-pane')).toBeVisible();
  expect(await stripKinds(page)).toEqual(['thread', 'workflows']);
});

test('captures the tools row and a workflows pane beside a thread', async ({ harness, page }, testInfo) => {
  await setClaudeScenario(harness, 'tool-panes-gate', [{ steps: [{ emit: { lines: [doneResult({ complete: true })] } }] }]);
  const project = await seedWorkflow(harness, 'tool-panes-flow', 'gate-flow', humanGateWorkflow('gate-flow'));
  const item = await startWorkflow(harness, project.projectId, 'gate-flow', 'Port the parser');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');
  await seedThreads(harness);

  await page.setViewportSize({ width: 1440, height: 860 });
  await harness.open(page);
  await useDarkTheme(page);
  await expect(page.getByTestId('sidebar-tool-workflows-badge')).toHaveText('1');
  await page.getByTestId('thread-row').filter({ hasText: 'Alpha task' }).click();
  await expect(page.getByText('Alpha answer.')).toBeVisible();
  await page.getByTestId('sidebar-tool-workflows').click({ modifiers: ['ControlOrMeta'] });
  await page.getByTestId('workflow-run-row').filter({ hasText: 'Port the parser' }).click();
  await expect(page.getByTestId('workflow-run-map')).toBeVisible();
  await expect(page.getByTestId('workflows-breadcrumb')).toContainText('Port the parser');
  await expect(page.getByTestId('sidebar-tool-workflows')).toHaveAttribute('title', 'Workflows · 1 needs you');

  const row = page.getByTestId('sidebar-tools-row');
  for (const width of [300, 200]) {
    await dragSidebarTo(page, width);
    await expect.poll(async () => Math.round((await page.getByTestId('sidebar').boundingBox())?.width ?? 0)).toBe(width);
    // Every control stays on one line inside the row at the narrowest width.
    const heights = await row.locator('button').evaluateAll((els) => els.map((el) => el.getBoundingClientRect().height));
    const rowBox = await row.boundingBox();
    expect(new Set(heights).size).toBe(1);
    for (const button of await row.locator('button').all()) {
      const box = await button.boundingBox();
      expect(box!.x + box!.width).toBeLessThanOrEqual(rowBox!.x + rowBox!.width);
    }
    await shoot(row, testInfo, `tools-row-${width}-dark`);
  }
  await dragSidebarTo(page, 300);
  await shoot(page, testInfo, 'workflows-pane-beside-thread-dark');
});
