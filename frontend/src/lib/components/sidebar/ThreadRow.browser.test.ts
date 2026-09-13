import { afterEach, beforeEach, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import ThreadRow from './ThreadRow.svelte';
import ThreadGroupRow from './ThreadGroupRow.svelte';
import SidebarSettings from '../settings/SidebarSettings.svelte';
import { resetSidebarForTest, setShowProviderIcons } from '../../stores/sidebar.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { resetSettingsForTest, updateSetting } from '../../stores/settings.svelte';
import { resetKeyboardModifiersForTest, subscribeJumpHints, trackSidebarJumpRows } from '../../stores/keyboardModifiers.svelte';
import { resetKeybindingsStore, setKeybindingsForTest } from '../../stores/keybindings.svelte';
import { isMacPlatform } from '../../utils/platform';
import type { Thread } from '../../types/models';

const thread: Thread = {
  id: 'row-layout', title: 'Investigate model selection and provider settings '.repeat(5),
  provider: 'codex', model: 'gpt-5.6', mode: 'chat', workspacePath: '/repo', projectPath: '/repo',
  createdAt: 0, updatedAt: Date.now(), archived: false, pinnedAt: 1,
};

beforeEach(() => {
  resetSettingsForTest();
  resetSidebarForTest();
  resetKeyboardModifiersForTest();
  resetKeybindingsStore();
  document.documentElement.style.fontSize = '16px';
});

afterEach(() => {
  setCompactLayoutForTest(false);
  resetKeyboardModifiersForTest();
  resetKeybindingsStore();
  document.documentElement.style.removeProperty('font-size');
});

it.each([
  { width: 220, compact: false, fontSize: 16 },
  { width: 320, compact: false, fontSize: 20 },
  { width: 360, compact: true, fontSize: 16 },
])('keeps the row height and trailing controls stable at $width px', async ({ width, compact, fontSize }) => {
  await page.viewport(800, 600);
  setCompactLayoutForTest(compact);
  document.documentElement.style.fontSize = `${fontSize}px`;
  const view = render(ThreadRow, { thread, pane: null });
  view.container.style.width = `${width}px`;
  const row = view.getByTestId('thread-row');
  const title = view.getByTestId('thread-row-title');
  const trailing = view.getByTestId('thread-row-trailing');
  const initial = title.getBoundingClientRect();
  const height = row.getBoundingClientRect().height;
  expect(height).toBe((compact ? 2.25 : 1.5) * fontSize);
  expect(trailing.getBoundingClientRect().left - initial.right).toBeCloseTo(fontSize / 4);
  expect(title.scrollWidth).toBeGreaterThan(title.clientWidth);

  for (let i = 0; i < 2; i++) {
    setShowProviderIcons(true);
    await tick();
    const icon = view.getByTestId('thread-row-provider').getBoundingClientRect();
    const time = view.getByTestId('thread-row-time').getBoundingClientRect();
    expect(icon.height).toBe(12);
    expect(icon.width).toBe(12);
    expect(icon.top).toBeGreaterThanOrEqual(row.getBoundingClientRect().top);
    expect(icon.bottom).toBeLessThanOrEqual(row.getBoundingClientRect().bottom);
    expect(icon.left - title.getBoundingClientRect().right).toBeCloseTo(fontSize / 4);
    expect(time.left - icon.right).toBeGreaterThanOrEqual(fontSize / 4);
    const titleWidth = title.getBoundingClientRect().width;

    await page.getByTestId('thread-row').hover();
    const action = view.getByTestId('thread-row-archive');
    await waitFor(() => expect(getComputedStyle(action.parentElement!).opacity).toBe('1'));
    expect(action.getBoundingClientRect().left).toBeGreaterThanOrEqual(icon.right);
    expect(action.getBoundingClientRect().right).toBeLessThanOrEqual(row.getBoundingClientRect().right);
    expect(title.getBoundingClientRect().width).toBe(titleWidth);
    expect(row.getBoundingClientRect().height).toBe(height);
    await page.getByTestId('thread-row').unhover();

    setShowProviderIcons(false);
    await tick();
    expect(view.queryByTestId('thread-row-provider')).toBeNull();
    expect(title.getBoundingClientRect().width).toBe(initial.width);
    expect(row.getBoundingClientRect().height).toBe(height);
  }
});

it('keeps the provider visible beside wide timestamps and shortcut hints', async () => {
  setShowProviderIcons(true);
  await updateSetting('timestampFormat', '12-hour');
  setKeybindingsForTest([{ key: 'ctrl+alt+2', command: 'thread.jump.1' }]);
  const release = subscribeJumpHints();
  const view = render(ThreadRow, { thread, pane: null });
  view.container.style.width = '260px';
  const tracking = trackSidebarJumpRows(view.container);
  try {
    const icon = view.getByTestId('thread-row-provider');
    expect(view.getByTestId('thread-row-time').getBoundingClientRect().left).toBeGreaterThan(icon.getBoundingClientRect().right);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: isMacPlatform() ? 'Meta' : 'Control', bubbles: true }));
    await waitFor(() => expect(view.queryByTestId('thread-row-jump-hint')).not.toBeNull());
    const hint = view.getByTestId('thread-row-jump-hint').getBoundingClientRect();
    expect(hint.left).toBeGreaterThan(icon.getBoundingClientRect().right);
    expect(hint.right).toBeLessThanOrEqual(view.getByTestId('thread-row').getBoundingClientRect().right);
    expect(view.queryByTestId('thread-row-archive')).toBeNull();
    expect(view.getByTestId('thread-row').getBoundingClientRect().height).toBe(24);
  } finally {
    tracking.destroy();
    release();
  }
});

it('uses the tighter trailing gap on group rows without adding a provider icon', () => {
  setShowProviderIcons(true);
  const view = render(ThreadGroupRow, {
    group: { id: 'group', projectId: 'project', name: 'A long group name '.repeat(10), createdAt: 0, updatedAt: Date.now() },
    pane: null, expanded: false, memberThreadIds: ['one', 'two'],
  });
  view.container.style.width = '240px';
  const title = view.getByTestId('thread-group-row-name').getBoundingClientRect();
  // The count span is the trailing slot itself: it carries the same -ml-0.5
  // and min-w-5 as ThreadRow's trailing slot, so the two row kinds align.
  const slot = view.getByTestId('thread-group-row-count').getBoundingClientRect();
  expect(slot.left - title.right).toBe(4);
  expect(slot.width).toBe(20);
  expect(view.queryByTestId('thread-row-provider')).toBeNull();
});

it('fits the Sidebar settings controls within a compact page', async () => {
  await page.viewport(360, 700);
  setCompactLayoutForTest(true);
  const view = render(SidebarSettings);
  view.container.style.width = '320px';
  for (const control of view.container.querySelectorAll('select, button')) {
    const rect = control.getBoundingClientRect();
    expect(rect.width).toBeGreaterThan(0);
    expect(rect.right).toBeLessThanOrEqual(320);
  }
});
