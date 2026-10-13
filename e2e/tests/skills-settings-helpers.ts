// The Skills settings page against the harness's own provider homes
// (`<dataRoot>/home/.claude` and `<dataRoot>/home/.codex`), shared by the
// desktop and compact specs.
import { existsSync, lstatSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';

import { expect } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';

export const MANIFEST = '.agent-overflow-pstack.json';

export function skillsDir(harness: HarnessApp, provider: 'claude' | 'codex'): string {
  return join(harness.bootstrap.dataRoot, 'home', provider === 'claude' ? '.claude' : '.codex', 'skills');
}

/** Opens Settings on the Skills page and waits for both homes to be read. */
export async function openSkillsPage(page: Page): Promise<void> {
  await page.getByText('Settings', { exact: true }).first().click();
  await page.getByRole('tab', { name: 'Skills', exact: true }).click();
  await expect(page.getByTestId('settings-skills-claude-status')).toBeVisible();
  await expect(page.getByTestId('settings-skills-codex-status')).toBeVisible();
}

export function status(page: Page, provider: 'claude' | 'codex') {
  return page.getByTestId(`settings-skills-${provider}-status`);
}

/**
 * Asserts the tree is installed flattened under dir: real directories one
 * level under skills/, the forge script executable, no dependency folders,
 * and a manifest that names what was written.
 */
export function expectInstalled(dir: string): void {
  const skill = join(dir, 'poteto-mode');
  expect(lstatSync(skill).isDirectory()).toBe(true);
  expect(lstatSync(skill).isSymbolicLink()).toBe(false);
  expect(existsSync(join(skill, 'SKILL.md'))).toBe(true);
  expect(existsSync(join(dir, 'pstack'))).toBe(false);
  expect(statSync(join(skill, 'scripts', 'forge')).mode & 0o111).not.toBe(0);
  expect(existsSync(join(skill, 'scripts', 'node_modules'))).toBe(false);
  const manifest = JSON.parse(readFileSync(join(dir, MANIFEST), 'utf8')) as { complete: boolean; files: string[] };
  expect(manifest.complete).toBe(true);
  expect(manifest.files).toContain('poteto-mode/SKILL.md');
}

/**
 * Replaces the computer's host name the page header shows, so a screenshot
 * names no real machine.
 */
export async function scrubComputerName(page: Page): Promise<void> {
  await page.evaluate(() => {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      if (node.textContent?.startsWith('Computer: ')) node.textContent = 'Computer: this computer';
    }
  });
}
