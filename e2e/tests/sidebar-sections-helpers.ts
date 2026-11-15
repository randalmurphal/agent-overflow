// Locators for the sidebar's two sections (docs/specs/standing-agents.md,
// "Sidebar"): an assistant's one row in the Assistants section, and a
// project's entry and its rows in the Projects section.
//
// The inner locator of a `has:` filter is page-rooted: Playwright matches it
// inside each candidate, so one rooted at a section would look for the
// section inside the row and never match.
import type { Locator, Page } from '@playwright/test';

import { expect } from './fixtures.js';

/** Where a row is looked for: the page, a section or a project's entry. */
type Scope = Page | Locator;

export type Layout = 'desktop' | 'compact';

function pageOf(scope: Scope): Page {
  return 'page' in scope ? scope.page() : scope;
}

export function assistantsSection(page: Page): Locator {
  return page.getByTestId('sidebar-assistants-section');
}

export function projectsSection(page: Page): Locator {
  return page.getByTestId('sidebar-projects-section');
}

/** A project's entry in the Projects section, its rows inside it. */
export function projectItem(page: Page, projectId: string): Locator {
  return page.locator(`[data-testid="project-item"][data-project-id="${projectId}"]`);
}

/** A thread's row in scope. */
export function rowIn(scope: Scope, id: string): Locator {
  return scope.locator(`[data-sidebar-thread-id="${id}"]`);
}

/** The shell of a thread's row in scope: the row with the lines under it. */
export function shellIn(scope: Scope, id: string): Locator {
  return scope.locator('[data-testid="thread-row-shell"]', { has: rowIn(pageOf(scope), id) });
}

/** An assistant's row in the Assistants section. */
export function assistantRow(page: Page, id: string): Locator {
  return rowIn(assistantsSection(page), id);
}

/** An assistant row's shell: the row with its state line and projects line under it. */
export function assistantShell(page: Page, id: string): Locator {
  return shellIn(assistantsSection(page), id);
}

/** A row in the Projects section. */
export function projectRow(page: Page, id: string): Locator {
  return rowIn(projectsSection(page), id);
}

/** A row's shell in the Projects section. */
export function projectShell(page: Page, id: string): Locator {
  return shellIn(projectsSection(page), id);
}

async function press(target: Locator, layout: Layout): Promise<void> {
  if (layout === 'desktop') await target.click();
  else await target.tap();
}

/** Opens a sidebar row's menu: a right click, or the row's menu button under the compact layout. */
export async function openRowMenu(page: Page, layout: Layout, id: string): Promise<Locator> {
  if (layout === 'desktop') await rowIn(page, id).click({ button: 'right' });
  else await shellIn(page, id).getByTestId('thread-row-menu').tap();
  const menu = page.locator('[data-popover]').filter({ has: page.getByRole('menuitem') }).last();
  await expect(menu).toBeVisible();
  return menu;
}

/** Picks an item of a sidebar row's menu. */
export async function chooseRowMenuItem(page: Page, layout: Layout, id: string, label: string): Promise<void> {
  const menu = await openRowMenu(page, layout, id);
  await press(menu.getByRole('menuitem', { name: label, exact: true }), layout);
}

/** Opens an assistant's Previous Threads dialog from its row's menu, its first page listed. */
export async function openPreviousThreads(page: Page, layout: Layout, assistant: string): Promise<Locator> {
  await chooseRowMenuItem(page, layout, assistant, 'Previous Threads…');
  const dialog = page.getByTestId('previous-threads-dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId('previous-threads-loading')).toHaveCount(0);
  return dialog;
}

/** The thread ids a Previous Threads dialog lists, top to bottom. */
export async function previousThreadIds(dialog: Locator): Promise<string[]> {
  return await dialog.getByTestId('previous-threads-item').evaluateAll((els) =>
    els.map((el) => (el as HTMLElement).dataset.threadId ?? ''),
  );
}
