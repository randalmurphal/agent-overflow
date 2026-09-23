// The fake forge from a spec: seed the pull or merge requests `gh` and
// `glab` answer from, read back what the app asked them, and reach a PR's
// review pane the way a user does. The fixture and invocation shapes are
// internal/harness/forgefake's (fixture.go, engine.go); its AGENTS.md lists
// which invocations have handlers.

import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';

export interface ForgeComment {
  id?: number;
  author?: string;
  body: string;
  createdAt?: string;
}

export interface ForgePull {
  number: number;
  title: string;
  body?: string;
  state?: 'open' | 'closed' | 'merged';
  headRef?: string;
  baseRef?: string;
  headSha?: string;
  baseSha?: string;
  diff?: string;
  comments?: ForgeComment[];
}

/** A GitHub attachment by `url`, or a GitLab upload by `secret` and `filename`. */
export interface ForgeAttachment {
  url?: string;
  secret?: string;
  filename?: string;
  contentType?: string;
  base64?: string;
  text?: string;
}

export interface ForgeRepo {
  forge: 'github' | 'gitlab';
  project: string;
  host?: string;
  pulls?: ForgePull[];
  attachments?: ForgeAttachment[];
}

export interface ForgeInvocation {
  seq: number;
  cli: 'gh' | 'glab';
  args: string[];
  cwd: string;
  stdin?: string;
  route?: string;
  unhandled?: boolean;
  exitCode: number;
  stderr?: string;
}

export async function seedForge(harness: HarnessApp, repos: ForgeRepo[]): Promise<void> {
  await harness.rpc('HarnessForgeSeed', { repos });
}

export async function forgeInvocations(harness: HarnessApp): Promise<ForgeInvocation[]> {
  const log = await harness.rpc<{ invocations: ForgeInvocation[] }>('HarnessForgeInvocations', 0);
  return log.invocations;
}

/** Fail on any invocation the fake has no handler for, naming its argv. */
export async function expectEveryForgeCallHandled(harness: HarnessApp): Promise<void> {
  const unhandled = (await forgeInvocations(harness)).filter((call) => call.unhandled);
  expect(
    unhandled.map((call) => call.stderr),
    'the app made forge CLI calls the fake does not implement',
  ).toEqual([]);
}

/** Run one command palette command by its label. */
export async function runPaletteCommand(page: Page, label: string): Promise<void> {
  const input = page.getByTestId('command-palette-input');
  // A chord pressed while the page is still installing its keybindings is
  // dropped, so it is repeated until the palette answers.
  await expect(async () => {
    if (!(await input.isVisible())) await page.keyboard.press('ControlOrMeta+Shift+K');
    await expect(input).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 15_000 });
  await input.fill(label);
  await page.getByRole('listbox', { name: 'Commands' }).getByRole('option').filter({ hasText: label }).click();
}

/**
 * Start a thread from `url` through the palette and the Start Thread From
 * Pull/Merge Request dialog, then open its review pane. Answers the
 * review pane section.
 */
export async function openPullRequestReview(page: Page, url: string) {
  await runPaletteCommand(page, 'Thread: New from Pull/Merge Request');
  const dialog = page.getByTestId('thread-from-pr-dialog');
  await page.getByTestId('thread-from-pr-url').fill(url);
  await page.getByTestId('thread-from-pr-provider-claude').click();
  await page.getByTestId('thread-from-pr-submit').click();
  await expect(dialog).toHaveCount(0);
  await runPaletteCommand(page, 'Toggle review pane');
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-pr-header')).toBeVisible();
  return review;
}

/** Expand one collapsed review header section (`review-pr-description`, ...). */
export async function expandReviewSection(page: Page, testId: string) {
  const section = page.getByTestId(testId);
  const toggle = section.locator('button[aria-expanded]').first();
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  return section;
}
