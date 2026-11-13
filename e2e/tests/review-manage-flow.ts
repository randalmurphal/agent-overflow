// Managing a PR from the review pane, shared by the desktop and compact
// projects, through the shipped UI, the real backend and the fake forge,
// on a GitLab merge request and a GitHub pull request
// (docs/specs/pull-requests.md, "Managing a PR"). Covered: a refused
// title edit shows the forge's sentence on the editor, which stays open,
// and the retry saves; a description edited elsewhere after the editor
// opened is refused with "The description changed since you opened it"
// and the editor stays open, and a fresh edit with Preview saves; the
// Details section adds and removes a reviewer, an assignee and a label,
// one change per picker, and a picker closed unchanged writes nothing;
// a reviewers change whose remove applied and whose add was refused
// shows the partial sentence on the Reviewers row, which shows the
// forge's reviewers (GitHub refuses the request; on GitLab the person
// left the project); a settled review is
// re-requested; the title bar's menu closes and reopens the PR and
// converts it to a draft, and the draft blocker's Mark ready makes it
// ready again; a refused Dismiss (GitHub) or Bypass (GitLab) shows its
// sentence on the changes-requested row with focus back on its button,
// and the retry dismisses with the reason kept (GitHub) or bypasses
// (GitLab), the blocker clears and focus lands on the Merge header;
// Merge now sends the edited commit message the fake received; the
// merged PR has no menu; and each action's event is in the Conversation
// feed. On GitLab: a refused management read leaves the PR loaded with
// its Merge, Description and Conversation, Details showing only the
// sentence and no management control anywhere, and the controls return
// on the next read of the PR (the Conversation's refresh) once the read
// answers; and a host without the set mutations' operationMode shows no
// Edit on the reviewers, assignees and labels rows while the title,
// description and state controls stay. A held Enter on the title bar's
// `⋯` opens the menu and activates nothing. Closing the pane with Ctrl+W
// while a picker holds a change writes that change (desktop).
// Screenshots (dark): the title bar's menu, the Details section
// collapsed and expanded, a picker with results, the description editor
// and its preview, the blocker row with Dismiss and its reason (GitHub)
// or Bypass (GitLab), and the commit message editor.

import { randomBytes } from 'node:crypto';
import type { Locator, Page, TestInfo } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expandReviewSection,
  expectEveryForgeCallHandled,
  forgeInvocations,
  publishPullRequest,
  clearForgeRefusal,
  refuseForgeRoute,
  seedForge,
  type ForgeHTTPInvocation,
  type ForgeRepo,
} from './forge-helpers.js';

export interface ManageSurface {
  /** The compact project: taps, the phone viewport. */
  compact: boolean;
}

type Forge = 'gitlab' | 'github';

const NUMBER = { gitlab: 4510, github: 4511 } as const;
const MENU_ROW = { gitlab: `MR !${NUMBER.gitlab}`, github: `PR #${NUMBER.github}` } as const;

const MEMBERS = [
  { login: 'sabossedgh', name: 'Sawmon Abossedgh' },
  { login: 'rmurphy', name: 'Randy Murphy' },
  { login: 'kchen', name: 'Kim Chen' },
  { login: 'alee', name: 'Al Lee' },
  { login: 'jdoe', name: 'Jane Doe' },
];
const LABELS = [
  { name: 'bug', color: '#d73a4a' },
  { name: 'feature', color: '#a2eeef' },
  { name: 'docs', color: '#0075ca' },
];

const TITLE = 'Queue: retry a failed delivery once';
const EDITED_TITLE = 'Queue: retry a failed delivery exactly once';
const BODY = 'Retries a delivery that failed with a 5xx once before giving up.';
const ELSEWHERE_BODY = 'Edited on the forge while the editor was open.';
const EDITED_BODY = 'Retries **once** on a 5xx.\n\n- [ ] load test';
const STALE = 'The description changed since you opened it. Copy your text, reload the description and edit again.';
const COMMIT_TITLE = 'Retry a failed delivery once';
const COMMIT_BODY = 'A 5xx is retried once before the delivery fails.';

function pull(forge: Forge): NonNullable<ForgeRepo['pulls']>[number] {
  const base = {
    number: NUMBER[forge],
    title: TITLE,
    body: BODY,
    author: 'sabossedgh',
    authorName: 'Sawmon Abossedgh',
    assignees: ['alee'],
    labels: ['bug'],
  };
  return forge === 'github'
    ? {
        ...base,
        // A change request blocks a GitHub merge only where the base
        // requires approving reviews.
        readiness: { requiredApprovals: 1 },
        reviewers: ['alee'],
        reviews: [
          { author: 'rmurphy', authorName: 'Randy Murphy', state: 'COMMENTED', body: 'One question on the backoff.', submittedAt: '2026-10-18T15:00:00Z' },
          { author: 'kchen', authorName: 'Kim Chen', state: 'CHANGES_REQUESTED', body: 'Cap the retries.', submittedAt: '2026-10-18T16:00:00Z' },
        ],
      }
    : {
        ...base,
        reviewers: ['alee', 'rmurphy', 'kchen'],
        changeRequests: ['kchen'],
        comments: [{ author: 'rmurphy', authorName: 'Randy Murphy', body: 'One question on the backoff.', createdAt: '2026-10-18T15:00:00Z' }],
      };
}

function repo(forge: Forge): ForgeRepo {
  const suffix = randomBytes(4).toString('hex');
  const common = { project: `ao-e2e/manage-${suffix}`, members: MEMBERS, labels: LABELS, pulls: [pull(forge)] };
  return forge === 'gitlab'
    ? { forge, ...common, id: 4_500_000 + parseInt(suffix.slice(0, 4), 16) }
    : { forge, ...common };
}

async function seedPullRequest(harness: HarnessApp, forge: Forge, title: string, adjust: (repo: ForgeRepo) => ForgeRepo = (same) => same): Promise<ForgeRepo> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `manage-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  return publishPullRequest(harness, seed.projects[0].path, adjust(repo(forge)), {
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

/** The HTTP writes the fake recorded on `routes` after `since`. */
async function writes(harness: HarnessApp, routes: readonly string[], since: number): Promise<ForgeHTTPInvocation[]> {
  return (await forgeInvocations(harness)).filter((call): call is ForgeHTTPInvocation =>
    call.via === 'http' && call.seq > since && routes.includes(call.route ?? ''));
}

async function lastSeq(harness: HarnessApp): Promise<number> {
  const calls = await forgeInvocations(harness);
  return calls.length ? calls[calls.length - 1].seq : 0;
}

// The routes each row's change writes on.
// A change removes first, then adds: one write per side.
const ROW_WRITES: Record<Forge, Record<'reviewers' | 'assignees' | 'labels', string[]>> = {
  gitlab: {
    reviewers: ['glab graphql PRSetReviewers'],
    assignees: ['glab graphql PRSetAssignees'],
    labels: ['glab graphql PRSetLabels'],
  },
  github: {
    reviewers: ['gh api request reviewers', 'gh api remove reviewers'],
    assignees: ['gh api add assignees', 'gh api remove assignees'],
    labels: ['gh graphql AddLabels', 'gh graphql RemoveLabels'],
  },
};

/** A GitLab set mutation's mode and what it named. */
function setMutation(call: ForgeHTTPInvocation): [unknown, unknown] {
  return [call.variables?.mode, call.variables?.usernames ?? call.variables?.labelIds];
}

export function reviewManageFlow({ compact }: ManageSurface): void {
  for (const forge of ['gitlab', 'github'] as const) {
    test(`${forge}: the review pane edits, assigns, labels, closes, reopens and merges the PR`, async ({ harness, page }, testInfo: TestInfo) => {
      test.setTimeout(240_000);
      const thread = `Manage thread ${forge}`;
      const seeded = await seedPullRequest(harness, forge, thread);
      await page.emulateMedia({ colorScheme: 'dark' });
      await harness.open(page);
      const review = await openReview(page, thread, forge, compact);
      const press = (target: Locator) => (compact ? target.tap() : target.click());
      const reveal = async (row: Locator) => { if (!compact) await row.hover(); };
      const shot = (target: Locator | Page, name: string) => target.screenshot({ path: testInfo.outputPath(`${testInfo.project.name}-${forge}-${name}.png`) });
      const header = review.getByTestId('review-pr-header');
      const title = header.getByTestId('review-pr-title');
      const statePill = header.getByTestId('review-pr-state');
      await expect(title).toContainText(TITLE, { timeout: 30_000 });

      // A refused title edit: the sentence on the editor, which stays
      // open; the retry saves and the title bar follows the forge.
      await refuseForgeRoute(harness, forge === 'gitlab'
        ? { route: 'glab api update merge request', status: 403, times: 1, body: { message: '403 Forbidden' } }
        : { route: 'gh graphql EditPR', status: 200, times: 1, body: { data: { updatePullRequest: null }, errors: [{ type: 'FORBIDDEN', path: ['updatePullRequest'], message: 'ao-viewer does not have the correct permissions to execute `UpdatePullRequest`' }] } });
      await reveal(header);
      await press(header.getByTestId('review-title-edit'));
      const titleInput = header.getByTestId('review-title-input');
      await expect(titleInput).toBeFocused();
      await titleInput.fill(EDITED_TITLE);
      await titleInput.press('Enter');
      const titleError = page.getByTestId('review-title-error');
      await expect(titleError).toBeVisible();
      await expect(titleError.getByTestId('review-title-error-message')).not.toBeEmpty();
      await expect(titleInput).toHaveValue(EDITED_TITLE);
      await titleInput.press('Enter');
      await expect(titleInput).toHaveCount(0);
      await expect(title).toContainText(EDITED_TITLE);
      await expect(header.getByTestId('review-title-edit')).toBeFocused();

      // The description: an edit made elsewhere after the editor opened is
      // refused and the editor stays open with its sentence.
      const description = review.getByTestId('review-pr-description');
      await reveal(description.locator(':scope > div').first());
      await press(description.getByTestId('review-description-edit'));
      const bodyInput = description.getByTestId('review-description-input');
      await expect(bodyInput).toHaveValue(BODY);
      await bodyInput.fill(`${BODY}\n\nMine.`);
      await seedForge(harness, [{ ...seeded, pulls: [{ ...seeded.pulls![0], title: EDITED_TITLE, body: ELSEWHERE_BODY }] }]);
      await press(description.getByTestId('review-description-save'));
      await expect(description.getByTestId('review-description-error-message')).toHaveText(STALE);
      await expect(bodyInput).toHaveValue(`${BODY}\n\nMine.`);
      await press(description.getByTestId('review-description-cancel'));
      await expect(description.getByTestId('review-description-editor')).toHaveCount(0);
      await expect(description).toContainText(ELSEWHERE_BODY);

      // A fresh edit, previewed and saved.
      await reveal(description.locator(':scope > div').first());
      await press(description.getByTestId('review-description-edit'));
      await expect(bodyInput).toHaveValue(ELSEWHERE_BODY);
      await bodyInput.fill(EDITED_BODY);
      if (!compact) await shot(description, 'description-edit');
      else await shot(page, 'description-edit');
      await press(description.getByTestId('review-description-preview'));
      const preview = description.getByTestId('review-description-preview-body');
      await expect(preview.locator('strong')).toHaveText('once');
      if (!compact) await shot(description, 'description-preview');
      else await shot(page, 'description-preview');
      await press(description.getByTestId('review-description-save'));
      await expect(description.getByTestId('review-description-editor')).toHaveCount(0);
      await expect(description.locator('strong')).toHaveText('once');

      // Details: collapsed, its header summing up the rows the forge
      // holds; opened, the rows.
      const details = review.getByTestId('review-pr-details');
      await expect(details).toHaveAttribute('data-open', 'false');
      await expect(details.getByTestId('review-details-summary')).toHaveText('3 reviewers · 1 assignee · 1 label');
      await shot(details, 'details-collapsed');
      await press(details.locator('button[aria-expanded]').first());
      await expect(details).toHaveAttribute('data-open', 'true');
      const reviewer = (login: string) => details.locator(`[data-testid="review-details-reviewer"][data-login="${login}"]`);
      await expect(reviewer('alee')).toHaveAttribute('data-state', 'PENDING');
      await expect(reviewer('rmurphy')).toHaveAttribute('data-state', 'COMMENTED');
      await expect(reviewer('kchen')).toHaveAttribute('data-state', 'CHANGES_REQUESTED');
      const assignees = () => details.getByTestId('review-details-assignee').evaluateAll((els) => els.map((el) => el.getAttribute('data-login')));
      await expect.poll(assignees).toEqual(['alee']);
      await expect(details.getByTestId('review-details-label')).toHaveText(['bug']);
      await shot(details, 'details-expanded');
      await expect(details.getByTestId('review-details-summary')).toHaveCount(0);

      // Each picker: one write for its row's adds and removes.
      const picker = page.getByTestId('review-picker');
      // A person by key (`u:login`), a label by its name (labels are keyed
      // by their forge id).
      const option = (key: string) => picker.locator(key.includes(':')
        ? `[data-testid="review-picker-option"][data-key="${key}"]`
        : `[data-testid="review-picker-option"][data-name="${key}"]`);
      async function editRow(row: 'reviewers' | 'assignees' | 'labels', add: string, query: string, remove: string, screenshot = false): Promise<ForgeHTTPInvocation[]> {
        const before = await lastSeq(harness);
        const rowEl = details.locator(`[data-testid="review-details-row"][data-row="${row}"]`);
        await reveal(rowEl);
        await press(details.getByTestId(`review-details-edit-${row}`));
        await expect(picker).toBeVisible();
        await expect(picker.getByTestId('review-picker-search')).toBeFocused();
        await expect(option(remove)).toHaveAttribute('data-checked', 'true');
        await picker.getByTestId('review-picker-search').fill(query);
        await expect(option(add)).toBeVisible();
        await expect(option(remove)).toHaveCount(0);
        await press(option(add));
        await expect(option(add)).toHaveAttribute('data-checked', 'true');
        if (screenshot) await shot(page, 'picker');
        await picker.getByTestId('review-picker-search').fill('');
        await press(option(remove));
        await expect(option(remove)).toHaveAttribute('data-checked', 'false');
        await page.keyboard.press('Escape');
        await expect(picker).toHaveCount(0);
        await expect(details.getByTestId(`review-details-edit-${row}`)).toBeFocused();
        let made: ForgeHTTPInvocation[] = [];
        await expect(async () => {
          made = await writes(harness, ROW_WRITES[forge][row], before);
          expect(made.length).toBe(2);
        }).toPass({ timeout: 15_000 });
        return made;
      }

      const reviewerWrites = await editRow('reviewers', 'u:jdoe', 'jane', 'u:alee', true);
      await expect(reviewer('jdoe')).toHaveAttribute('data-state', 'PENDING');
      await expect(reviewer('alee')).toHaveCount(0);
      if (forge === 'github') {
        expect(reviewerWrites.map((call) => [call.route, call.body])).toEqual([
          ['gh api remove reviewers', { reviewers: ['alee'], team_reviewers: [] }],
          ['gh api request reviewers', { reviewers: ['jdoe'], team_reviewers: [] }],
        ]);
      } else {
        expect(reviewerWrites.map(setMutation)).toEqual([['REMOVE', ['alee']], ['APPEND', ['jdoe']]]);
      }

      // A partial change: the remove applies and the add is refused. The
      // row says what applied and what was refused, and shows the forge's
      // reviewers. GitHub refuses the request; GitLab skips a person who
      // is no longer a member, which the app reads back as refused.
      const partialAt = await lastSeq(harness);
      const reviewersRow = details.locator('[data-testid="review-details-row"][data-row="reviewers"]');
      await reveal(reviewersRow);
      await press(details.getByTestId('review-details-edit-reviewers'));
      await picker.getByTestId('review-picker-search').fill('sawmon');
      await press(option('u:sabossedgh'));
      await picker.getByTestId('review-picker-search').fill('');
      await press(option('u:jdoe'));
      await expect(option('u:jdoe')).toHaveAttribute('data-checked', 'false');
      if (forge === 'github') {
        await refuseForgeRoute(harness, { route: 'gh api request reviewers', status: 422, times: 1, body: { message: 'Reviews may only be requested from collaborators.' } });
      } else {
        await seedForge(harness, [{
          ...seeded,
          members: MEMBERS.filter((member) => member.login !== 'sabossedgh'),
          pulls: [{ ...seeded.pulls![0], title: EDITED_TITLE, body: EDITED_BODY, reviewers: ['jdoe', 'rmurphy', 'kchen'] }],
        }]);
      }
      await page.keyboard.press('Escape');
      await expect(picker).toHaveCount(0);
      const reviewersError = details.getByTestId('review-details-error-reviewers-message');
      await expect(reviewersError).toContainText('jdoe');
      await expect(reviewersError).toContainText('sabossedgh');
      await expect(reviewersError).toContainText('refused');
      const partial = await writes(harness, ROW_WRITES[forge].reviewers, partialAt);
      if (forge === 'github') {
        expect(partial.map((call) => [call.route, call.body])).toEqual([
          ['gh api remove reviewers', { reviewers: ['jdoe'], team_reviewers: [] }],
          ['gh api request reviewers', { reviewers: ['sabossedgh'], team_reviewers: [] }],
        ]);
      } else {
        expect(partial.map(setMutation)).toEqual([['REMOVE', ['jdoe']], ['APPEND', ['sabossedgh']]]);
      }
      await expect(reviewer('jdoe')).toHaveCount(0);
      await expect(reviewer('sabossedgh')).toHaveCount(0);
      await expect(reviewer('kchen')).toHaveAttribute('data-state', 'CHANGES_REQUESTED');
      await shot(details, 'details-partial');
      await editRow('assignees', 'u:jdoe', 'jane', 'u:alee');
      await expect.poll(assignees).toEqual(['jdoe']);
      const labelWrites = await editRow('labels', 'feature', 'feat', 'bug');
      await expect(details.getByTestId('review-details-label')).toHaveText(['feature']);
      if (forge === 'gitlab') {
        const [removed, added] = labelWrites.map(setMutation);
        expect(removed[0]).toBe('REMOVE');
        expect(added[0]).toBe('APPEND');
        for (const ids of [removed[1], added[1]]) expect(ids).toEqual([expect.stringMatching(/^gid:\/\/gitlab\/ProjectLabel\/\d+$/)]);
      }

      // A picker closed with nothing changed writes nothing.
      const quiet = await lastSeq(harness);
      await reveal(details.locator('[data-testid="review-details-row"][data-row="labels"]'));
      await press(details.getByTestId('review-details-edit-labels'));
      await expect(option('feature')).toHaveAttribute('data-checked', 'true');
      await page.keyboard.press('Escape');
      await expect(picker).toHaveCount(0);
      expect(await writes(harness, ROW_WRITES[forge].labels, quiet)).toEqual([]);

      // Re-request the settled review.
      await reveal(reviewer('rmurphy'));
      await press(reviewer('rmurphy').getByTestId('review-details-rerequest'));
      await expect(reviewer('rmurphy')).toHaveAttribute('data-state', 'PENDING');

      // The title bar's menu: close, reopen, convert to draft; the draft
      // blocker's Mark ready.
      const stateMenu = header.getByTestId('review-pr-state-menu');
      const stateItems = page.getByTestId('review-pr-state-items');
      // A held Enter opens the menu, and its repeats never activate the
      // item focus moved to.
      const beforeHeld = await lastSeq(harness);
      await stateMenu.focus();
      await page.keyboard.down('Enter');
      for (let repeat = 0; repeat < 5; repeat += 1) await page.keyboard.down('Enter');
      await page.keyboard.up('Enter');
      await expect(stateItems.getByRole('menuitem')).toHaveText(['Convert to draft', 'Close']);
      await expect(statePill).toHaveText('open');
      expect((await forgeInvocations(harness)).filter((call) => call.seq > beforeHeld && call.via === 'http' && /Close|Convert|Draft|update merge request/.test(call.route ?? ''))).toEqual([]);
      await page.keyboard.press('Escape');
      await expect(stateItems).toHaveCount(0);
      await reveal(header);
      await press(stateMenu);
      await expect(stateItems.getByRole('menuitem')).toHaveText(['Convert to draft', 'Close']);
      await shot(page, 'state-menu');
      await press(stateItems.getByTestId('review-pr-state-close'));
      await expect(statePill).toHaveText('closed');
      await reveal(header);
      await press(stateMenu);
      await expect(stateItems.getByRole('menuitem')).toHaveText(['Reopen']);
      await press(stateItems.getByTestId('review-pr-state-reopen'));
      await expect(statePill).toHaveText('open');
      await reveal(header);
      await press(stateMenu);
      await press(stateItems.getByTestId('review-pr-state-draft'));
      await expect(statePill).toHaveText('draft');
      const merge = await expandReviewSection(page, 'review-pr-merge');
      await press(merge.locator('[data-testid="review-merge-blocker"][data-kind="draft"]').getByTestId('review-merge-row-ready'));
      await expect(statePill).toHaveText('open');

      // The review that requested changes: dismissed with a reason on
      // GitHub, bypassed on GitLab; the blocker clears.
      // The first try is refused: its sentence on the blocker row, and
      // focus back on the button that tried.
      const changes = merge.locator('[data-testid="review-merge-blocker"][data-kind="changes_requested"]');
      const changesError = changes.getByTestId('review-merge-change-request-error-message');
      const mergeHeader = merge.locator('button[aria-expanded]').first();
      await expect(changes).toHaveCount(1);
      if (forge === 'github') {
        await refuseForgeRoute(harness, { route: 'gh graphql DismissReview', status: 200, times: 1, body: { data: { dismissPullRequestReview: null }, errors: [{ type: 'FORBIDDEN', path: ['dismissPullRequestReview'], message: 'ao-viewer does not have the correct permissions to execute `DismissPullRequestReview`' }] } });
        const dismissButton = changes.getByTestId('review-merge-row-dismiss');
        const dismiss = page.getByTestId('review-dismiss-popover');
        const reason = dismiss.getByTestId('review-dismiss-reason');
        await press(dismissButton);
        await expect(reason).toBeFocused();
        await expect(dismiss.getByTestId('review-dismiss-confirm')).toBeDisabled();
        await reason.fill('Addressed in the retry cap.');
        await expect(dismiss.getByTestId('review-dismiss-confirm')).toBeEnabled();
        await shot(page, 'blocker-dismiss');
        await reason.press('Enter');
        await expect(dismiss).toHaveCount(0);
        await expect(changesError).not.toBeEmpty();
        await expect(dismissButton).toBeFocused();
        await shot(merge, 'blocker-dismiss-refused');
        // The retry opens with the reason that failed; its success takes
        // the blocker, and its Dismiss, away.
        await press(dismissButton);
        await expect(reason).toHaveValue('Addressed in the retry cap.');
        await expect(changesError).toHaveCount(0);
        await reason.press('Enter');
        await expect(dismiss).toHaveCount(0);
      } else {
        await refuseForgeRoute(harness, { route: 'glab graphql PRBypassChanges', status: 200, times: 1, body: { errors: [{ message: 'You do not have permission to override requested changes.' }] } });
        const bypass = changes.getByTestId('review-merge-row-bypass');
        await shot(merge, 'blocker-bypass');
        await press(bypass);
        await expect(changesError).not.toBeEmpty();
        await expect(bypass).toBeFocused();
        await shot(merge, 'blocker-bypass-refused');
        await press(bypass);
        await expect(changesError).toHaveCount(0);
        await expect(merge.locator('[data-testid="review-merge-settled"][data-kind="changes_requested"]')).toContainText('Requested changes bypassed');
      }
      await expect(changes).toHaveCount(0);
      await expect(mergeHeader).toBeFocused();
      // GitHub still needs the approval its base requires.
      if (forge === 'github') await press(merge.getByTestId('review-merge-approve'));
      const pill = header.getByTestId('review-pr-readiness');
      await expect(pill).toHaveText('Ready to merge');

      // Merge with an edited commit message: what the fake received.
      const trigger = merge.getByTestId('review-merge-trigger');
      await press(trigger);
      await press(page.getByTestId('review-merge-menu').getByTestId('review-merge-edit-message'));
      const message = merge.getByTestId('review-merge-message');
      const messageTitle = message.getByTestId('review-merge-message-title');
      await expect(messageTitle).toBeEnabled();
      await expect(messageTitle).not.toHaveValue('');
      await expect(message.getByTestId('review-merge-message-state')).toHaveText('default');
      await messageTitle.fill(COMMIT_TITLE);
      await message.getByTestId('review-merge-message-body').fill(COMMIT_BODY);
      await expect(message.getByTestId('review-merge-message-state')).toHaveText('edited');
      await shot(merge, 'commit-message');
      const beforeMerge = await lastSeq(harness);
      await press(trigger);
      await press(page.getByTestId('review-merge-menu').getByTestId('review-merge-now'));
      await expect(pill).toHaveText('Merged');
      const mergeCalls = await writes(harness, forge === 'gitlab' ? ['glab api merge'] : ['gh graphql MergePRMessage'], beforeMerge);
      expect(mergeCalls).toHaveLength(1);
      if (forge === 'gitlab') {
        expect((mergeCalls[0].body as Record<string, unknown>).merge_commit_message).toBe(`${COMMIT_TITLE}\n\n${COMMIT_BODY}`);
      } else {
        expect(mergeCalls[0].variables).toMatchObject({ commitHeadline: COMMIT_TITLE, commitBody: COMMIT_BODY });
      }
      // A merged PR has no menu.
      await expect(stateMenu).toHaveCount(0);

      // Each action's event is in the Conversation feed.
      const conversation = await expandReviewSection(page, 'review-pr-conversation');
      const fresh = conversation.getByTestId('review-conversation-new');
      if (await fresh.isVisible()) await press(fresh);
      const kinds = ['closed', 'reopened', 'marked_draft', 'marked_ready', 'review_requested', ...(forge === 'github' ? ['review_dismissed'] : []), 'merged'];
      for (const kind of kinds) {
        await expect(conversation.locator(`[data-testid="review-conversation-event"][data-kind="${kind}"]`).first()).toBeVisible();
      }

      await expectEveryForgeCallHandled(harness);
    });
  }

  test("github: closing the pane from the keyboard writes the open picker's change", async ({ harness, page }) => {
    test.skip(compact, 'the compact layout closes a pane by Back, which closes the picker first');
    test.setTimeout(120_000);
    const thread = 'Manage pane close thread';
    await seedPullRequest(harness, 'github', thread);
    await harness.open(page);
    const review = await openReview(page, thread, 'github', compact);
    await expect(review.getByTestId('review-pr-title')).toContainText(TITLE, { timeout: 30_000 });
    const details = review.getByTestId('review-pr-details');
    await details.locator('button[aria-expanded]').first().click();
    await details.locator('[data-testid="review-details-row"][data-row="reviewers"]').hover();
    await details.getByTestId('review-details-edit-reviewers').click();
    const picker = page.getByTestId('review-picker');
    const search = picker.getByTestId('review-picker-search');
    await expect(search).toBeFocused();
    await search.fill('jane');
    await picker.locator('[data-testid="review-picker-option"][data-key="u:jdoe"]').click();
    await expect(picker.locator('[data-key="u:jdoe"]')).toHaveAttribute('data-checked', 'true');
    const before = await lastSeq(harness);
    await search.focus();
    await page.keyboard.press('Control+w');
    await expect(review).toHaveCount(0);
    await expect(async () => {
      const made = await writes(harness, ['gh api request reviewers', 'gh api remove reviewers'], before);
      expect(made.map((call) => [call.route, call.body])).toEqual([['gh api request reviewers', { reviewers: ['jdoe'], team_reviewers: [] }]]);
    }).toPass({ timeout: 15_000 });
    await expectEveryForgeCallHandled(harness);
  });

  // Every control that hangs off the management flags, wherever it is.
  const MANAGE_CONTROLS = [
    'review-title-edit',
    'review-description-edit',
    'review-pr-state-menu',
    'review-details-edit-reviewers',
    'review-details-edit-assignees',
    'review-details-edit-labels',
    'review-details-rerequest',
    'review-merge-row-dismiss',
    'review-merge-row-bypass',
  ];

  test('gitlab: a refused management read leaves the PR, says why in Details and offers no management control until it answers', async ({ harness, page }, testInfo: TestInfo) => {
    test.setTimeout(180_000);
    const thread = 'Manage unavailable thread';
    await seedPullRequest(harness, 'gitlab', thread);
    const refusal = { status: 403, body: { message: '403 Forbidden' } };
    await refuseForgeRoute(harness, { route: 'glab graphql PRManageChecks', ...refusal });
    await refuseForgeRoute(harness, { route: 'glab graphql PRManage', ...refusal });
    await page.emulateMedia({ colorScheme: 'dark' });
    await harness.open(page);
    const review = await openReview(page, thread, 'gitlab', compact);
    const press = (target: Locator) => (compact ? target.tap() : target.click());

    // The PR is there: its title, the Merge section with its blocker,
    // the description and the conversation.
    await expect(review.getByTestId('review-pr-title')).toContainText(TITLE, { timeout: 30_000 });
    const merge = await expandReviewSection(page, 'review-pr-merge');
    await expect(merge.locator('[data-testid="review-merge-blocker"][data-kind="changes_requested"]')).toHaveCount(1);
    await expect(await expandReviewSection(page, 'review-pr-description')).toContainText(BODY);
    await expect(await expandReviewSection(page, 'review-pr-conversation')).toContainText('One question on the backoff.');

    // Details is collapsed and, opened, holds only the sentence.
    const details = review.getByTestId('review-pr-details');
    await expect(details).toHaveAttribute('data-open', 'false');
    await press(details.locator('button[aria-expanded]').first());
    await expect(details.getByTestId('review-details-unavailable')).not.toBeEmpty();
    await expect(details.getByTestId('review-details-rows')).toHaveCount(0);
    await details.screenshot({ path: testInfo.outputPath(`${testInfo.project.name}-gitlab-details-unavailable.png`) });
    for (const id of MANAGE_CONTROLS) await expect(page.getByTestId(id), id).toHaveCount(0);

    // The read answers again: the controls return with the next read of
    // the PR, which the Conversation's refresh asks for.
    await clearForgeRefusal(harness, 'glab graphql PRManageChecks');
    await clearForgeRefusal(harness, 'glab graphql PRManage');
    await press(review.getByTestId('review-refresh-comments'));
    await expect(details.getByTestId('review-details-unavailable')).toHaveCount(0, { timeout: 30_000 });
    await expect(details.getByTestId('review-details-rows')).toBeVisible();
    for (const id of ['review-title-edit', 'review-description-edit', 'review-pr-state-menu', 'review-details-edit-reviewers', 'review-details-edit-assignees', 'review-details-edit-labels', 'review-merge-row-bypass']) {
      await expect(page.getByTestId(id), id).toBeAttached();
    }
    await expectEveryForgeCallHandled(harness);
  });

  test('gitlab: a host without the set mutations offers no people or label edits, and keeps the rest', async ({ harness, page }, testInfo: TestInfo) => {
    test.setTimeout(180_000);
    const thread = 'Manage older host thread';
    await seedPullRequest(harness, 'gitlab', thread, (seed) => ({ ...seed, serverLacks: ['operationMode'] }));
    await page.emulateMedia({ colorScheme: 'dark' });
    await harness.open(page);
    const review = await openReview(page, thread, 'gitlab', compact);
    await expect(review.getByTestId('review-pr-title')).toContainText(TITLE, { timeout: 30_000 });
    const details = review.getByTestId('review-pr-details');
    await (compact ? details.locator('button[aria-expanded]').first().tap() : details.locator('button[aria-expanded]').first().click());
    await expect(details.getByTestId('review-details-rows')).toBeVisible();
    await expect(details.locator('[data-testid="review-details-reviewer"][data-login="alee"]')).toBeVisible();
    for (const id of ['review-title-edit', 'review-description-edit', 'review-pr-state-menu', 'review-details-rerequest']) {
      await expect(page.getByTestId(id).first(), id).toBeAttached();
    }
    for (const row of ['reviewers', 'assignees', 'labels']) {
      await expect(details.getByTestId(`review-details-edit-${row}`), row).toHaveCount(0);
    }
    await details.screenshot({ path: testInfo.outputPath(`${testInfo.project.name}-gitlab-details-older-host.png`) });
    await expectEveryForgeCallHandled(harness);
  });
}
