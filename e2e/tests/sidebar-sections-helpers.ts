// Locators for the sidebar's two sections (docs/specs/standing-agents.md,
// "Sidebar"): an assistant's card in the Assistants section, and a project's
// entry and its rows in the Projects section, where the assistant has a row
// of its own in each project holding its nested work. An assistant's thread
// id names both its card and those rows, so a spec scopes a row to its
// section or project.
//
// The inner locator of a `has:` filter is page-rooted: Playwright matches it
// inside each candidate, so one rooted at a section would look for the
// section inside the row and never match.
import type { Locator, Page } from '@playwright/test';

/** Where a row is looked for: the page, a section or a project's entry. */
type Scope = Page | Locator;

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

/** An assistant's card in the Assistants section. */
export function cardRow(page: Page, id: string): Locator {
  return rowIn(assistantsSection(page), id);
}

/** A card's shell: the row with its state line and projects line under it. */
export function cardShell(page: Page, id: string): Locator {
  return shellIn(assistantsSection(page), id);
}

/** A row in the Projects section: an assistant's row there, not its card. */
export function projectRow(page: Page, id: string): Locator {
  return rowIn(projectsSection(page), id);
}

/** A row's shell in the Projects section. */
export function projectShell(page: Page, id: string): Locator {
  return shellIn(projectsSection(page), id);
}

export type SidebarRail = '[data-agent-member]' | '[data-group-member]';

/** The entry behind a rail in scope that holds a thread's row. */
export function railEntry(scope: Scope, rail: SidebarRail, id: string): Locator {
  return scope.locator(rail, { has: rowIn(pageOf(scope), id) });
}

/** The ids of the live rows behind a rail in scope, top to bottom; the done list's are not live. */
export async function liveIds(scope: Scope, rail: SidebarRail): Promise<string[]> {
  return await scope.locator(`${rail}:not([data-agent-done-row]) [data-sidebar-thread-id]`).evaluateAll((els) =>
    els.map((el) => (el as HTMLElement).dataset.sidebarThreadId ?? ''),
  );
}

/** The ids of the rows the open done footers in scope list, top to bottom. */
export async function doneIds(scope: Scope): Promise<string[]> {
  return await scope.locator('[data-agent-done-row] [data-sidebar-thread-id]').evaluateAll((els) =>
    els.map((el) => (el as HTMLElement).dataset.sidebarThreadId ?? ''),
  );
}
