// The update controls sit in the Setup header's badge row. Where the row
// holds them beside the pill, appearing there or switching to the running
// state moves nothing below the header. Where it does not, they wrap under
// the pill: nothing overlaps and nothing leaves the page's width.
import { afterEach, beforeEach, expect, it } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import ClaudeSettings from './ClaudeSettings.svelte';
import { seedSettingsPages } from '../../../test/helpers/settingsPages';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { resetProviderUpdatesForTest } from '../../stores/providerUpdates.svelte';
import { listenProviderUpdates } from '../../../test/helpers/providerUpdates';

function status(overrides: Record<string, unknown> = {}) {
  return {
    provider: 'claude', installedVersion: '2.1.321', latestVersion: '2.1.321', available: false, checkedAt: 1, run: { phase: 'idle' },
    ...overrides,
  };
}

let host: HTMLDivElement;
beforeEach(async () => {
  await seedSettingsPages();
  setBindingMock('GetProviderStatuses', async () => [
    { provider: 'claude', installed: true, version: '2.1.321 (Claude Code)', binaryPath: '/home/user/.local/bin/claude', status: 'ready', message: '' },
  ]);
  setBindingMock('CheckProviderUpdates', async () => [status()]);
  listenProviderUpdates();
  host = document.createElement('div');
  document.body.append(host);
});
afterEach(() => {
  cleanup();
  host.remove();
  resetProviderUpdatesForTest();
  setCompactLayoutForTest(false);
});

async function mount(width: number) {
  setCompactLayoutForTest(width < 600);
  host.style.cssText = `width:${Math.min(width, 768)}px;padding:0 16px;box-sizing:border-box;background:var(--color-surface-0);color:var(--color-fg)`;
  const view = render(ClaudeSettings, { target: host });
  const pill = view.getByTestId('settings-provider-status-pill');
  await waitFor(() => expect(pill.textContent).toContain('2.1.321 (Claude Code)'));
  return { view, pill };
}

function overlaps(a: DOMRect, b: DOMRect): boolean {
  return a.left < b.right - 0.5 && b.left < a.right - 0.5 && a.top < b.bottom - 0.5 && b.top < a.bottom - 0.5;
}

it('moves nothing below the header when the update controls appear on a wide page', async () => {
  const { view, pill } = await mount(1280);
  const below = host.querySelector<HTMLElement>('#claude-path')!;
  const before = below.getBoundingClientRect().top;

  emitWailsEvent('provider:updates', [status({ latestVersion: '2.1.325', available: true })]);
  const button = await view.findByTestId('settings-provider-update-button');
  const notes = view.getByTestId('settings-provider-update-notes-button');
  expect(below.getBoundingClientRect().top).toBe(before);
  // One line: the controls sit beside the pill, not under it.
  for (const control of [button, notes]) {
    expect(Math.abs(control.getBoundingClientRect().top - pill.getBoundingClientRect().top)).toBeLessThan(6);
  }

  emitWailsEvent('provider:updates', [status({ latestVersion: '2.1.325', available: true, run: { phase: 'running' } })]);
  await waitFor(() => expect(button.textContent).toContain('Updating…'));
  expect(below.getBoundingClientRect().top).toBe(before);
});

it.each([320, 360])('wraps the update controls without overlap at %ipx', async (width) => {
  const { view, pill } = await mount(width);
  emitWailsEvent('provider:updates', [status({ latestVersion: '2.1.325', available: true, checkError: '' })]);
  const button = await view.findByTestId('settings-provider-update-button');
  const notes = view.getByTestId('settings-provider-update-notes-button');
  const title = [...host.querySelectorAll('h3')].find((h) => h.textContent === 'Setup')!;
  const description = title.closest('header')!.querySelector('p')!;
  const bounds = host.getBoundingClientRect();
  const parts = [title, pill, button, notes, description];
  const rects = parts.map((part) => part.getBoundingClientRect());
  for (const rect of rects) {
    expect(rect.width).toBeGreaterThan(0);
    expect(rect.left).toBeGreaterThanOrEqual(bounds.left);
    expect(rect.right).toBeLessThanOrEqual(bounds.right);
  }
  for (let i = 0; i < rects.length; i++) {
    for (let j = i + 1; j < rects.length; j++) {
      expect(overlaps(rects[i], rects[j]), `${parts[i].textContent?.trim()} over ${parts[j].textContent?.trim()}`).toBe(false);
    }
  }
  // Nothing is cut to make room: the controls wrap before the pill's
  // version truncates, and every control shows its whole text.
  const version = pill.querySelector<HTMLElement>('.font-mono')!;
  for (const text of [version, button, notes]) expect(text.scrollWidth).toBeLessThanOrEqual(text.clientWidth + 1);
  expect(host.scrollWidth).toBeLessThanOrEqual(host.clientWidth + 1);
});
