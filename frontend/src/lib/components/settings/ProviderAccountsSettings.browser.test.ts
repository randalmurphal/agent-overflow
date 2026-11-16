// An account card lays out inside the real settings page at every interface
// text size and font the settings offer. On a phone the name takes the
// card's width and the controls a row beneath it, wrapping within the card
// with each label whole; on desktop the controls sit beside the name.
import { afterEach, beforeAll, beforeEach, expect, it } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import SettingsView from './SettingsView.svelte';
import { seedSettingsPages } from '../../../test/helpers/settingsPages';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { hideSettingsRail, resetSettingsOverlayForTest } from '../../stores/settingsOverlay.svelte';
import { resetForTest as resetProviderAccountsForTest } from '../../stores/providerAccounts.svelte';
import { resetForTest as resetRateLimitsForTest, setProviderRateLimits } from '../../stores/rateLimitsInfo.svelte';
import { applyFonts } from '../../utils/fonts';
import { applyFontScale } from '../../utils/zoom';
import type { SansFont } from '../../types/settings';

const DAY_MS = 24 * 3600_000;
const DEFAULT_TEXT_SIZE = 13;
// Every interface font the Typography page offers, and its text sizes:
// the default, a common enlargement and the largest.
const FONTS: SansFont[] = ['geist', 'hack-nerd', 'system'];
const TEXT_SIZES = [DEFAULT_TEXT_SIZE, 16, 20];

function account(overrides: Record<string, unknown> = {}) {
  return {
    id: 'acct-1',
    provider: 'codex',
    email: 'mock@agent-overflow.test',
    orgName: 'e2e-workspace',
    addedAt: 1,
    active: true,
    generation: 1,
    needsLogin: false,
    signInRequired: false,
    autoSwitch: false,
    ...overrides,
  };
}

beforeAll(async () => {
  // Hack Nerd Font loads on selection; load it up front so a measurement
  // never sees its fallback.
  await import('../../utils/hack-nerd.css');
  for (const face of ['400 1em "Hack Nerd Font"', '700 1em "Hack Nerd Font"']) {
    if ((await document.fonts.load(face, 'Auto-switch')).length === 0) throw new Error(`${face} did not load`);
  }
});

let host: HTMLDivElement;
beforeEach(async () => {
  resetProviderAccountsForTest();
  await seedSettingsPages();
  host = document.createElement('div');
  document.body.append(host);
});
afterEach(() => {
  cleanup();
  host.remove();
  resetProviderAccountsForTest();
  resetRateLimitsForTest();
  resetSettingsOverlayForTest();
  setCompactLayoutForTest(false);
  applyFonts('geist', 'geist');
  applyFontScale(DEFAULT_TEXT_SIZE);
});

/** The lines a node's characters sit on. */
function lineCount(node: Node): number {
  const range = document.createRange();
  range.selectNodeContents(node);
  return new Set([...range.getClientRects()].filter((r) => r.width > 0 && r.height > 0).map((r) => Math.round(r.top))).size;
}

function textRects(el: Element): DOMRect[] {
  const rects: DOMRect[] = [];
  const texts = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  for (let node = texts.nextNode(); node; node = texts.nextNode()) {
    const range = document.createRange();
    range.selectNodeContents(node);
    rects.push(...[...range.getClientRects()].filter((r) => r.width > 0 && r.height > 0));
  }
  return rects;
}

/** The card's box inside its border and padding. */
function contentBox(card: HTMLElement) {
  const box = card.getBoundingClientRect();
  const style = getComputedStyle(card);
  const left = box.left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft);
  const right = box.right - parseFloat(style.borderRightWidth) - parseFloat(style.paddingRight);
  return { left, right, width: right - left };
}

// An email is how a person tells two saved logins apart.
const LONG_EMAIL = 'morgan.alvarez-kowalski@northwind-engineering.test';
interface AccountState {
  rows: Record<string, unknown>[];
  /** The first card's usage refresh is in flight. */
  refreshing?: boolean;
}
// Each state lists its cards in order; every card is `acct-<position>`.
const ACCOUNT_STATES: [string, AccountState][] = [
  ['a Codex login that is gone', { rows: [{ needsLogin: true }] }],
  ['a Codex login with a long email that is gone', { rows: [{ needsLogin: true, email: LONG_EMAIL }] }],
  ['a signed-in Codex account with a long email', { rows: [{ email: LONG_EMAIL }] }],
  ['an inactive Codex account with usage shown', { rows: [{ email: LONG_EMAIL, active: false, usage: true }] }],
  ['a Codex login about to expire', { rows: [{ refreshTokenExpiresAt: Date.now() + 2 * DAY_MS }] }],
  ['a Codex account refreshing its usage', { rows: [{ email: LONG_EMAIL }], refreshing: true }],
  [
    'two Claude accounts, one inactive and gone',
    {
      rows: [
        { provider: 'claude', email: LONG_EMAIL, orgName: 'Northwind', usage: true },
        { provider: 'claude', active: false, needsLogin: true, email: 'sam@northwind.test' },
      ],
    },
  ],
];

/**
 * Opens the provider's settings page on a `screenWidth` screen, as the app
 * does: the real panel padding and column around the accounts.
 */
async function renderAccounts(screenWidth: number, state: AccountState, textSize: number, font: SansFont) {
  const provider = (state.rows[0].provider as 'codex' | 'claude' | undefined) ?? 'codex';
  const accounts = state.rows.map(({ usage, ...overrides }, index) => {
    const id = `acct-${index + 1}`;
    if (usage) {
      setProviderRateLimits({
        provider,
        accountId: id,
        updatedAt: Date.now(),
        limits: [
          { limitId: 'primary', limitName: '', usedPercent: 42, windowMins: 300, resetsAt: Date.now() / 1000 + 3600 },
          { limitId: 'secondary', limitName: '', usedPercent: 7, windowMins: 10080, resetsAt: Date.now() / 1000 + DAY_MS / 1000 },
        ],
      });
    }
    return account({ id, provider, ...overrides });
  });
  setBindingMock('ListProviderAccounts', async () => accounts);
  setBindingMock('RefreshProviderAccountUsage', () => new Promise<void>(() => {}));
  applyFonts(font, 'geist');
  applyFontScale(textSize);
  setCompactLayoutForTest(screenWidth < 600);
  hideSettingsRail();
  host.style.cssText = `width:${screenWidth}px;height:800px;background:var(--color-surface-0);color:var(--color-fg)`;
  const view = render(SettingsView, { target: host, props: { onClose() {}, initialSection: provider } });
  await view.findByTestId(`provider-account-acct-${state.rows.length}`);
  if (state.refreshing) {
    view.getByRole('button', { name: `Refresh usage for ${LONG_EMAIL}` }).click();
    await waitFor(() => expect(view.getByTestId('provider-account-acct-1').querySelector('.animate-spin')).toBeTruthy());
  }
  await document.fonts.ready;
  return view;
}

/** The controls row's lines: its children grouped where they overlap vertically. */
function controlLines(controls: HTMLElement): { top: number; bottom: number; right: number }[] {
  const rects = [...controls.children].map((el) => el.getBoundingClientRect()).sort((a, b) => a.top - b.top);
  const lines: { top: number; bottom: number; right: number }[] = [];
  for (const rect of rects) {
    const line = lines.at(-1);
    if (line && rect.top < line.bottom) {
      line.bottom = Math.max(line.bottom, rect.bottom);
      line.right = Math.max(line.right, rect.right);
    } else {
      lines.push({ top: rect.top, bottom: rect.bottom, right: rect.right });
    }
  }
  return lines;
}

/** "Active" is never broken: the name beside it gives way instead. */
function expectActiveWhole(card: HTMLElement, active: boolean): HTMLElement | undefined {
  const pill = [...card.querySelectorAll('span')].find((el) => el.textContent?.trim() === 'Active');
  expect(!!pill).toBe(active);
  if (pill) expect(lineCount(pill)).toBe(1);
  return pill;
}

function overlapsVertically(a: DOMRect, b: DOMRect): boolean {
  return a.top < b.bottom && a.bottom > b.top;
}

/**
 * The page's sign-in button lies inside the accounts column with its label;
 * `wraps` says whether the label may take more than one line.
 */
function expectLoginButtonFits(view: Awaited<ReturnType<typeof renderAccounts>>, provider: string, wraps: boolean): DOMRect {
  const column = view.getByTestId(`provider-accounts-${provider}`).getBoundingClientRect();
  const button = view.getByRole('button', { name: 'Log in to another account' });
  const rect = button.getBoundingClientRect();
  expect(rect.left, 'sign-in button left').toBeGreaterThanOrEqual(column.left - 0.5);
  expect(rect.right, 'sign-in button right').toBeLessThanOrEqual(column.right + 0.5);
  for (const text of textRects(button)) {
    expect(text.left, 'sign-in label left').toBeGreaterThanOrEqual(rect.left - 0.5);
    expect(text.right, 'sign-in label right').toBeLessThanOrEqual(rect.right + 0.5);
  }
  if (!wraps) expect(new Set(textRects(button).map((r) => Math.round(r.top))).size, 'sign-in label lines').toBe(1);
  return rect;
}

it.each(
  ACCOUNT_STATES.flatMap(([label, state]) =>
    [320, 380].flatMap((width) =>
      TEXT_SIZES.flatMap((size) => FONTS.map((font) => [label, width, size, font, state] as const)),
    ),
  ),
)('fits %s on a %ipx phone at text size %i in %s', async (_label, width, size, font, state) => {
  const view = await renderAccounts(width, state, size, font);
  for (let index = 0; index < state.rows.length; index++) {
    const id = `acct-${index + 1}`;
    const row = state.rows[index];
    const card = view.getByTestId(`provider-account-${id}`);
    const box = contentBox(card);
    const name = view.getByTestId(`provider-account-name-${id}`);
    const controls = view.getByTestId(`provider-account-controls-${id}`);
    expect(card.textContent?.includes('Usage not checked yet.')).toBe(!row.usage);
    const pill = expectActiveWhole(card, row.active !== false);

    // Its column runs to the card's edge, so the explanation lines below it
    // get the same width, and the controls sit on their own row beneath.
    const column = name.closest('button')!;
    expect(column.getBoundingClientRect().right).toBeCloseTo(box.right, 0);
    // A name is shown whole or keeps at least 6em of text, the layout
    // spec's bar for a cut text; at the default text size that is at least
    // half the card.
    const em = parseFloat(getComputedStyle(name).fontSize);
    expect(name.clientWidth, 'name width').toBeGreaterThanOrEqual(Math.min(name.scrollWidth, 6 * em) - 0.5);
    if (size === DEFAULT_TEXT_SIZE) {
      expect(name.clientWidth).toBeGreaterThanOrEqual(Math.min(name.scrollWidth, box.width / 2));
    }
    // "Active" sits beside the name unless that would leave the name under
    // 6em; then it drops below, in line with the name's text. At the default
    // text size it always fits beside.
    const nameBox = name.getBoundingClientRect();
    const pillBox = pill?.getBoundingClientRect();
    const pillBeside = !!pillBox && overlapsVertically(pillBox, nameBox);
    if (pillBox && !pillBeside) {
      const gap = parseFloat(getComputedStyle(pill!.parentElement!).columnGap);
      expect(box.right - nameBox.left - gap - pillBox.width, 'room beside the pill').toBeLessThan(6 * em);
      expect(pillBox.top).toBeGreaterThanOrEqual(nameBox.bottom - 0.5);
      expect(pillBox.left).toBeCloseTo(nameBox.left, 0);
      expect(pillBox.right).toBeLessThanOrEqual(box.right + 0.5);
    }
    if (pillBox && size === DEFAULT_TEXT_SIZE) expect(pillBeside, 'pill beside the name').toBe(true);
    // A name that is cut takes all its line has left.
    if (name.scrollWidth > name.clientWidth + 1) {
      expect(pillBeside ? pillBox!.right : nameBox.right).toBeCloseTo(box.right, 0);
    }
    expect(controls.getBoundingClientRect().top).toBeGreaterThanOrEqual(name.getBoundingClientRect().bottom);

    // Every control, and every letter of its label, lies inside the card's
    // content box, each label on one line; each line of controls ends at
    // the card's right edge.
    const items = [...controls.querySelectorAll<HTMLElement>('button, label, input')];
    expect(items.length).toBeGreaterThanOrEqual(4);
    for (const item of items) {
      const rect = item.getBoundingClientRect();
      const what = `${item.tagName.toLowerCase()} "${item.textContent?.trim() || item.getAttribute('aria-label')}"`;
      expect(rect.left, `${what} left`).toBeGreaterThanOrEqual(box.left - 0.5);
      expect(rect.right, `${what} right`).toBeLessThanOrEqual(box.right + 0.5);
      for (const text of textRects(item)) {
        expect(text.left, `${what} text left`).toBeGreaterThanOrEqual(Math.max(box.left, rect.left) - 0.5);
        expect(text.right, `${what} text right`).toBeLessThanOrEqual(Math.min(box.right, rect.right) + 0.5);
      }
      expect(new Set(textRects(item).map((r) => Math.round(r.top))).size, `${what} lines`).toBeLessThanOrEqual(1);
    }
    for (const line of controlLines(controls)) expect(line.right).toBeCloseTo(box.right, 0);
    // Refresh and remove wrap as a pair, so neither sits alone on a line.
    const refresh = controls.querySelector('[aria-label^="Refresh usage for "]')!.getBoundingClientRect();
    const remove = controls.querySelector('[aria-label^="Remove "]')!.getBoundingClientRect();
    expect(remove.top).toBeCloseTo(refresh.top, 0);
    expect(card.scrollWidth).toBeLessThanOrEqual(card.clientWidth + 1);
  }
  // The page's sign-in button stays inside the column; its label wraps only
  // when one line cannot fit, which never happens at the default size.
  const provider = (state.rows[0].provider as string | undefined) ?? 'codex';
  expectLoginButtonFits(view, provider, size !== DEFAULT_TEXT_SIZE);
});

// Desktop keeps one row: the name column on the left, the controls beside it
// at the top right.
it.each(ACCOUNT_STATES.flatMap(([label, state]) => [1024, 1280].map((width) => [label, width, state] as const)))(
  'keeps the controls beside the name for %s at %ipx',
  async (_label, width, state) => {
    const view = await renderAccounts(width, state, DEFAULT_TEXT_SIZE, 'geist');
    for (let index = 0; index < state.rows.length; index++) {
      const id = `acct-${index + 1}`;
      const card = view.getByTestId(`provider-account-${id}`);
      const name = view.getByTestId(`provider-account-name-${id}`).getBoundingClientRect();
      const controls = view.getByTestId(`provider-account-controls-${id}`).getBoundingClientRect();
      const cardBox = card.getBoundingClientRect();
      const padding = parseFloat(getComputedStyle(card).paddingTop);
      const pill = expectActiveWhole(card, state.rows[index].active !== false);
      if (pill) expect(overlapsVertically(pill.getBoundingClientRect(), name)).toBe(true);
      expect(controls.top).toBeCloseTo(cardBox.top + padding + 1, 0);
      expect(controls.left).toBeGreaterThanOrEqual(name.right);
      expect(cardBox.right - controls.right).toBeCloseTo(parseFloat(getComputedStyle(card).paddingRight) + 1, 0);
    }
    // The sign-in button keeps its one-line width beside the section title.
    const provider = (state.rows[0].provider as string | undefined) ?? 'codex';
    const login = expectLoginButtonFits(view, provider, false);
    const title = view.getByRole('heading', { name: 'Accounts' }).getBoundingClientRect();
    expect(overlapsVertically(login, title)).toBe(true);
  },
);
