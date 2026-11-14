// Suggested changes, reactions, preview and copy link: the fixture and
// steps shared by review-suggestions-reactions.spec.ts (desktop, both
// forges) and compact-review-suggestions-reactions.spec.ts. A PR changing
// src/values.ts whose head files the fake holds (a suggestion's apply
// rewrites them), with a thread holding a suggestion and reactions, an
// outdated thread's suggestion and, on GitLab, an applied one.
import { randomBytes } from 'node:crypto';
import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';
import { publishPullRequest, refuseForgeRoute, type ForgeRepo } from './forge-helpers.js';
import { launchPullsHarness, openPinnedReview } from './pulls-page-flow.js';

export type Forge = ForgeRepo['forge'];

const BASE = 'export const one = 1;\nexport const two = 2;\nexport const three = 3;\nexport const four = 4;\n';
const HEAD = 'export const one = 1;\nexport const two = 2;\nexport const half = 2.5;\nexport const three = 3;\nexport const four = 4;\nexport const five = 5;\n';

export const SUGGEST_ID = 9201;
export const OUTDATED_ID = 9202;
export const APPLIED_ID = 9203;

export const SUGGESTION_BODY = 'A half reads better as a fraction:\n```suggestion\nexport const half = 5 / 2;\n```\nYour call.';

// The routes each forge's writes land on in the fake.
export const ROUTES: Record<Forge, { apply: string; react: string; unreact: string; reactionsRead: string | null; lineComment: string }> = {
  github: {
    apply: 'gh graphql SuggestionCommit',
    react: 'gh graphql AddReaction',
    unreact: 'gh graphql RemoveReaction',
    reactionsRead: null,
    lineComment: 'gh api pull comments',
  },
  gitlab: {
    apply: 'glab api apply suggestion',
    react: 'glab api create note award emoji',
    unreact: 'glab api delete note award emoji',
    reactionsRead: 'glab graphql MRNoteReactions',
    lineComment: 'glab api create discussion',
  },
};

export interface Opened {
  harness: HarnessApp;
  review: Locator;
}

export async function seedPull(harness: HarnessApp, forge: Forge, name: string): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `${name}-${forge}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Suggestions\n', 'src/values.ts': BASE } }] },
      threads: [],
    }],
  });
  // GitLab keeps any award; GitHub only the eight shared reactions.
  const extra = forge === 'gitlab' ? [{ name: 'fire', user: 'octo' }, { name: 'party_parrot', user: 'kchen' }] : [];
  await publishPullRequest(harness, seed.projects[0].path, {
    forge,
    project: `ao-e2e/${name}-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 7, title: 'Values', author: 'octo', body: 'Values change.',
      files: { 'README.md': '# Suggestions\n', 'src/values.ts': HEAD },
      threads: [
        { path: 'src/values.ts', line: 3, comments: [{
          id: SUGGEST_ID, author: 'octo', body: SUGGESTION_BODY,
          reactions: [{ name: 'thumbsup', user: 'octo' }, { name: 'heart', user: 'ao-viewer' }, ...extra],
        }] },
        { path: 'src/values.ts', line: 5, outdated: true, comments: [{
          id: OUTDATED_ID, author: 'kchen', body: 'Old code:\n```suggestion\nexport const four = 4.0;\n```',
        }] },
        ...(forge === 'gitlab' ? [{ path: 'src/values.ts', line: 1, comments: [{
          id: APPLIED_ID, author: 'kchen', body: 'Done already:\n```suggestion\nexport const one = 1;\n```', appliedSuggestions: [0],
        }] }] : []),
      ],
    }],
  }, { 'src/values.ts': HEAD });
}

export async function openPinned(page: Page, forge: Forge, name: string, compact = false, opts: { refuseReactionsRead?: boolean; clipboard?: boolean } = {}): Promise<Opened> {
  const harness = await launchPullsHarness();
  await seedPull(harness, forge, name);
  // GitLab's reactions read refused from the first read on.
  if (opts.refuseReactionsRead) await refuseForgeRoute(harness, { route: 'glab graphql MRNoteReactions', status: 403, body: { message: '403 Forbidden' } });
  if (opts.clipboard) await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(harness.url).origin });
  await harness.open(page);
  const review = await openPinnedReview(page, 7, compact);
  return { harness, review };
}

export function line(review: Locator, text: string): Locator {
  return review.locator('[data-testid="review-line-block"] > div').filter({ hasText: text });
}

export function thread(review: Locator, text: string): Locator {
  return review.getByTestId('review-pr-thread').filter({ hasText: text });
}

export async function openEditor(review: Locator, text: string, compact = false): Promise<Locator> {
  const row = line(review, text);
  if (compact) await row.getByTestId('review-add-comment').tap();
  else {
    await row.hover();
    await row.getByTestId('review-add-comment').click();
  }
  const editor = review.getByTestId('review-draft-editor');
  await expect(editor).toBeVisible();
  return editor;
}

/** Scrolls a card clear of the sticky file bar, for a screenshot. */
export async function centre(card: Locator): Promise<void> {
  await card.evaluate((el) => el.scrollIntoView({ block: 'center' }));
}

export function chip(scope: Locator, name: string): Locator {
  return scope.locator(`[data-testid="review-reaction-chip"][data-reaction="${name}"]`);
}

export async function press(target: Locator, compact: boolean): Promise<void> {
  await (compact ? target.tap() : target.click());
}

export async function closeAll(page: Page, harness: HarnessApp): Promise<void> {
  await page.close();
  await harness.close();
}
