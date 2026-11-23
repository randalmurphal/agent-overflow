// Where a Codex card's "Usage resets (N)" button sits, on the provider's
// Accounts page and in the account switcher. The button lives in a box at the
// end of the card's last limits line (ProviderAccountLimits lineEnd) that is
// there before the card has any reset, so a card that is handed its first
// list later moves nothing. Each case mounts a card with no resets, then
// hands it a list (once usable resets, once an unconfirmed one) and checks:
//
// - nothing on the card moves and the card keeps its height;
// - no text is newly cut off, by the box or by the button;
// - the button's text and icon overlap no text or bar, and its tap area
//   overlaps no other control and stays inside the card;
// - on compact the tap area is at least 36px tall, and what it adds below
//   the label falls in the card's bottom padding;
// - the box reserves no height where it fits at the end of a line, and
//   only what the tap area needs on compact; everything above the last line
//   is where it is without the box (the switcher's top line included);
// - as the countdowns run through every form they take, the button and
//   everything but the countdowns' own text stay where they are.
//
// The space each case reserves is logged as RESERVED lines and summed in a
// RESERVED-SUMMARY line. The last tests check what belongs to the card
// rather than to the limits under it: an open menu and confirmation, and
// where Tab takes focus from the menu.
import { afterAll, afterEach, beforeAll, beforeEach, expect, it } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import SettingsView from './SettingsView.svelte';
import AccountSwitcher from '../accounts/AccountSwitcher.svelte';
import { seedSettingsPages } from '../../../test/helpers/settingsPages';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { hideSettingsRail, resetSettingsOverlayForTest } from '../../stores/settingsOverlay.svelte';
import { loadProviderAccounts, resetForTest as resetProviderAccountsForTest } from '../../stores/providerAccounts.svelte';
import { resetForTest as resetRateLimitsForTest, setProviderRateLimits } from '../../stores/rateLimitsInfo.svelte';
import { applyFonts } from '../../utils/fonts';
import { applyFontScale } from '../../utils/zoom';
import type { SansFont } from '../../types/settings';

const DAY_MS = 24 * 3600_000;
// The compact tap target: at least 36px, and 2.25rem like every compact
// target in the app, so it grows with the text size.
const COMPACT_TARGET_PX = 36;
const compactTarget = () => Math.max(COMPACT_TARGET_PX, 2.25 * parseFloat(getComputedStyle(document.documentElement).fontSize));
const FONTS: SansFont[] = ['geist', 'hack-nerd', 'system'];
const TEXT_SIZES = [13, 16, 20];
const WIDTHS = [320, 380, 1024, 1280];
const LONG_EMAIL = 'morgan.alvarez-kowalski@northwind-engineering.test';
const RESETS = { usageResets: { revision: 1, options: [{ id: 'a', kind: 'full', label: 'Full reset', refills: '', expiresAt: 0, usable: true }] } };
const UNSETTLED = { ...RESETS, pendingReset: { id: 'spend-1', label: 'Full reset', refills: '', startedAt: 1_800_000_000_000 } };
const FORMS = [['usable resets', RESETS], ['an unconfirmed reset', UNSETTLED]] as const;

type Usage = 'countdowns' | 'no countdown';
interface CardState { usage?: Usage; [field: string]: unknown }

const STATES: [string, CardState][] = [
  ['login gone', { needsLogin: true }],
  ['login gone, long email', { needsLogin: true, email: LONG_EMAIL }],
  ['signed in, long email', { email: LONG_EMAIL }],
  ['inactive with usage', { email: LONG_EMAIL, active: false, usage: 'countdowns' }],
  ['about to expire', { refreshTokenExpiresAt: Date.now() + 2 * DAY_MS }],
  ['active with usage', { usage: 'countdowns' }],
];

function account(overrides: Record<string, unknown>) {
  return {
    id: 'acct-1', provider: 'codex', email: 'mock@agent-overflow.test', orgName: 'e2e-workspace', addedAt: 1,
    active: true, generation: 1, needsLogin: false, signInRequired: false, autoSwitch: false, ...overrides,
  };
}

const WINDOWS = [
  { limitId: 'primary', limitName: '', usedPercent: 42, windowMins: 300 },
  { limitId: 'secondary', limitName: '', usedPercent: 7, windowMins: 10080 },
];

/** Both windows, each resetting `seconds` from now or, past its length, just inside it. */
function usageAt(provider: string, id: string, seconds: number | null) {
  setProviderRateLimits({
    provider, accountId: id, updatedAt: Date.now(),
    limits: WINDOWS.map((window) => ({
      ...window,
      resetsAt: seconds === null ? 0 : Date.now() / 1000 + Math.min(seconds, window.windowMins * 60 - 60),
    })),
  });
}

function usageFor(provider: string, id: string, usage: Usage) {
  usageAt(provider, id, usage === 'countdowns' ? DAY_MS / 1000 : null);
}

// Every form formatResetCountdown gives.
const COUNTDOWN_FORMS: [string, number][] = [
  ['under a minute', 30],
  ['minutes', 30 * 60],
  ['hours and minutes', 3 * 3600 + 30 * 60],
  ['days and hours', 2 * 86400 + 5 * 3600],
];

beforeAll(async () => {
  await import('../../utils/hack-nerd.css');
  for (const face of ['400 1em "Hack Nerd Font"', '700 1em "Hack Nerd Font"']) {
    if ((await document.fonts.load(face, 'Auto-switch')).length === 0) throw new Error(`${face} did not load`);
  }
});

let host: HTMLDivElement;
function freshHost(): void {
  host?.remove();
  host = document.createElement('div');
  document.body.append(host);
}
beforeEach(async () => {
  resetProviderAccountsForTest();
  await seedSettingsPages();
  freshHost();
});
afterEach(() => {
  cleanup();
  host.remove();
  resetProviderAccountsForTest();
  resetRateLimitsForTest();
  resetSettingsOverlayForTest();
  setCompactLayoutForTest(false);
  applyFonts('geist', 'geist');
  applyFontScale(13);
});

const frames = () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
const round = (v: number) => Math.round(v * 2) / 2;

function intersects(a: DOMRect, b: DOMRect): boolean {
  return Math.min(a.right, b.right) - Math.max(a.left, b.left) > 0.5 && Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 0.5;
}

function shownTextRects(root: Element, skip: (node: Node) => boolean): [string, DOMRect][] {
  const out: [string, DOMRect][] = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const parent = node.parentElement;
    if (!parent || skip(node) || getComputedStyle(parent).visibility !== 'visible') continue;
    const range = document.createRange();
    range.selectNodeContents(node);
    for (const r of range.getClientRects()) if (r.width > 0 && r.height > 0) out.push([`text "${node.textContent?.trim().slice(0, 24)}"`, r]);
  }
  return out;
}

function describeEl(el: Element): string {
  return `${el.tagName.toLowerCase()} ${el.getAttribute('aria-label') ?? el.getAttribute('data-testid') ?? el.textContent?.trim().slice(0, 32)}`;
}

/** Numbers every element on the card, so later reads compare the same nodes. */
function tag(card: HTMLElement): void {
  [card, ...card.querySelectorAll<HTMLElement>('*')].forEach((el, i) => {
    el.dataset.measure = String(i);
  });
}

interface Snapshot {
  height: number;
  /** Each numbered element's box from the card's corner. */
  boxes: Record<string, string>;
  /** Elements, the card's own included, whose content is cut off. */
  cut: string[];
}

function keyOf(el: HTMLElement): string {
  return `${el.dataset.measure} ${el.tagName.toLowerCase()} ${el.dataset.testid ?? (el.getAttribute('class') ?? '').slice(0, 48)}`;
}

function isCut(el: HTMLElement): boolean {
  const style = getComputedStyle(el);
  const clipsX = style.overflowX !== 'visible' || style.textOverflow === 'ellipsis';
  const clipsY = style.overflowY !== 'visible' || style.webkitLineClamp !== 'none';
  return (clipsX && el.scrollWidth > el.clientWidth + 1) || (clipsY && el.scrollHeight > el.clientHeight + 1);
}

/**
 * Whether an element shows anything of its own: text, an icon, a control, or
 * a fill or border (a bar). A layout box that only holds others can stretch
 * with its grid row without anything on screen moving.
 */
function paints(el: HTMLElement): boolean {
  if (el.matches('svg, img, input, button, label, select, textarea, [role="switch"]')) return true;
  if ([...el.childNodes].some((n) => n.nodeType === Node.TEXT_NODE && n.textContent?.trim())) return true;
  const style = getComputedStyle(el);
  return style.backgroundColor !== 'rgba(0, 0, 0, 0)' || parseFloat(style.borderTopWidth) > 0 || parseFloat(style.borderLeftWidth) > 0;
}

/**
 * Where everything that shows sits, but what `skip` names; what is cut off
 * covers every element.
 */
function snapshot(card: HTMLElement, skip: (el: Element) => boolean = () => false): Snapshot {
  const origin = card.getBoundingClientRect();
  const boxes: Record<string, string> = {};
  const all = [card, ...card.querySelectorAll<HTMLElement>('[data-measure]')];
  for (const el of all) {
    if (el === card || skip(el) || !paints(el)) continue;
    const r = el.getBoundingClientRect();
    boxes[keyOf(el)] = [r.left - origin.left, r.top - origin.top, r.width, r.height].map(round).join(',');
  }
  return { height: round(origin.height), boxes, cut: all.filter(isCut).map(keyOf) };
}

/** What the button's text and icon overlap, and what its tap area overlaps. */
function overlaps(card: HTMLElement, trigger: HTMLElement): string[] {
  const shown: DOMRect[] = [
    ...shownTextRects(trigger, () => false).map(([, r]) => r),
    ...[...trigger.querySelectorAll('svg')].map((s) => s.getBoundingClientRect()),
  ];
  const out: string[] = [];
  const others = shownTextRects(card, (node) => trigger.contains(node));
  for (const bar of card.querySelectorAll('.h-1.rounded-full')) others.push(['bar', bar.getBoundingClientRect()]);
  for (const [name, rect] of others) if (shown.some((r) => intersects(r, rect))) out.push(`over ${name}`);
  const target = trigger.getBoundingClientRect();
  for (const control of card.querySelectorAll<HTMLElement>('button, input, label, a[href], select, textarea, [role="button"], [role="switch"], [tabindex]')) {
    if (control === trigger || trigger.contains(control) || control.contains(trigger) || (control.tabIndex < 0 && control.tagName !== 'BUTTON')) continue;
    if (intersects(target, control.getBoundingClientRect())) out.push(`tap area over ${describeEl(control)}`);
  }
  return out;
}

interface Reserved { where: string; label: string; width: number; size: number; font: string; px: number; fits: boolean }
const reservedLog: Reserved[] = [];

afterAll(() => {
  if (reservedLog.length === 0) return;
  const inMatrix = (r: Reserved) => STATES.some(([label]) => label === r.label) && WIDTHS.includes(r.width);
  const line = (name: string, rows: Reserved[]) => {
    const px = rows.map((r) => r.px);
    const reserving = px.filter((v) => v > 0.5);
    const range = reserving.length ? `, ${Math.min(...reserving)}-${Math.max(...reserving)}px` : '';
    return `${name}: ${reserving.length} of ${rows.length} cases reserve${range}`;
  };
  const groups = new Map<string, Reserved[]>();
  for (const r of reservedLog) {
    const key = `${inMatrix(r) ? 'matrix' : 'extra'} ${r.where} ${r.width < 600 ? 'compact' : 'desktop'} ${r.fits ? 'at the line end' : 'on a line of its own'} (${r.label})`;
    groups.set(key, [...(groups.get(key) ?? []), r]);
  }
  console.log([
    `RESERVED-SUMMARY|${line('matrix', reservedLog.filter(inMatrix))}|${line('extra', reservedLog.filter((r) => !inMatrix(r)))}`,
    ...[...groups].sort(([a], [b]) => a.localeCompare(b)).map(([key, rows]) => `RESERVED|${line(key, rows)}`),
  ].join('\n'));
});

const card = () => document.querySelector<HTMLElement>('[data-measure="0"]');

async function checkCase(
  where: 'settings' | 'switcher',
  label: string,
  state: CardState,
  width: number,
  size: number,
  font: SansFont,
  mount: (width: number) => Promise<HTMLElement>,
): Promise<void> {
  const { usage, ...rest } = state;
  if (usage) usageFor('codex', 'acct-1', usage);
  const compact = width < 600;
  for (const [form, extra] of FORMS) {
    const what = `${where}, ${label}, ${width}px, text size ${size}, ${font}, ${form}`;
    let listing = [account(rest)];
    setBindingMock('ListProviderAccounts', async () => listing);
    setBindingMock('RefreshProviderAccountUsage', () => new Promise<void>(() => {}));
    applyFonts(font, 'geist');
    applyFontScale(size);
    setCompactLayoutForTest(compact);
    const mounted = await mount(width);
    await document.fonts.ready;
    await frames();
    tag(mounted);
    const box = mounted.querySelector<HTMLElement>('[data-testid="provider-account-resets-box-acct-1"]');
    expect(box, `${what}: the box is there before any reset`).not.toBeNull();
    // The box and what holds it: everything else is compared with the card
    // as it would be with no box, what nothing reserved looks like.
    const boxOrHolder = (el: Element) => box!.contains(el) || el.contains(box!);
    box!.style.display = 'none';
    await frames();
    const bare = snapshot(mounted, boxOrHolder);
    box!.style.display = '';
    await frames();
    const empty = snapshot(mounted);
    expect(empty.cut, `${what}: text the box cuts off`).toEqual(bare.cut);
    expect(snapshot(mounted, boxOrHolder).boxes, `${what}: what the box moves`).toEqual(bare.boxes);

    listing = [account({ ...rest, ...extra })];
    await loadProviderAccounts();
    const trigger = await waitFor(() => {
      const el = mounted.querySelector<HTMLElement>('[data-testid="provider-account-resets-acct-1"]');
      if (!el) throw new Error('no button');
      return el;
    });
    await frames();
    expect(card(), `${what}: the card stays mounted`).toBe(mounted);
    const after = snapshot(mounted, (el) => trigger.contains(el));
    expect(after.boxes, `${what}: what moved`).toEqual(empty.boxes);
    expect(after.height, `${what}: the card's height`).toBe(empty.height);
    expect(after.cut, `${what}: text cut off`).toEqual(empty.cut);
    expect(overlaps(mounted, trigger), `${what}: what the button covers`).toEqual([]);

    const c = mounted.getBoundingClientRect();
    const t = trigger.getBoundingClientRect();
    const rect = (r: DOMRect) => [r.left, r.top, r.right, r.bottom].map(round).join(',');
    expect(t.left >= c.left - 0.5 && t.right <= c.right + 0.5 && t.top >= c.top - 0.5 && t.bottom <= c.bottom + 0.5, `${what}: the tap area ${rect(t)} inside the card ${rect(c)}`).toBe(true);
    const b = box!.getBoundingClientRect();
    // The label is whole, inside its box.
    const labelRects = [...shownTextRects(trigger, () => false).map(([, r]) => r), ...[...trigger.querySelectorAll('svg')].map((s) => s.getBoundingClientRect())];
    expect(labelRects.length, `${what}: the label shows`).toBeGreaterThan(0);
    for (const r of labelRects) {
      expect(r.left >= b.left - 0.5 && r.right <= b.right + 0.5 && r.top >= b.top - 0.5 && r.bottom <= b.bottom + 0.5, `${what}: the label inside its box`).toBe(true);
    }
    if (compact) {
      expect(round(t.height), `${what}: the tap area's height`).toBeGreaterThanOrEqual(round(compactTarget()) - 0.5);
      // Below the box the tap area takes only the card's bottom padding.
      const cardPad = parseFloat(getComputedStyle(mounted).paddingBottom);
      expect(t.bottom - b.bottom, `${what}: the tap area below the box`).toBeLessThanOrEqual(cardPad + 0.5);
    }

    // The space the box reserves: none where it fits at a line's end off
    // compact; on compact only what reaches below the line's text; and its
    // own line where it does not fit. The box is no taller than its label,
    // or on compact than the tap area needs above the card's padding.
    const line = mounted.querySelector('[data-limits-last-line]')!;
    const text = line.querySelector('p')?.getBoundingClientRect();
    const fits = !!text && b.top < text.bottom - 0.5;
    const px = round(empty.height - bare.height);
    const allowed = fits ? Math.max(0, b.bottom - text!.bottom) : b.bottom - (text?.bottom ?? line.getBoundingClientRect().top);
    const geometry = `box ${round(b.top)}-${round(b.bottom)}, text ${text ? `${round(text.top)}-${round(text.bottom)}` : 'none'}`;
    expect(px, `${what}: px reserved (${geometry})`).toBeLessThanOrEqual(round(allowed) + 0.5);
    if (fits && !compact) expect(px, `${what}: px reserved beside the line's text (${geometry})`).toBe(0);
    const sizer = box!.querySelector('[aria-hidden="true"]')!.getBoundingClientRect();
    const padBottom = parseFloat(getComputedStyle(mounted).paddingBottom);
    expect(round(b.height), `${what}: the box's height`).toBeLessThanOrEqual(round(Math.max(sizer.height, compact ? compactTarget() - padBottom : 0)) + 0.5);
    if (form === FORMS[0][0]) {
      reservedLog.push({ where, label, width, size, font, px, fits });
    }

    // The countdowns run through every form; nothing but their own text moves.
    if (usage === 'countdowns' && WIDTHS.includes(width)) {
      const skipCountdown = (el: Element) => trigger.contains(el) || !!el.closest('[data-countdown]');
      const relative = () => {
        const r = trigger.getBoundingClientRect();
        const o = mounted.getBoundingClientRect();
        return [r.left - o.left, r.top - o.top, r.width, r.height].map(round).join(',');
      };
      const settled = snapshot(mounted, skipCountdown);
      const button = relative();
      for (const [countdownForm, seconds] of COUNTDOWN_FORMS) {
        usageAt('codex', 'acct-1', seconds);
        await frames();
        const now = snapshot(mounted, skipCountdown);
        expect(now.boxes, `${what}, countdowns ${countdownForm}: what moved`).toEqual(settled.boxes);
        expect(now.height, `${what}, countdowns ${countdownForm}: the card's height`).toBe(settled.height);
        expect(relative(), `${what}, countdowns ${countdownForm}: the button`).toBe(button);
      }
      usageFor('codex', 'acct-1', usage);
    }

    cleanup();
    resetProviderAccountsForTest();
    freshHost();
  }
}

// The page and the viewport are the same width, as in the app: the limits'
// two columns follow the viewport.
async function mountSettings(width: number, provider: 'codex' | 'claude' = 'codex'): Promise<HTMLElement> {
  await page.viewport(width, 800);
  hideSettingsRail();
  host.style.cssText = `width:${width}px;height:800px`;
  const view = render(SettingsView, { target: host, props: { onClose() {}, initialSection: provider } });
  return view.findByTestId('provider-account-acct-1');
}

async function mountSwitcher(width: number): Promise<HTMLElement> {
  await page.viewport(width, 800);
  const view = render(AccountSwitcher, { open: true, onClose() {} });
  return view.findByTestId('account-switcher-row-acct-1');
}

function cases(states: [string, CardState][], widths: number[], sizes: number[], fonts: (width: number) => SansFont[]) {
  return states.flatMap(([label, state]) =>
    widths.flatMap((width) => sizes.flatMap((size) => fonts(width).map((font) => [label, state, width, size, font] as const))),
  );
}

// 144 per screen: every state at two phone and two desktop widths, every
// text size, and on a phone every interface font.
const CASES = cases(STATES, WIDTHS, TEXT_SIZES, (width) => (width < 600 ? FONTS : ['geist']));

it.each(CASES)('settings: %s', async (label, state, width, size, font) => {
  await checkCase('settings', label, state, width, size, font, mountSettings);
});

it.each(CASES)('switcher: %s', async (label, state, width, size, font) => {
  await checkCase('switcher', label, state, width, size, font, mountSwitcher);
});

// A last limit with no countdown leaves no text on the line: the box takes
// a line of its own, with and without resets.
const NO_COUNTDOWN = cases([['usage without a countdown', { usage: 'no countdown' }]], [320, 1280], [13, 20], () => ['geist']);
// The narrowest viewport with two limit columns, at the largest text size,
// in every font: the box beside the last column's countdown.
const NARROW_COLUMNS = cases(
  [['active with usage', { usage: 'countdowns' }], ['usage without a countdown', { usage: 'no countdown' }]],
  [640],
  [20],
  () => FONTS,
);
it.each(NARROW_COLUMNS)('settings, two narrow columns: %s', async (label, state, width, size, font) => {
  await checkCase('settings', label, state, width, size, font, mountSettings);
});
it.each(NARROW_COLUMNS)('switcher, two narrow columns: %s', async (label, state, width, size, font) => {
  await checkCase('switcher', label, state, width, size, font, mountSwitcher);
});
it.each(NO_COUNTDOWN)('settings: %s', async (label, state, width, size, font) => {
  await checkCase('settings', label, state, width, size, font, mountSettings);
});
it.each(NO_COUNTDOWN)('switcher: %s', async (label, state, width, size, font) => {
  await checkCase('switcher', label, state, width, size, font, mountSwitcher);
});

it('gives Claude cards no box, on the Accounts page and in the switcher', async () => {
  const claude = { ...account({ provider: 'claude' }), ...RESETS };
  setBindingMock('ListProviderAccounts', async () => [claude]);
  setBindingMock('RefreshProviderAccountUsage', () => new Promise<void>(() => {}));
  usageFor('claude', 'acct-1', 'countdowns');
  const settingsCard = await mountSettings(1280, 'claude');
  expect(settingsCard.querySelector('[data-limits-last-line]')).toBeNull();
  expect(settingsCard.querySelector('[data-testid^="provider-account-resets"]')).toBeNull();
  cleanup();
  freshHost();
  const row = await mountSwitcher(1280);
  expect(row.querySelector('[data-limits-last-line]')).toBeNull();
  expect(row.querySelector('[data-testid^="provider-account-resets"]')).toBeNull();
});

const triggerOf = (mounted: HTMLElement) => waitFor(() => {
  const el = mounted.querySelector<HTMLElement>('[data-testid="provider-account-resets-acct-1"]');
  if (!el) throw new Error('no button');
  return el;
});
const resetsMenu = () => document.querySelector<HTMLElement>('[data-testid="provider-account-resets-list-acct-1"]');
const confirmDialog = () => [...document.querySelectorAll<HTMLElement>('[role="dialog"]')].find((el) => el.textContent?.includes('Use a Full reset?'));

async function mountCard(where: 'settings' | 'switcher'): Promise<HTMLElement> {
  setBindingMock('ListProviderAccounts', async () => [account(RESETS)]);
  setBindingMock('RefreshProviderAccountUsage', () => new Promise<void>(() => {}));
  setBindingMock('SpendProviderAccountReset', async () => ({ message: 'Reset used.', spent: true, readingLanded: true }));
  return where === 'settings' ? mountSettings(1280) : mountSwitcher(1280);
}

// What the person has open belongs to the card: limits appearing, changing
// and going away under an open menu or confirmation leave it open, and the
// confirmation still spends what it showed.
it.each(['settings', 'switcher'] as const)('%s: an open menu and confirmation outlive the limits under them', async (where) => {
  const mounted = await mountCard(where);
  const trigger = await triggerOf(mounted);
  trigger.click();
  const menu = await waitFor(() => resetsMenu() ?? Promise.reject(new Error('no menu')));
  for (const usage of ['countdowns', 'no countdown', null] as const) {
    if (usage === null) setProviderRateLimits({ provider: 'codex', accountId: 'acct-1', updatedAt: Date.now(), limits: [] });
    else usageFor('codex', 'acct-1', usage);
    await frames();
    expect(resetsMenu(), `the menu with limits ${usage ?? 'gone'}`).toBe(menu);
    expect(mounted.querySelector('[data-testid="provider-account-resets-acct-1"]'), 'the button').toBe(trigger);
  }
  menu.querySelector<HTMLElement>('[data-testid="provider-account-reset-use"]')!.click();
  const dialog = await waitFor(() => confirmDialog() ?? Promise.reject(new Error('no confirm')));
  for (const usage of ['countdowns', null, 'countdowns'] as const) {
    if (usage === null) setProviderRateLimits({ provider: 'codex', accountId: 'acct-1', updatedAt: Date.now(), limits: [] });
    else usageFor('codex', 'acct-1', usage);
    await frames();
    expect(confirmDialog(), `the confirmation with limits ${usage ?? 'gone'}`).toBe(dialog);
  }
  [...dialog.querySelectorAll('button')].find((el) => el.textContent?.trim() === 'Use reset')!.click();
  await waitFor(() => expect(getBindingMock('SpendProviderAccountReset')).toHaveBeenCalledWith('codex', 'acct-1', 'a', 1));
});

// The menu is portaled out of the card, so Tab from it would walk out of a
// modal around the card (the switcher); Tab and Shift+Tab close it onto its
// button instead.
it.each(['settings', 'switcher'] as const)('%s: Tab and Shift+Tab from the menu close it onto its button', async (where) => {
  const mounted = await mountCard(where);
  const trigger = await triggerOf(mounted);
  if (where === 'switcher') {
    // The switcher focuses its list as it opens.
    await waitFor(() => expect(document.activeElement?.getAttribute('data-testid')).toBe('account-switcher'));
  }
  for (const key of ['{Tab}', '{Shift>}{Tab}{/Shift}']) {
    trigger.focus();
    await userEvent.keyboard('{Enter}');
    await waitFor(() => expect(resetsMenu()?.contains(document.activeElement)).toBe(true));
    await userEvent.keyboard(key);
    await waitFor(() => expect(resetsMenu(), key).toBeNull());
    expect(document.activeElement, key).toBe(trigger);
  }
});
