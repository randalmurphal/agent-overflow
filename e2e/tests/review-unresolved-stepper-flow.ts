// The unresolved-thread stepper and the jump pulse in the PR review pane,
// shared by the desktop and compact projects, through the shipped UI, the
// real backend and the fake forge. The PR carries five unresolved threads
// (four on the diff, one on a file the diff does not touch), a resolved
// and an outdated thread, a PR-level comment and two verdicts. Covered:
// the Conversation section's count chip and the title bar's Conversation
// peek are each one segmented control (one continuous 1px border, the
// segments fill its content box edge to edge, the end segments round to
// the border's inner radius, a divider before each chevron) and stay so
// while a segment is hovered (desktop: the hovered segment tints inside
// the border, the border color and every rect are unchanged); a jump to
// a thread (chip stepper, title-bar stepper onto a diff row, title-bar
// stepper onto a card the diff cannot show, a card's location) pulses the
// destination card once: an accent ring is on it right after the jump and
// gone within the pulse's lifetime, the card's box does not move, a
// repeat jump to the same card restarts the pulse, and under reduced
// motion the ring holds unchanged and then clears.
import { randomBytes } from 'node:crypto';
import type { Locator, Page, TestInfo } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  publishPullRequest,
  type ForgeRepo,
} from './forge-helpers.js';

export interface StepperSurface {
  /** The compact project: taps, a 32px control, no hover. */
  compact: boolean;
}

const TITLE = 'Stepper thread';
const PR_NUMBER = 48;
const BOT = 'coderabbitai[bot]';

// Enough diff below the overview that the overview row scrolls off and
// the title bar grows its peek controls.
function refreshSource(): string {
  const head = [
    "import { decodeToken, type Claims } from './claims';",
    "import { httpPost } from '../net/http';",
    "import type { Session } from './session';",
    '',
    'const REFRESH_PATH = \'/oauth/token\';',
    'const CLOCK_SKEW_MS = 30_000;',
    '',
    'export interface RefreshResult {',
    '  session: Session;',
    '  claims: Claims;',
    '}',
    '',
    'export async function refreshSession(session: Session): Promise<RefreshResult> {',
    '  const claims = decodeToken(session.accessToken);',
    '  if (claims.exp * 1000 - CLOCK_SKEW_MS > Date.now()) {',
    '    return { session, claims };',
    '  }',
    '  const next = await requestRefresh(session.refreshToken);',
    '  return { session: next, claims: decodeToken(next.accessToken) };',
    '}',
    '',
  ];
  const lines = [...head];
  let step = 1;
  while (lines.length < 150) {
    lines.push(
      `async function requestRefresh${step === 1 ? '' : step}(refreshToken: string): Promise<Session> {`,
      `  const response = await httpPost(REFRESH_PATH, { grant_type: 'refresh_token', refresh_token: refreshToken });`,
      '  if (response.status === 401) {',
      `    return requestRefresh${step === 1 ? '' : step}(refreshToken);`,
      '  }',
      '  if (!response.ok) {',
      `    throw new Error(\`refresh failed: \${response.status}\`);`,
      '  }',
      '  const body = (await response.json()) as { access_token: string; refresh_token: string; expires_in: number };',
      '  return {',
      '    accessToken: body.access_token,',
      '    refreshToken: body.refresh_token,',
      '    expiresAt: Date.now() + body.expires_in * 1000,',
      '  };',
      '}',
      '',
    );
    step += 1;
  }
  return `${lines.join('\n')}\n`;
}

const SESSION_SOURCE = [
  'export interface Session {',
  '  accessToken: string;',
  '  refreshToken: string;',
  '  expiresAt: number;',
  '}',
  '',
  'const STORAGE_KEY = \'auth.session\';',
  '',
  'export function loadSession(): Session | null {',
  '  const raw = localStorage.getItem(STORAGE_KEY);',
  '  if (raw === null) return null;',
  '  return JSON.parse(raw) as Session;',
  '}',
  '',
  'export function saveSession(session: Session): void {',
  '  localStorage.setItem(STORAGE_KEY, JSON.stringify(session));',
  '}',
  '',
  'export function sessionExpired(session: Session): boolean {',
  '  return session.expiresAt < Date.now();',
  '}',
  '',
].join('\n');

const DESCRIPTION = [
  '## Summary',
  '',
  'Refreshes the access token ahead of expiry instead of on the first 401, and keeps the refresh token out of the URL.',
  '',
  '- `refreshSession` compares the claim expiry against the local clock with a skew allowance',
  '- `requestRefresh` posts the refresh token in the body',
  '',
  '## Test plan',
  '',
  '- [x] unit tests for the skew window',
  '- [ ] manual: sign in, wait for expiry, keep browsing',
  '',
].join('\n');

const BOT_FINDING = [
  '_⚠️ Potential issue_ | _🔴 Critical_',
  '',
  '**Guard the null token before `decodeToken`.**',
  '',
  'A session restored from storage can carry an empty `accessToken`; `decodeToken` throws on it and the refresh never runs.',
  '',
  '```suggestion',
  '  if (!session.accessToken) return { session: await requestRefresh(session.refreshToken), claims: null };',
  '```',
].join('\n');

export const THREADS = {
  nullToken: { id: 'T_null', text: 'Guard the null token' },
  retry: { id: 'T_retry', text: 'loop forever' },
  clock: { id: 'T_clock', text: 'client clocks skew' },
  session: { id: 'T_session', text: 'compare as a number' },
  legacy: { id: 'T_legacy', text: 'legacy refresher' },
} as const;

function repo(): ForgeRepo {
  return {
    forge: 'github',
    project: `ao-e2e/stepper-${randomBytes(4).toString('hex')}`,
    pulls: [
      {
        number: PR_NUMBER,
        title: 'Harden the token refresh path',
        body: DESCRIPTION,
        author: 'mkaplan',
        authorName: 'Maya Kaplan',
        comments: [
          { author: 'mkaplan', authorName: 'Maya Kaplan', body: 'Split the clock fix into its own commit so it can be cherry-picked.', createdAt: '2026-04-01T14:00:00Z' },
        ],
        threads: [
          {
            id: THREADS.nullToken.id,
            path: 'src/auth/refresh.ts',
            line: 14,
            comments: [{ author: BOT, body: BOT_FINDING, createdAt: '2026-04-02T09:40:00Z' }],
          },
          {
            id: THREADS.retry.id,
            path: 'src/auth/refresh.ts',
            line: 25,
            comments: [{ author: 'priya-n', authorName: 'Priya Natarajan', body: 'Retrying on a 401 will loop forever once the refresh token itself is revoked. Cap it, or surface the sign-out.', createdAt: '2026-04-02T09:10:00Z' }],
          },
          {
            id: THREADS.clock.id,
            path: 'src/auth/refresh.ts',
            line: 15,
            comments: [{ author: 'jrow', authorName: 'Jonah Rowe', body: 'Use the server `Date` header here; client clocks skew by more than 30s on the kiosks.', createdAt: '2026-04-01T17:20:00Z' }],
          },
          {
            id: THREADS.session.id,
            path: 'src/auth/session.ts',
            line: 20,
            comments: [{ author: 'priya-n', authorName: 'Priya Natarajan', body: '`expiresAt` is a string on the wire; compare as a number or this is always false.', createdAt: '2026-04-01T16:05:00Z' }],
          },
          {
            id: THREADS.legacy.id,
            path: 'src/auth/legacy.ts',
            line: 3,
            comments: [{ author: 'jrow', authorName: 'Jonah Rowe', body: 'This still imports the legacy refresher; drop it in this PR?', createdAt: '2026-04-01T15:30:00Z' }],
          },
          {
            id: 'T_docs',
            path: 'docs/auth.md',
            line: 3,
            resolved: true,
            comments: [
              { author: 'priya-n', authorName: 'Priya Natarajan', body: 'Mention the skew window here.', createdAt: '2026-04-01T11:00:00Z' },
              { author: 'mkaplan', authorName: 'Maya Kaplan', body: 'Added.', createdAt: '2026-04-01T11:20:00Z' },
            ],
          },
          {
            id: 'T_old',
            path: 'src/auth/refresh.ts',
            line: 140,
            outdated: true,
            comments: [{ author: 'jrow', authorName: 'Jonah Rowe', body: 'Dead branch after the early return above.', createdAt: '2026-03-31T18:00:00Z' }],
          },
        ],
        reviews: [
          { author: 'priya-n', authorName: 'Priya Natarajan', state: 'CHANGES_REQUESTED', body: 'Blocking on the retry loop.', submittedAt: '2026-04-02T09:15:00Z' },
          { author: 'jrow', authorName: 'Jonah Rowe', state: 'COMMENTED', submittedAt: '2026-04-01T17:30:00Z' },
        ],
      },
    ],
  };
}

async function seedPullRequest(harness: HarnessApp): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `stepper-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(harness, seed.projects[0].path, repo(), {
    'src/auth/refresh.ts': refreshSource(),
    'src/auth/session.ts': SESSION_SOURCE,
    'docs/auth.md': '# Auth\n\nTokens refresh thirty seconds before expiry.\n',
  });
}

/** The review pane on the PR, opened the way each layout offers it. */
async function openReview(page: Page, compact: boolean): Promise<Locator> {
  const row = page.getByTestId('thread-row').filter({ hasText: TITLE });
  if (!compact) {
    await row.click();
    const badge = page.getByTestId('chat-header-pr-badge');
    await expect(badge).toBeVisible({ timeout: 30_000 });
    await badge.click();
  } else {
    await row.tap();
    // The PR row appears once git status has looked the branch's PR up.
    const more = page.getByTestId('chat-header-more');
    const menu = page.locator('[data-popover]');
    const prRow = menu.getByRole('menuitem', { name: `PR #${PR_NUMBER}` });
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
  // The pane slides into place (the compact pane strip): geometry is
  // read once it has stopped, whole inside the viewport.
  await expect.poll(() => review.evaluate((el) => {
    const r = el.getBoundingClientRect();
    return r.left >= 0 && r.right <= window.innerWidth ? `${r.left}` : 'moving';
  })).not.toBe('moving');
  const left = await review.evaluate((el) => el.getBoundingClientRect().left);
  await page.waitForTimeout(100);
  expect(await review.evaluate((el) => el.getBoundingClientRect().left)).toBe(left);
  return review;
}

interface Rect { left: number; top: number; right: number; bottom: number }

interface SegmentGeometry {
  rect: Rect;
  /** A button segment tints on hover; a text segment does not. */
  interactive: boolean;
  radius: string;
  borderLeft: string;
  background: string;
}

interface ControlGeometry {
  rect: Rect;
  viewportWidth: number;
  border: string[];
  borderColor: string;
  radius: string;
  segments: SegmentGeometry[];
}

function geometryOf(control: Locator): Promise<ControlGeometry> {
  return control.evaluate((el) => {
    const rect = (node: Element): Rect => {
      const r = node.getBoundingClientRect();
      return { left: r.left, top: r.top, right: r.right, bottom: r.bottom };
    };
    const cs = getComputedStyle(el);
    return {
      rect: rect(el),
      viewportWidth: window.innerWidth,
      border: [cs.borderTopWidth, cs.borderRightWidth, cs.borderBottomWidth, cs.borderLeftWidth],
      borderColor: cs.borderTopColor,
      radius: cs.borderRadius,
      segments: [...el.children].map((child) => {
        const s = getComputedStyle(child);
        return { rect: rect(child), interactive: child.tagName === 'BUTTON', radius: s.borderRadius, borderLeft: s.borderLeftWidth, background: s.backgroundColor };
      }),
    };
  });
}

const TRANSPARENT = 'rgba(0, 0, 0, 0)';

/**
 * One segmented control, whole on screen: a 1px border all round, the
 * segments fill the content box exactly (no gap, no overflow onto the
 * border), adjacent segments share an edge, the end segments round to the
 * inner radius and every other segment is square, and each chevron
 * carries its divider.
 */
function expectSegmented(geometry: ControlGeometry, height: number): void {
  const { rect, segments } = geometry;
  expect(rect.left).toBeGreaterThanOrEqual(0);
  expect(rect.right).toBeLessThanOrEqual(geometry.viewportWidth);
  expect(geometry.border).toEqual(['1px', '1px', '1px', '1px']);
  expect(geometry.radius).toBe('6px');
  expect(rect.bottom - rect.top).toBeCloseTo(height, 0);
  expect(segments.length).toBe(3);
  const inner = { left: rect.left + 1, top: rect.top + 1, right: rect.right - 1, bottom: rect.bottom - 1 };
  segments.forEach((segment, index) => {
    expect(segment.rect.top, `segment ${index} top`).toBeCloseTo(inner.top, 0);
    expect(segment.rect.bottom, `segment ${index} bottom`).toBeCloseTo(inner.bottom, 0);
    const previous = segments[index - 1];
    expect(segment.rect.left, `segment ${index} left`).toBeCloseTo(previous ? previous.rect.right : inner.left, 0);
  });
  expect(segments[2].rect.right).toBeCloseTo(inner.right, 0);
  expect(segments[0].radius).toBe('5px 0px 0px 5px');
  expect(segments[1].radius).toBe('0px');
  expect(segments[2].radius).toBe('0px 5px 5px 0px');
  expect(segments[1].borderLeft).toBe('1px');
  expect(segments[2].borderLeft).toBe('1px');
}

/** Hover each segment in turn: only it tints (a text label does not),
 * nothing moves, the border keeps its color. */
async function expectHoverStaysInside(
  control: Locator,
  rest: ControlGeometry,
  shot: (index: number) => Promise<void>,
): Promise<void> {
  for (let index = 0; index < 3; index += 1) {
    await control.locator(':scope > *').nth(index).hover();
    if (rest.segments[index].interactive) {
      await expect.poll(async () => (await geometryOf(control)).segments[index].background).not.toBe(TRANSPARENT);
    }
    await shot(index);
    const hovered = await geometryOf(control);
    expect(hovered.rect).toEqual(rest.rect);
    expect(hovered.borderColor).toBe(rest.borderColor);
    hovered.segments.forEach((segment, other) => {
      expect(segment.rect, `segment ${other} rect while ${index} hovered`).toEqual(rest.segments[other].rect);
      if (other !== index) expect(segment.background, `segment ${other} while ${index} hovered`).toBe(TRANSPARENT);
    });
  }
}

interface Pulse {
  running: number;
  boxShadow: string;
  /** The first animation's elapsed ms. */
  time: number | null;
}

function pulseOf(card: Locator): Promise<Pulse> {
  return card.evaluate((el) => {
    const animations = el.getAnimations();
    const first = animations[0];
    return {
      running: animations.length,
      boxShadow: getComputedStyle(el).boxShadow,
      time: first ? Number(first.currentTime) : null,
    };
  });
}

/** The pulse is on the card: one animation, an accent ring painted. */
async function expectPulsing(card: Locator): Promise<Pulse> {
  await expect.poll(async () => (await pulseOf(card)).running, { timeout: 3_000 }).toBe(1);
  const pulse = await pulseOf(card);
  expect(pulse.boxShadow).not.toBe('none');
  return pulse;
}

/** The pulse has ended on its own: no animation, no ring. */
async function expectPulseGone(card: Locator): Promise<void> {
  await expect.poll(async () => await pulseOf(card), { timeout: 3_000 }).toEqual({ running: 0, boxShadow: 'none', time: null });
}

/** Scroll the diff list until the overview row is above the viewport. */
async function scrollPastOverview(review: Locator): Promise<void> {
  const scroller = review.getByTestId('review-scroll');
  await expect
    .poll(async () => {
      await scroller.evaluate((el) => { el.scrollTop += 600; });
      return review.getByTestId('review-overview-peek').isVisible();
    })
    .toBe(true);
}

/** The card (the bordered bubble beside the avatar) of a thread article. */
function cardOf(article: Locator): Locator {
  return article.locator(':scope > div').nth(1);
}

export function unresolvedStepperFlow({ compact }: StepperSurface): void {
  const height = compact ? 32 : 24;

  test('the unresolved stepper is one segmented control and every jump pulses its card once', async ({ harness, page }, testInfo) => {
    await seedPullRequest(harness);
    await harness.open(page);
    const review = await openReview(page, compact);
    const press = (target: Locator) => (compact ? target.tap() : target.click());
    const shot = (name: string) => page.screenshot({ path: testInfo.outputPath(`${name}.png`) });
    const headerShot = async (name: string) => {
      const box = (await review.getByTestId('review-pr-header').boundingBox())!;
      await page.screenshot({ path: testInfo.outputPath(`${name}.png`), clip: { x: box.x, y: box.y, width: box.width, height: box.height } });
    };
    const chipShot = async (name: string) => {
      const box = (await review.getByTestId('review-pr-conversation').locator('[data-testid="review-conversation-open-count"]').boundingBox())!;
      await page.screenshot({ path: testInfo.outputPath(`${name}.png`), clip: { x: box.x - 160, y: box.y - 12, width: box.width + 180, height: box.height + 24 } });
    };
    const card = (id: string) => cardOf(review.locator(`[data-testid="review-conversation-thread"][data-thread-id="${id}"]`));
    const row = (text: string) => review.locator('[data-testid="review-pr-thread"]').filter({ hasText: text });

    // The Conversation section's count chip is a segmented control.
    await press(review.getByTestId('review-pr-conversation').locator('button[aria-expanded]').first());
    await expect(review.getByTestId('review-pr-conversation')).toHaveAttribute('data-open', 'true');
    const chip = review.getByTestId('review-conversation-open-count');
    await expect(chip).toContainText('5 unresolved');
    await chipShot(`${testInfo.project.name}-chip-rest`);
    const chipRest = await geometryOf(chip);
    if (!compact) {
      await expectHoverStaysInside(chip, chipRest, (index) => chipShot(`${testInfo.project.name}-chip-hover-${['label', 'up', 'down'][index]}`));
      await page.mouse.move(0, 0);
    }
    expectSegmented(chipRest, height);

    // The chip's stepper lands on the newest open thread's card, which
    // pulses once and settles without moving.
    await press(chip.getByRole('button', { name: 'Next unresolved thread' }));
    const nullCard = card(THREADS.nullToken.id);
    await expect(nullCard).toBeInViewport();
    await expectPulsing(nullCard);
    const during = (await nullCard.boundingBox())!;
    await shot(`${testInfo.project.name}-pulse-mid`);
    await expectPulseGone(nullCard);
    expect(await nullCard.boundingBox()).toEqual(during);
    await shot(`${testInfo.project.name}-pulse-after`);

    // Reduced motion: the ring holds, unchanged, then clears.
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await press(chip.getByRole('button', { name: 'Next unresolved thread' }));
    const retryCard = card(THREADS.retry.id);
    await expect(retryCard).toBeInViewport();
    const held = await expectPulsing(retryCard);
    await page.waitForTimeout(400);
    const later = await pulseOf(retryCard);
    expect(later.running).toBe(1);
    expect(later.boxShadow).toBe(held.boxShadow);
    await expectPulseGone(retryCard);
    await page.emulateMedia({ reducedMotion: 'no-preference' });

    // A card's location jumps to its diff row; the same jump again, mid
    // pulse, starts the pulse over.
    const nullRow = row(THREADS.nullToken.text);
    await press(card(THREADS.nullToken.id).getByTestId('review-conversation-jump-diff'));
    await expect(nullRow).toBeInViewport();
    const first = await expectPulsing(cardOf(nullRow));
    await page.waitForTimeout(500);
    const elapsed = (await pulseOf(cardOf(nullRow))).time ?? 0;
    expect(elapsed).toBeGreaterThan(first.time ?? 0);
    // Back to the card through the list, not through the tap's own
    // scroll-into-view: that centers the target in every scrollable
    // ancestor, the compact pane strip included.
    await review.getByTestId('review-scroll').evaluate((el) => { el.scrollTop = 0; });
    await expect(card(THREADS.nullToken.id).getByTestId('review-conversation-jump-diff')).toBeInViewport();
    await press(card(THREADS.nullToken.id).getByTestId('review-conversation-jump-diff'));
    await expect(nullRow).toBeInViewport();
    await expect.poll(async () => {
      const pulse = await pulseOf(cardOf(nullRow));
      return pulse.running === 1 && pulse.time !== null && pulse.time < elapsed;
    }).toBe(true);
    await expectPulseGone(cardOf(nullRow));

    // The title bar's Conversation peek joins the count and the stepper
    // into one control.
    await scrollPastOverview(review);
    const bar = review.getByTestId('review-unresolved-stepper-bar');
    await expect(bar.getByTestId('review-peek-conversation')).toContainText('5 unresolved');
    await headerShot(`${testInfo.project.name}-titlebar-rest`);
    const barRest = await geometryOf(bar);
    if (!compact) {
      await expectHoverStaysInside(bar, barRest, (index) => headerShot(`${testInfo.project.name}-titlebar-hover-${['label', 'up', 'down'][index]}`));
      await page.mouse.move(0, 0);
    }
    expectSegmented(barRest, height);

    // From the title bar, a thread on the diff pulses its row's card ...
    await press(bar.getByRole('button', { name: 'Next unresolved thread' }));
    const clockRow = row(THREADS.clock.text);
    await expect(clockRow).toBeInViewport();
    await expectPulsing(cardOf(clockRow));
    await shot(`${testInfo.project.name}-row-pulse-mid`);
    await expectPulseGone(cardOf(clockRow));
    await press(bar.getByRole('button', { name: 'Next unresolved thread' }));
    const sessionRow = row(THREADS.session.text);
    await expect(sessionRow).toBeInViewport();
    await expectPulsing(cardOf(sessionRow));

    // ... and a thread the diff cannot show brings the overview back with
    // its card pulsing, then settled.
    // The overview row is unmounted this deep in the diff, so the jump
    // scrolls a feed that mounts with it.
    await expect(review.getByTestId('review-overview')).toHaveCount(0);
    await press(bar.getByRole('button', { name: 'Next unresolved thread' }));
    const legacyCard = card(THREADS.legacy.id);
    await expect(legacyCard).toBeInViewport();
    await expectPulsing(legacyCard);
    await expectPulseGone(legacyCard);
    await shot(`${testInfo.project.name}-card-settled`);

    await expectEveryForgeCallHandled(harness);
  });
}
