// The review pane's Merge section, shared by the desktop and compact
// projects, through the shipped UI, the real backend and the fake forge,
// on a GitLab merge request and a GitHub pull request. Each needs one
// approval and its pipeline (GitLab: the project requires it; GitHub: the
// base requires the `unit` check), which is still running. Covered: the
// refs line's pill reads "Blocked · 2" and opens the section, whose rows
// name each blocker beside its fix; Approve stays pending until the
// pump's frame lands and then reads Unapprove, the pill drops to
// "Blocked · 1" and the approval is the viewer's verdict in the
// Conversation; Unapprove brings the blocker back and the withdrawal is a
// Conversation event row; Auto-merge when ready arms the forge's
// auto-merge (the armed row, "Auto-merge set") and its row's Cancel
// disarms it; a token without the scope refuses Approve with a toast
// naming the scope and the button free again; once the pipeline passes
// and a reviewer approved the pill reads "Ready to merge", a head that
// moved refuses Merge now with the moved-head sentence and a Reload
// action, and Merge now then merges: the pill reads "Merged", the section
// shows the merged row and no actions. On a GitLab merge train project
// Merge adds to the train, the armed row shows the place and Remove takes
// it off, with no merge around the train. A GitHub viewer GitHub refuses
// Update branch, a thread's resolve, its reply and its reactions sees each reason in the
// control's place, and no refused request is sent. On a GitHub base with
// a merge queue, Merge offers no method or branch delete, "Add to merge
// queue" puts the pull request in the queue (the armed row reads "In the
// merge queue · 1 of 1"), its Remove takes it out, and no direct merge is
// sent. Screenshots: the blocked and ready sections on desktop, the
// wrapped section header on compact, the train row on both.

import { randomBytes } from 'node:crypto';
import type { Locator, Page, TestInfo } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expandReviewSection,
  expectEveryForgeCallHandled,
  forgeInvocations,
  publishPullRequest,
  refuseForgeRoute,
  seedForge,
  type ForgeRepo,
} from './forge-helpers.js';
import { screenshot } from '../src/screenshot.js';

export interface MergeSurface {
  /** The compact project: taps, the phone viewport. */
  compact: boolean;
}

type Forge = 'gitlab' | 'github';

const NUMBER = { gitlab: 4410, github: 4411 } as const;
const MENU_ROW = { gitlab: `MR !${NUMBER.gitlab}`, github: `PR #${NUMBER.github}` } as const;
// The event the forge records for a withdrawn approval.
const WITHDRAWN = { gitlab: 'unapproved', github: 'review_dismissed' } as const;
// What each forge's adapter names in a moved-head refusal.
const MOVED_HEAD = {
  gitlab: 'The branch moved after you loaded this. Reload and try again.',
  github: 'The branch moved after you loaded this pull request. Reload and try again.',
} as const;
const SCOPE = { gitlab: 'api', github: 'repo' } as const;

// ApprovePR, as frontend/src/lib/transport/methodRoutes.ts lists it.
const APPROVE_PR_METHOD_ID = 580_543_372;

function pull(forge: Forge, ci: 'running' | 'success', approvedByReviewer: boolean): NonNullable<ForgeRepo['pulls']>[number] {
  return {
    number: NUMBER[forge],
    title: 'Queue: retry a failed delivery once',
    body: 'Retries a delivery that failed with a 5xx once before giving up.',
    author: 'sabossedgh',
    authorName: 'Sawmon Abossedgh',
    comments: [{ author: 'rmurphy', authorName: 'Randy Murphy', body: 'One question on the backoff.', createdAt: '2026-10-18T15:00:00Z' }],
    readiness: forge === 'github' ? { requiredApprovals: 1, requiredChecks: ['unit'] } : { requiredApprovals: 1 },
    reviews: approvedByReviewer ? [{ author: 'rmurphy', authorName: 'Randy Murphy', state: 'APPROVED', submittedAt: '2026-10-19T09:00:00Z' }] : [],
    ci: { id: 44_100, jobs: [{ id: 44_101, name: 'unit', stage: 'test', status: ci, log: 'unit tests\n' }] },
  };
}

function repo(forge: Forge): ForgeRepo {
  const suffix = randomBytes(4).toString('hex');
  return forge === 'gitlab'
    ? {
        forge,
        // A fixed id keeps project_id stable across the reseed.
        id: 4_400_000 + parseInt(suffix.slice(0, 4), 16),
        project: `ao-e2e/merge-${suffix}`,
        merge: { pipelineMustSucceed: true },
        pulls: [pull(forge, 'running', false)],
      }
    : {
        forge,
        project: `ao-e2e/merge-${suffix}`,
        merge: { autoMergeAllowed: true },
        pulls: [pull(forge, 'running', false)],
      };
}

async function seedMergeRequest(harness: HarnessApp, forge: Forge, title: string, forgeRepo: ForgeRepo = repo(forge)): Promise<ForgeRepo> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `merge-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  return publishPullRequest(harness, seed.projects[0].path, forgeRepo, {
    'src/queue/deliver.ts': 'export async function deliver(): Promise<void> {\n  await post();\n}\n',
  });
}

/** The review pane on the PR, opened the way each layout offers it. */
async function openReview(page: Page, title: string, forge: Forge, compact: boolean): Promise<Locator> {
  const row = page.getByTestId('thread-row').filter({ hasText: title });
  if (!compact) {
    await row.click();
    const badge = page.getByTestId('chat-header-pr-badge');
    await expect(badge).toBeVisible({ timeout: 30_000 });
    await badge.click();
  } else {
    await row.tap();
    const more = page.getByTestId('chat-header-more');
    const menu = page.locator('[data-popover]');
    const prRow = menu.getByRole('menuitem', { name: MENU_ROW[forge] });
    await expect(async () => {
      if ((await menu.count()) > 0) await page.keyboard.press('Escape');
      await expect(menu).toHaveCount(0);
      await more.tap();
      await expect(prRow).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 30_000 });
    await prRow.tap();
  }
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-pr-header')).toBeVisible();
  await expect(review.getByTestId('review-file-header-path').first()).toBeVisible();
  await expect.poll(() => review.evaluate((el) => {
    const r = el.getBoundingClientRect();
    return r.left >= 0 && r.right <= window.innerWidth ? `${r.left}` : 'moving';
  })).not.toBe('moving');
  return review;
}

/**
 * Holds the pump's pr:updated frames once armed: from the first one after
 * an ApprovePR call left the page, every pr:updated frame waits (in
 * order) until release, while every other server frame, the call's own
 * reply among them, passes. `replied` reports that reply delivered, so a
 * spec can prove the pane is pending on the pump's answer and not on an
 * unanswered call. Install before the page opens.
 */
async function holdPumpFrames(page: Page) {
  const state = { armed: false, callId: '', replied: false, holding: false, held: [] as (() => void)[] };
  const carriesUpdate = (frame: { type?: string; channel?: string; events?: { channel?: string }[] }) =>
    (frame.type === 'event' && frame.channel === 'pr:updated') ||
    (frame.type === 'batch' && (frame.events ?? []).some((event) => event.channel === 'pr:updated'));
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string; methodId?: number };
      if (state.armed && frame.type === 'rpc' && frame.methodId === APPROVE_PR_METHOD_ID && frame.id) state.callId = frame.id;
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string; channel?: string; events?: { channel?: string }[] };
      if (state.callId !== '' && frame.id === state.callId) state.replied = true;
      if (state.armed && state.callId !== '' && carriesUpdate(frame)) {
        state.holding = true;
        state.held.push(() => socket.send(message));
        return;
      }
      socket.send(message);
    });
  });
  return {
    arm() {
      state.armed = true;
    },
    get holding() {
      return state.holding;
    },
    get replied() {
      return state.replied;
    },
    release() {
      state.armed = false;
      state.callId = '';
      state.replied = false;
      state.holding = false;
      for (const send of state.held.splice(0)) send();
    },
  };
}

export function reviewMergeFlow({ compact }: MergeSurface): void {
  for (const forge of ['gitlab', 'github'] as const) {
    test(`${forge}: the Merge section drives approve, auto-merge and merge, and explains the forge's refusals`, async ({ harness, page }, testInfo: TestInfo) => {
      const title = `Merge thread ${forge}`;
      const seeded = await seedMergeRequest(harness, forge, title);
      const frames = await holdPumpFrames(page);
      await page.emulateMedia({ colorScheme: 'dark' });
      await harness.open(page);
      const review = await openReview(page, title, forge, compact);
      const press = (target: Locator) => (compact ? target.tap() : target.click());
      const shot = (target: Locator, name: string) => screenshot(target, { path: testInfo.outputPath(`${testInfo.project.name}-${forge}-${name}.png`) });

      // Blocked: the pill counts the blockers and opens the section.
      const pill = review.getByTestId('review-pr-readiness');
      await expect(pill).toHaveText('Blocked · 2', { timeout: 30_000 });
      const section = review.getByTestId('review-pr-merge');
      await expect(section).toHaveAttribute('data-open', 'false');
      await press(pill);
      await expect(section).toHaveAttribute('data-open', 'true');
      await expect(section.getByTestId('review-merge-pill')).toHaveText('Blocked · 2');
      const blockers = section.getByTestId('review-merge-blocker');
      await expect(blockers).toHaveCount(2);
      const approvals = section.locator('[data-testid="review-merge-blocker"][data-kind="approvals"]');
      const checks = section.locator('[data-testid="review-merge-blocker"][data-kind="checks"]');
      await expect(approvals).toContainText('Needs 1 more approval');
      await expect(approvals.getByTestId('review-merge-row-approve')).toBeVisible();
      await expect(checks).toContainText(forge === 'gitlab' ? 'The pipeline is still running' : '1 required check pending');
      await expect(checks.getByTestId('review-merge-row-pipeline')).toBeVisible();
      const approve = section.getByTestId('review-merge-approve');
      const trigger = section.getByTestId('review-merge-trigger');
      await expect(approve).toHaveText('Approve');
      await expect(trigger).toContainText(compact ? 'Auto-merge' : 'Auto-merge when ready');
      if (compact) {
        // The header's controls stay inside it, clear of the pill.
        const header = section.locator(':scope > div').first();
        await shot(header, 'merge-header');
        const headerBox = (await header.boundingBox())!;
        const pillBox = (await section.getByTestId('review-merge-pill').boundingBox())!;
        for (const control of [approve, trigger]) {
          const box = (await control.boundingBox())!;
          expect(box.x).toBeGreaterThanOrEqual(headerBox.x);
          expect(box.x + box.width).toBeLessThanOrEqual(headerBox.x + headerBox.width);
          expect(box.x >= pillBox.x + pillBox.width || box.y >= pillBox.y + pillBox.height).toBe(true);
        }
      } else {
        await shot(section, 'merge-blocked');
      }

      // Approve pends until the pump's frame lands, then reads Unapprove.
      // The call's reply is through (the pane holds the seq it named); the
      // frame carrying that seq is what it waits for.
      frames.arm();
      await press(approve);
      await expect.poll(() => frames.holding, { timeout: 15_000 }).toBe(true);
      await expect.poll(() => frames.replied, { timeout: 15_000 }).toBe(true);
      await expect(approve).toHaveText('Approve');
      await expect(approve).toHaveAttribute('aria-busy', 'true');
      await expect(trigger).toBeDisabled();
      frames.release();
      await expect(approve).toHaveText('Unapprove');
      await expect(approve).not.toHaveAttribute('aria-busy', 'true');
      await expect(pill).toHaveText('Blocked · 1');
      await expect(approvals).toHaveCount(0);
      await expect(section.locator('[data-testid="review-merge-settled"][data-kind="approvals"]').getByTestId('review-merge-you-approved')).toBeVisible();
      // The approval is the viewer's verdict in the feed; the forge's
      // approval event beside it is the same fact and is not repeated.
      const conversation = await expandReviewSection(page, 'review-pr-conversation');
      const verdict = conversation.getByTestId('review-conversation-verdict').filter({ hasText: 'ao-viewer' });
      await expect(verdict).toContainText('approved');
      await expect(conversation.locator('[data-testid="review-conversation-event"][data-kind="approved"]')).toHaveCount(0);

      // Unapprove withdraws it: the blocker is back and the withdrawal is
      // an event row, in the feed with no press.
      await press(approve);
      await expect(approve).toHaveText('Approve');
      await expect(pill).toHaveText('Blocked · 2');
      await expect(approvals).toHaveCount(1);
      await expect(conversation.locator(`[data-testid="review-conversation-event"][data-kind="${WITHDRAWN[forge]}"]`)).toContainText('ao-viewer');

      // Auto-merge when ready arms the forge's; the armed row's Cancel
      // disarms it.
      await press(trigger);
      const menu = page.getByTestId('review-merge-menu');
      await expect(menu).toBeVisible();
      await expect(menu.getByTestId('review-merge-now')).toHaveAttribute('aria-disabled', 'true');
      await press(menu.getByTestId('review-merge-auto'));
      const armed = section.getByTestId('review-merge-armed');
      await expect(armed).toContainText('Auto-merge set by ao-viewer');
      await expect(trigger).toContainText('Auto-merge set');
      await press(armed.getByTestId('review-merge-row-cancel-auto'));
      await expect(armed).toHaveCount(0);
      await expect(trigger).toContainText(compact ? 'Auto-merge' : 'Auto-merge when ready');

      // A token without the scope: the toast names it, Approve is free.
      await refuseForgeRoute(harness, forge === 'gitlab'
        ? { route: 'glab api approve', status: 403, times: 1, body: { error: 'insufficient_scope', error_description: 'The request requires higher privileges than provided by the access token.', scope: 'api' } }
        : { route: 'gh api pull reviews', status: 403, times: 1, header: { 'X-Accepted-OAuth-Scopes': 'repo', 'X-OAuth-Scopes': 'read:org' }, body: { message: 'Resource not accessible by integration' } });
      await press(approve);
      const scopeToast = page.getByTestId('toast').filter({ hasText: 'Approve failed' });
      await expect(scopeToast.getByTestId('toast-message')).toContainText(`lacks the \`${SCOPE[forge]}\` scope`);
      await expect(approve).toHaveText('Approve');
      await expect(approve).toBeEnabled();
      await expect(pill).toHaveText('Blocked · 2');
      await scopeToast.getByRole('button', { name: 'Dismiss Notification' }).click();

      // The pipeline passed and a reviewer approved: ready.
      // The reseed keeps the published refs and shas (publishPullRequest's).
      await seedForge(harness, [{ ...seeded, pulls: [{ ...seeded.pulls![0], ...pull(forge, 'success', true) }] }]);
      await press(conversation.getByTestId('review-refresh-comments'));
      await expect(pill).toHaveText('Ready to merge');
      await expect(blockers).toHaveCount(0);
      await expect(trigger).toContainText('Merge');
      if (!compact) await shot(section, 'merge-ready');

      // A head that moved: the moved-head sentence and Reload.
      await refuseForgeRoute(harness, forge === 'gitlab'
        ? { route: 'glab api merge', status: 409, times: 1, body: { message: 'SHA does not match HEAD of source branch: 0000000000000000000000000000000000000000' } }
        : { route: 'gh graphql MergePR', status: 200, times: 1, body: { data: { mergePullRequest: null }, errors: [{ type: 'UNPROCESSABLE', path: ['mergePullRequest'], message: 'Head branch was modified. Review and try the merge again.' }] } });
      await press(trigger);
      await press(page.getByTestId('review-merge-menu').getByTestId('review-merge-now'));
      const movedToast = page.getByTestId('toast').filter({ hasText: 'Merge failed' });
      await expect(movedToast.getByTestId('toast-message')).toHaveText(`Merge failed: ${MOVED_HEAD[forge]}`);
      await movedToast.getByRole('button', { name: 'Reload' }).click();
      await expect(movedToast).toHaveCount(0);
      await expect(pill).toHaveText('Ready to merge');

      // Merge now merges: the pill and the section follow the forge.
      await press(trigger);
      await press(page.getByTestId('review-merge-menu').getByTestId('review-merge-now'));
      await expect(pill).toHaveText('Merged');
      await expect(section.locator('[data-testid="review-merge-settled"][data-kind="merged"]')).toContainText('Merged by ao-viewer');
      await expect(section.getByTestId('review-merge-approve')).toHaveCount(0);
      await expect(section.getByTestId('review-merge-trigger')).toHaveCount(0);
      await expect(section.getByTestId('review-merge-blocker')).toHaveCount(0);

      const routes = (await forgeInvocations(harness)).map((call) => call.route);
      expect(routes).toContain(forge === 'gitlab' ? 'glab api merge' : 'gh graphql MergePR');
      await expectEveryForgeCallHandled(harness);
    });
  }

  // A GitLab merge train project, with another merge request already on
  // the train: Merge reads "Add to merge train" and offers no branch
  // delete or commit message; it puts the merge request on the train
  // behind the other one ("In the merge train · 2 of 2", the button "In
  // merge train"), and the row's Remove takes it off. Nothing merges
  // around the train.
  test('gitlab: a merge train project adds to the train, shows the place and removes it', async ({ harness, page }, testInfo: TestInfo) => {
    const title = 'Merge train thread';
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: `train-${randomBytes(4).toString('hex')}`,
          repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
          threads: [{ title, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
        },
      ],
    });
    const base = repo('gitlab');
    const ahead = {
      ...pull('gitlab', 'success', true),
      number: NUMBER.gitlab + 1,
      title: 'Ahead on the train',
      headRef: 'ahead',
      readiness: { autoMerge: { strategy: 'merge_train' as const } },
      ci: { id: 44_200, jobs: [{ id: 44_201, name: 'unit', stage: 'test', status: 'success' as const, log: 'unit tests\n' }] },
    };
    await publishPullRequest(harness, seed.projects[0].path, {
      ...base,
      merge: { pipelineMustSucceed: true, mergeTrains: true },
      pulls: [pull('gitlab', 'success', true), ahead],
    }, { 'src/queue/deliver.ts': 'export async function deliver(): Promise<void> {\n  await post();\n}\n' });
    await page.emulateMedia({ colorScheme: 'dark' });
    await harness.open(page);
    const review = await openReview(page, title, 'gitlab', compact);
    const press = (target: Locator) => (compact ? target.tap() : target.click());

    const pill = review.getByTestId('review-pr-readiness');
    await expect(pill).toHaveText('Ready to merge', { timeout: 30_000 });
    await press(pill);
    const section = review.getByTestId('review-pr-merge');
    const trigger = section.getByTestId('review-merge-trigger');
    await expect(trigger).toContainText(compact ? 'Add to train' : 'Add to merge train');
    await press(trigger);
    const menu = page.getByTestId('review-merge-menu');
    await expect(menu.getByTestId('review-merge-now')).toContainText('Add to merge train');
    await expect(menu.getByTestId('review-merge-delete-branch')).toHaveCount(0);
    await expect(menu.getByTestId('review-merge-edit-message')).toHaveCount(0);
    await press(menu.getByTestId('review-merge-now'));

    const armed = section.getByTestId('review-merge-armed');
    await expect(armed).toContainText('In the merge train · 2 of 2');
    await expect(armed).toContainText('Added by ao-viewer');
    await expect(trigger).toContainText(compact ? 'Queued' : 'In merge train');
    await expect(pill).toHaveText('Ready to merge');
    await screenshot(armed, { path: testInfo.outputPath(`${testInfo.project.name}-merge-train.png`) });
    await press(armed.getByTestId('review-merge-row-cancel-auto'));
    await expect(armed).toHaveCount(0);
    await expect(trigger).toContainText(compact ? 'Add to train' : 'Add to merge train');

    const routes = (await forgeInvocations(harness)).map((call) => call.route);
    expect(routes).toContain('glab api add to merge train');
    expect(routes).not.toContain('glab api merge');
    await expectEveryForgeCallHandled(harness);
  });

  test('github: Update branch and the thread actions GitHub refuses this viewer say why in their place', async ({ harness, page }) => {
    const title = 'Merge thread refused controls';
    const base = repo('github');
    await seedMergeRequest(harness, 'github', title, {
      ...base,
      merge: { ...base.merge, permission: 'READ' },
      pulls: [{
        ...pull('github', 'success', true),
        readiness: { behind: true },
        viewer: { updateBranch: false, resolve: false, reply: false, react: false },
        threads: [{ id: 'T_refused', path: 'src/queue/deliver.ts', line: 2, comments: [{ author: 'rmurphy', body: 'Should this retry?', createdAt: '2026-10-19T10:00:00Z', reactions: [{ name: 'thumbsup', user: 'octo' }] }] }],
      }],
    });
    await harness.open(page);
    const review = await openReview(page, title, 'github', compact);
    const press = (target: Locator) => (compact ? target.tap() : target.click());

    const pill = review.getByTestId('review-pr-readiness');
    await expect(pill).toHaveText(/Blocked/, { timeout: 30_000 });
    await press(pill);
    const section = review.getByTestId('review-pr-merge');
    await expect(section).toHaveAttribute('data-open', 'true');
    const rebase = section.locator('[data-testid="review-merge-blocker"][data-kind="rebase"]');
    await expect(rebase.getByTestId('review-merge-cannot-update')).toHaveText('GitHub does not let you update this branch.');
    await expect(rebase.getByTestId('review-merge-row-rebase')).toHaveCount(0);

    const conversation = await expandReviewSection(page, 'review-pr-conversation');
    const card = conversation.locator('[data-testid="review-conversation-thread"][data-thread-id="T_refused"]');
    const resolve = card.getByTestId('review-thread-resolve');
    await expect(resolve).toBeDisabled();
    await expect(resolve).toHaveAttribute('title', "Only the pull request's author and people with write access can resolve or reopen conversations.");
    await expect(card.getByTestId('review-thread-cannot-reply')).toHaveText('GitHub does not let you reply to this conversation.');
    await expect(card.getByTestId('review-thread-reply')).toHaveCount(0);
    const chip = card.getByTestId('review-reaction-chip');
    await expect(chip).toBeDisabled();
    await expect(chip).toHaveAttribute('title', 'GitHub does not let you react to this comment.');

    const routes = (await forgeInvocations(harness)).map((call) => call.route);
    expect(routes).not.toContain('gh graphql UpdatePRBranch');
    expect(routes).not.toContain('gh graphql SetThreadResolved');
    expect(routes).not.toContain('gh graphql AddReaction');
    await expectEveryForgeCallHandled(harness);
  });

  test('github: on a merge queue base, Merge adds the pull request to the queue, shows its place, and Remove takes it out', async ({ harness, page }) => {
    const title = 'Merge thread queue';
    const base = repo('github');
    await seedMergeRequest(harness, 'github', title, {
      ...base,
      merge: { mergeQueue: true },
      pulls: [{ ...pull('github', 'success', true), readiness: {} }],
    });
    await harness.open(page);
    const review = await openReview(page, title, 'github', compact);
    const press = (target: Locator) => (compact ? target.tap() : target.click());

    const pill = review.getByTestId('review-pr-readiness');
    await expect(pill).toHaveText('Ready to merge', { timeout: 30_000 });
    await press(pill);
    const section = review.getByTestId('review-pr-merge');
    await expect(section).toHaveAttribute('data-open', 'true');
    const trigger = section.getByTestId('review-merge-trigger');
    await expect(trigger).toContainText(compact ? 'Add to queue' : 'Add to merge queue');

    // The queue's method applies: the menu offers none, nor a branch delete.
    await press(trigger);
    const menu = page.getByTestId('review-merge-menu');
    await expect(menu).toBeVisible();
    await expect(menu.locator('[data-testid^="review-merge-method-"]')).toHaveCount(0);
    await expect(menu.getByTestId('review-merge-delete-branch')).toHaveCount(0);
    await press(menu.getByTestId('review-merge-now'));
    const queued = section.locator('[data-testid="review-merge-armed"][data-queued="true"]');
    await expect(queued).toContainText('In the merge queue · 1 of 1');
    await expect(queued).toContainText('Added by ao-viewer');
    await expect(trigger).toContainText(compact ? 'Queued' : 'In merge queue');

    // Remove takes it out; the section offers the queue again.
    await press(queued.getByTestId('review-merge-row-cancel-auto'));
    await expect(section.getByTestId('review-merge-armed')).toHaveCount(0);
    await expect(trigger).toContainText(compact ? 'Add to queue' : 'Add to merge queue');

    const routes = (await forgeInvocations(harness)).map((call) => call.route);
    expect(routes).toContain('gh graphql EnqueuePR');
    expect(routes).toContain('gh graphql DequeuePR');
    expect(routes).not.toContain('gh graphql MergePR');
    await expectEveryForgeCallHandled(harness);
  });
}
