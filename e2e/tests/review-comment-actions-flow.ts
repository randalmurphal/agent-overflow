// The comment actions' fixture and steps, shared by
// review-comment-actions.spec.ts (desktop, both forges) and
// compact-review-comment-actions.spec.ts: a PR changing src/values.ts with
// two threads, the routes each forge's writes land on in the fake, and
// the pane's editors and thread cards.
import { randomBytes } from 'node:crypto';
import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';
import { publishPullRequest, type ForgeRepo } from './forge-helpers.js';
import { launchPullsHarness, openPinnedReview } from './pulls-page-flow.js';

export type Forge = ForgeRepo['forge'];

const BASE = 'export const one = 1;\nexport const two = 2;\nexport const three = 3;\nexport const four = 4;\n';
// Line 3 is added between two unchanged lines; line 6 is added at the end.
const HEAD = 'export const one = 1;\nexport const two = 2;\nexport const half = 2.5;\nexport const three = 3;\nexport const four = 4;\nexport const five = 5;\n';

export const ROOT_ID = 9101;
export const REPLY_ID = 9102;
export const ONLY_ID = 9103;

// The routes each forge's write lands on in the fake.
export const ROUTES: Record<Forge, {
  lineComment: string; reply: string; prComment: string; edit: string; delete: string;
  publish: string; heldReply: string;
}> = {
  github: {
    lineComment: 'gh api pull comments',
    reply: 'gh api pull comment reply',
    prComment: 'gh api issue comments',
    edit: 'gh graphql UpdateReviewComment',
    delete: 'gh graphql DeleteReviewComment',
    publish: 'gh api pull reviews',
    // A held reply goes up after the review, as a reply to its thread.
    heldReply: 'gh api pull comment reply',
  },
  gitlab: {
    lineComment: 'glab api create discussion',
    reply: 'glab api create discussion note',
    prComment: 'glab api create note',
    edit: 'glab api update discussion note',
    delete: 'glab api delete discussion note',
    publish: 'glab api publish draft notes',
    // A held reply is a draft note in the thread's discussion.
    heldReply: 'glab api create draft note',
  },
};

export interface Opened {
  harness: HarnessApp;
  review: Locator;
}

// A PR on `forge` changing src/values.ts, with two threads: one by
// another user on line 1 that the viewer answered, and one on line 5
// holding only the viewer's comment.
export async function seedPull(harness: HarnessApp, forge: Forge, name: string, threadTitle?: string): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `${name}-${forge}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Comments\n', 'src/values.ts': BASE } }] },
      threads: threadTitle ? [{ title: threadTitle, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }] : [],
    }],
  });
  await publishPullRequest(harness, seed.projects[0].path, {
    forge,
    project: `ao-e2e/${name}-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 7, title: 'Values', author: 'octo',
      threads: [
        { path: 'src/values.ts', line: 1, comments: [
          { id: ROOT_ID, author: 'octo', body: 'Why start at one?' },
          { id: REPLY_ID, author: 'ao-viewer', body: 'Counting starts there.' },
        ] },
        { path: 'src/values.ts', line: 5, comments: [{ id: ONLY_ID, author: 'ao-viewer', body: 'Only my note.' }] },
      ],
    }],
  }, { 'src/values.ts': HEAD });
}

export async function openPinned(page: Page, forge: Forge, name: string, compact = false): Promise<Opened> {
  const harness = await launchPullsHarness();
  await seedPull(harness, forge, name);
  await harness.open(page);
  const review = await openPinnedReview(page, 7, compact);
  return { harness, review };
}

export function line(review: Locator, text: string): Locator {
  return review.locator('[data-testid="review-line-block"] > div').filter({ hasText: text });
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

export function thread(review: Locator, text: string): Locator {
  return review.getByTestId('review-pr-thread').filter({ hasText: text });
}

/** Scrolls a card clear of the sticky file bar, for a screenshot. */
export async function centre(card: Locator): Promise<void> {
  await card.evaluate((el) => el.scrollIntoView({ block: 'center' }));
}

export async function openReply(review: Locator, text: string, compact = false): Promise<Locator> {
  const card = thread(review, text);
  const reply = card.getByTestId('review-thread-reply');
  await (compact ? reply.tap() : reply.click());
  const composer = card.getByTestId('review-thread-composer');
  await expect(composer).toBeVisible();
  return composer;
}

export async function closeAll(page: Page, harness: HarnessApp): Promise<void> {
  await page.close();
  await harness.close();
}
