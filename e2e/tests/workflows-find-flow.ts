// mod+f on Workflows home filters its rows through the pane's find bar,
// shared by the desktop and compact projects (workflows-find.spec.ts,
// compact-workflows-find.spec.ts), against the real engine with seeded runs.
// Covered: mod+f at home opens the bar under the pane header, above home's
// controls and rows, with no step buttons; typing narrows the run and
// definition rows live by what each shows (a run's goal, its workflow, the
// project), and the bar's count and an empty state naming the query follow
// it; Enter focuses the first row; Escape closes the bar, restores the rows
// and returns focus; a run opened from the filtered list shows its detail,
// where mod+f opens no bar and Escape is the detail's, and Back returns to
// home still filtered. Filter rules: workflowData.test.ts.

import type { Locator, Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { doneResult, seedWorkflowProject, setClaudeScenario, singlePhaseWorkflow } from './workflows-helpers.js';

function runGoals(pane: Locator): Promise<string[]> {
  return pane.getByTestId('workflow-run-row').evaluateAll((rows) =>
    rows.map((el) => el.querySelector('[data-testid="workflow-run-title"]')?.textContent?.trim() ?? '').sort());
}

function definitionIds(pane: Locator): Promise<string[]> {
  return pane.getByTestId('workflow-definition-row').evaluateAll((rows) =>
    rows.map((el) => (el as HTMLElement).dataset.workflowId ?? '').sort());
}

function focusedTestId(page: Page): Promise<string | null> {
  return page.evaluate(() => (document.activeElement as HTMLElement | null)?.dataset.testid ?? null);
}

export function registerWorkflowsFindFlow({ compact }: { compact: boolean }): void {
  test('mod+f filters home\'s rows, Escape restores them, and a run detail has no find', async ({ harness, page }) => {
    await setClaudeScenario(harness, `find-${compact ? 'compact' : 'desktop'}`, [{ steps: [{ emit: { lines: [doneResult({ complete: true })] } }] }]);
    await seedWorkflowProject(
      harness,
      'find-workflows',
      [
        { name: 'port-flow', yaml: singlePhaseWorkflow('port-flow', '        - to: done') },
        { name: 'audit-flow', yaml: singlePhaseWorkflow('audit-flow', '        - to: done') },
      ],
      [
        { workflow: 'port-flow', goal: 'Port the parser', target: 'done' },
        { workflow: 'audit-flow', goal: 'Trim retention', target: 'done' },
      ],
    );
    await harness.open(page);
    await page.getByTestId('sidebar-tool-workflows').click();
    const pane = page.getByTestId('workflows-pane');
    await expect(pane).toBeVisible();
    await expect.poll(() => runGoals(pane)).toEqual(['Port the parser', 'Trim retention']);
    await expect.poll(() => definitionIds(pane)).toEqual(['audit-flow', 'port-flow']);

    // From home's project filter, mod+f opens the bar above the controls.
    const projectFilter = pane.getByTestId('workflows-project-filter');
    await projectFilter.focus();
    await page.keyboard.press('ControlOrMeta+f');
    const bar = pane.getByTestId('pane-find-bar');
    await expect(bar).toBeVisible();
    await expect(bar).toHaveAttribute('data-find-mode', 'filter');
    await expect(bar.getByTestId('pane-find-next')).toHaveCount(0);
    const input = bar.getByTestId('pane-find-input');
    await expect(input).toBeFocused();
    await expect(input).toHaveAttribute('placeholder', 'Filter workflows');
    const barBox = (await bar.boundingBox())!;
    const controlsBox = (await pane.getByTestId('workflows-controls').boundingBox())!;
    expect(barBox.y + barBox.height).toBeLessThanOrEqual(controlsBox.y + 1);

    // Typing narrows the rows and the count, live.
    await page.keyboard.type('parser');
    await expect.poll(() => runGoals(pane)).toEqual(['Port the parser']);
    await expect.poll(() => definitionIds(pane)).toEqual([]);
    await expect(bar.getByTestId('pane-find-count')).toHaveText('1 match');
    // A workflow id matches its definition and its runs.
    await input.fill('audit');
    await expect.poll(() => runGoals(pane)).toEqual(['Trim retention']);
    await expect.poll(() => definitionIds(pane)).toEqual(['audit-flow']);
    await expect(bar.getByTestId('pane-find-count')).toHaveText('2 matches');
    // The project name matches every row under it.
    await input.fill('find-workflows');
    await expect(bar.getByTestId('pane-find-count')).toHaveText('4 matches');
    await input.fill('zzz-none');
    await expect(pane.getByTestId('workflows-no-match')).toHaveText('No workflows match "zzz-none".');
    await expect(bar.getByTestId('pane-find-count')).toHaveText('No matches');

    // Enter focuses the first row.
    await input.fill('port');
    await expect.poll(() => runGoals(pane)).toEqual(['Port the parser']);
    await page.keyboard.press('Enter');
    await expect.poll(() => focusedTestId(page)).toBe('workflow-run-row');

    // Escape closes the bar and restores the rows.
    await page.keyboard.press('Escape');
    await expect(bar).toHaveCount(0);
    await expect.poll(() => runGoals(pane)).toEqual(['Port the parser', 'Trim retention']);
    // Reopened from the project filter, Escape returns focus there.
    await projectFilter.focus();
    await page.keyboard.press('ControlOrMeta+f');
    await expect(input).toHaveValue('port');
    await page.keyboard.press('Escape');
    await expect(bar).toHaveCount(0);
    await expect(projectFilter).toBeFocused();

    // A run opened from the filtered list: its detail has no find, and
    // Escape there is the detail's; home comes back still filtered.
    await projectFilter.focus();
    await page.keyboard.press('ControlOrMeta+f');
    await expect.poll(() => runGoals(pane)).toEqual(['Port the parser']);
    await pane.getByTestId('workflow-run-row').click();
    await expect(pane.getByTestId('workflow-run-detail')).toBeVisible();
    await expect(bar).toHaveCount(0);
    await page.keyboard.press('ControlOrMeta+f');
    await expect(bar).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(pane.getByTestId('workflow-run-detail')).toHaveCount(0);
    await expect(bar).toBeVisible();
    await expect(input).toHaveValue('port');
    await expect.poll(() => runGoals(pane)).toEqual(['Port the parser']);
    await input.focus();
    await page.keyboard.press('Escape');
    await expect(bar).toHaveCount(0);
    await expect.poll(() => runGoals(pane)).toEqual(['Port the parser', 'Trim retention']);
  });
}
