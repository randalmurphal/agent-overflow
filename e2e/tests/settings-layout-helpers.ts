import { expect, type Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';

export interface LayoutFault {
  section: string;
  kind: 'panel-overflow' | 'content-clipped' | 'word-broken' | 'control-wraps' | 'input-crushed' | 'command-wraps';
  text: string;
}

const panelSelector = '[role="tabpanel"][id^="settings-panel-"]';

/**
 * Every layout fault the open settings page shows: content wider than the
 * panel or cut off by a box inside it, a word broken mid-word, a labelled
 * control wrapped onto a second line, or a text field squeezed too narrow
 * to read. A break at a hyphen,
 * dot or underscore is a legitimate wrap point; paths and words longer than
 * the column may break anywhere.
 */
async function layoutFaults(page: Page, section: string): Promise<LayoutFault[]> {
  const found = await page.locator(panelSelector).evaluate((root) => {
    const faults: { kind: LayoutFault['kind']; text: string }[] = [];
    if (root.scrollWidth > root.clientWidth + 1) {
      faults.push({ kind: 'panel-overflow', text: `${root.scrollWidth}px content in ${root.clientWidth}px` });
    }
    const shown = (el: Element) => (el as HTMLElement).offsetParent !== null && getComputedStyle(el).visibility !== 'hidden';
    const lineTops = (range: Range) =>
      new Set([...range.getClientRects()].filter((r) => r.width > 0 && r.height > 0).map((r) => Math.round(r.top))).size;

    // A field scrolls its own value and an ellipsis is a deliberate cut.
    for (const el of root.querySelectorAll<HTMLElement>('*')) {
      if (el.scrollWidth <= el.clientWidth + 1 || !shown(el) || el.closest('input, textarea, select, pre, .sr-only')) continue;
      const style = getComputedStyle(el);
      if (style.overflowX === 'visible' || style.textOverflow === 'ellipsis') continue;
      faults.push({
        kind: 'content-clipped',
        text: `${el.tagName.toLowerCase()} "${(el.textContent ?? '').trim().slice(0, 40)}" ${el.scrollWidth}px in ${el.clientWidth}px`,
      });
    }

    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const el = node.parentElement;
      if (!el || !shown(el) || el.closest('pre, textarea, .sr-only')) continue;
      const text = node.textContent ?? '';
      for (const word of text.matchAll(/\S+/g)) {
        if (/[/\\:@]/.test(word[0])) continue;
        let offset = word.index;
        for (const segment of word[0].split(/[-._]/)) {
          if (segment.length >= 3 && segment.length <= 24) {
            const range = document.createRange();
            range.setStart(node, offset);
            range.setEnd(node, offset + segment.length);
            if (lineTops(range) > 1) {
              faults.push({ kind: 'word-broken', text: `"${segment}" in "${(el.textContent ?? '').trim().slice(0, 60)}"` });
            }
          }
          offset += segment.length + 1;
        }
      }
    }

    for (const el of root.querySelectorAll('button, [role="button"], a')) {
      const label = (el.textContent ?? '').trim();
      if (!shown(el) || !label || label.length >= 40) continue;
      const range = document.createRange();
      range.selectNodeContents(el);
      if (lineTops(range) > 1) faults.push({ kind: 'control-wraps', text: label });
    }

    // With the When column shown the table has room for whole commands.
    const whenShown = [...root.querySelectorAll('th')].some((th) => th.textContent?.trim() === 'When' && shown(th));
    for (const cell of whenShown ? root.querySelectorAll('[data-testid^="keybinding-row-"] > td:first-child') : []) {
      const range = document.createRange();
      range.selectNodeContents(cell);
      if (lineTops(range) > 1) faults.push({ kind: 'command-wraps', text: (cell.textContent ?? '').trim() });
    }

    for (const el of root.querySelectorAll<HTMLInputElement | HTMLSelectElement>('input, select')) {
      if (!shown(el)) continue;
      const type = el instanceof HTMLInputElement ? el.type : 'select';
      const min = type === 'number' ? 48 : ['text', 'password', 'search', 'url', 'email', 'select'].includes(type) ? 96 : 0;
      const width = el.getBoundingClientRect().width;
      if (width < min) {
        const name = el.getAttribute('aria-label') ?? (el as HTMLInputElement).placeholder ?? el.tagName;
        faults.push({ kind: 'input-crushed', text: `${name} is ${Math.round(width)}px` });
      }
    }
    return faults;
  });
  return found.map((fault) => ({ section, ...fault }));
}

async function sectionNames(page: Page): Promise<string[]> {
  const tabs = page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab');
  await expect(tabs.first()).toBeVisible();
  return (await tabs.allTextContents()).map((name) => name.trim());
}

async function openSection(page: Page, name: string): Promise<void> {
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name, exact: true }).click();
  await settle(page);
}

async function settle(page: Page): Promise<void> {
  const panel = page.locator(panelSelector);
  await expect(panel).toBeVisible();
  // A page's computer-owned values arrive over the socket after it mounts,
  // so it is measured once its DOM has stopped changing, or after 5s for a
  // page with a live indicator that never stops.
  await panel.evaluate(async (root) => {
    await document.fonts.ready;
    await new Promise<void>((resolve) => {
      let quiet = setTimeout(done, 400);
      const cap = setTimeout(done, 5000);
      const observer = new MutationObserver(() => {
        clearTimeout(quiet);
        quiet = setTimeout(done, 400);
      });
      function done() {
        clearTimeout(quiet);
        clearTimeout(cap);
        observer.disconnect();
        resolve();
      }
      observer.observe(root, { subtree: true, childList: true, characterData: true, attributes: true });
    });
  });
}

/**
 * Opens Settings and measures every section, populated: a project, saved
 * variables on each provider page, and a rebound chord. On compact each
 * section is a page reached from the rail.
 */
export async function settingsLayoutFaults(harness: HarnessApp, page: Page, compact: boolean): Promise<LayoutFault[]> {
  await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'settings-layout', repo: {} }] });
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  const faults: LayoutFault[] = [];
  for (const name of await sectionNames(page)) {
    await openSection(page, name);
    const providerPage = await page.getByTestId('settings-provider-env-name-input').isVisible();
    if (providerPage) {
      await addEnvVars(page);
      await settle(page);
    }
    if (name === 'Keybindings') {
      await rebindFirstCommand(page);
      await settle(page);
    }
    faults.push(...await layoutFaults(page, name));
    if (providerPage) await removeEnvVars(page);
    if (compact) await page.getByTestId('settings-page-back').click();
  }
  return faults;
}

const envVars = [
  { name: 'LAYOUT_DEFAULT_MODEL', value: 'layout-model-name', sensitive: false },
  { name: 'LAYOUT_TOKEN', value: 'secret', sensitive: true },
];

/** Saved variables on the open provider page: one plain, one sensitive. */
async function addEnvVars(page: Page): Promise<void> {
  for (const { name, value, sensitive } of envVars) {
    await page.getByTestId('settings-provider-env-name-input').fill(name);
    await page.getByTestId('settings-provider-env-value-input').fill(value);
    await page.getByTestId('settings-provider-env-sensitive-input').setChecked(sensitive);
    await page.getByTestId('settings-provider-env-add').click();
    await expect(page.getByTestId(`settings-provider-env-row-${name}`)).toBeVisible();
  }
}

/** Provider settings outlive the per-test reset, so the variables go once measured. */
async function removeEnvVars(page: Page): Promise<void> {
  for (const { name } of envVars) {
    await page.getByTestId(`settings-provider-env-remove-${name}`).click();
    await expect(page.getByTestId(`settings-provider-env-row-${name}`)).toHaveCount(0);
  }
}

/** Rebinds the first command to a long chord so the row shows Clear and Restore. */
async function rebindFirstCommand(page: Page): Promise<void> {
  // A rebound row re-sorts, so it is followed by its id, not its position.
  const id = await page.locator('[data-testid^="keybinding-row-"]').first().getAttribute('data-testid');
  const row = page.getByTestId(id!);
  await row.locator('button[data-chord-state]').click();
  await page.keyboard.press('Control+Alt+Shift+F12');
  await expect(row.locator('button[data-chord-state="custom"]')).toBeVisible();
}
