import { afterEach, beforeEach, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import ProjectThreadList from './ProjectThreadList.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest } from '../../stores/panes.svelte';
import { getCollapsedGroups, isDiscussionExpanded, resetSidebarForTest, toggleDiscussion } from '../../stores/sidebar.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { touchThreadActivity } from '../../stores/threads.svelte';
import { projectTurnStarted } from '../../stores/threadStatuses.svelte';
import { appStorageGet, resetAppStorageForTest } from '../../stores/appStorage';
import type { Thread, ThreadGroup } from '../../types/models';

const group: ThreadGroup = { id: 'group', projectId: 'project', name: 'Work', createdAt: 0, updatedAt: 0 };
function thread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id, title: id, provider: 'claude', mode: 'chat', model: 'test', projectId: 'project',
    projectPath: '/tmp/ws', workspacePath: '/tmp/ws', createdAt: 0, updatedAt: 0,
    archived: false, ...overrides,
  };
}

beforeEach(() => {
  resetAppStorageForTest();
  resetSidebarForTest();
});
afterEach(() => setCompactLayoutForTest(false));

function row(container: HTMLElement, id: string): HTMLElement {
  const element = container.querySelector<HTMLElement>(`[data-sidebar-thread-id="${id}"]`);
  if (!element) throw new Error(`Missing sidebar thread ${id}`);
  return element;
}

async function settled(container: HTMLElement): Promise<void> {
  await tick();
  await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
  await waitFor(() => expect(container.getAnimations({ subtree: true }).filter(a =>
    a.playState === 'running' && a.effect?.getComputedTiming().endTime !== Infinity,
  )).toHaveLength(0));
}

async function sampleFocusedRow(container: HTMLElement, id: string, duration: number): Promise<void> {
  const initial = row(container, id);
  const end = performance.now() + duration;
  do {
    await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
    const active = row(container, id);
    expect(active).toBe(initial);
    expect(active.getBoundingClientRect().height).toBeGreaterThan(0);
    for (let ancestor: HTMLElement | null = active; ancestor && ancestor !== container; ancestor = ancestor.parentElement) {
      expect(Number(getComputedStyle(ancestor).opacity)).toBe(1);
    }
  } while (performance.now() < end);
}

it.each([false, true])('keeps the focused member painted while collapsing and changing focus (compact=%s)', async compact => {
  await page.viewport(compact ? 390 : 800, 700);
  setCompactLayoutForTest(compact);
  const threads = [
    thread('alpha', { groupId: group.id, updatedAt: 3 }),
    thread('beta', { groupId: group.id, updatedAt: 2 }),
    thread('loose'),
  ];
  const pane = createThreadPane();
  const other = createThreadPane();
  registerPaneForTest('main', pane);
  registerPaneForTest('other', other);
  pane.replaceThread(threads[0]);
  other.replaceThread(threads[1]);
  const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [group], pane });
  view.container.style.width = '360px';
  await settled(view.container);

  view.getByTestId('thread-group-row-expand').click();
  await tick();
  await sampleFocusedRow(view.container, 'alpha', 350);
  expect(view.getByTestId('thread-group-row').getAttribute('data-expanded')).toBe('false');
  expect(view.getByTestId('thread-group-row-count').textContent?.trim()).toBe('2');
  expect([...getCollapsedGroups()]).toEqual(['group']);
  expect(JSON.parse(appStorageGet('sidebar:collapsedGroups')!)).toEqual(['group']);
  expect(view.container.querySelector('[data-sidebar-thread-id="beta"]')).toBeNull();
  const activeRect = row(view.container, 'alpha').getBoundingClientRect();
  expect(Math.abs(row(view.container, 'loose').getBoundingClientRect().top - activeRect.bottom)).toBeLessThanOrEqual(2);

  // Stream-driven tree rebuilds cannot override the saved collapse choice.
  projectTurnStarted('alpha', 'turn', 0, 0);
  touchThreadActivity('beta', 100);
  await settled(view.container);
  expect([...getCollapsedGroups()]).toEqual(['group']);
  expect(view.container.querySelectorAll('[data-group-member]')).toHaveLength(1);

  for (const focused of [other, pane, null, other]) {
    await view.rerender({ pane: focused });
    await settled(view.container);
    const members = [...view.container.querySelectorAll('[data-group-member] [data-sidebar-thread-id]')];
    expect(members.map(el => el.getAttribute('data-sidebar-thread-id'))).toEqual(focused ? [focused.threadId] : []);
    expect([...getCollapsedGroups()]).toEqual(['group']);
  }

  for (let repeat = 0; repeat < 2; repeat++) {
    view.getByTestId('thread-group-row-expand').click();
    await settled(view.container);
    expect(view.container.querySelectorAll('[data-group-member]')).toHaveLength(2);
    view.getByTestId('thread-group-row-expand').click();
    await settled(view.container);
    expect(view.container.querySelectorAll('[data-group-member]')).toHaveLength(1);
  }

  // Reverse an expansion before its entrance animation completes.
  view.getByTestId('thread-group-row-expand').click();
  await tick();
  await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
  view.getByTestId('thread-group-row-expand').click();
  await tick();
  await sampleFocusedRow(view.container, 'beta', 350);
  expect(view.container.querySelectorAll('[data-group-member]')).toHaveLength(1);
  expect([...getCollapsedGroups()]).toEqual(['group']);
});

it.each([false, true])('keeps a focused discussion child visible without reopening its ancestors (grouped=%s)', async grouped => {
  const groupId = grouped ? group.id : undefined;
  const threads = [
    thread('parent', { groupId }),
    thread('child', { groupId, parentThreadId: 'parent' }),
    thread('sibling', { groupId }),
  ];
  const pane = createThreadPane();
  registerPaneForTest('main', pane);
  pane.replaceThread(threads[1]);
  toggleDiscussion('parent');
  const view = render(ProjectThreadList, { projectId: 'project', threads, groups: grouped ? [group] : [], pane });
  await settled(view.container);
  row(view.container, 'parent').querySelector<HTMLButtonElement>('[data-testid="thread-row-expand"]')!.click();
  await tick();
  await sampleFocusedRow(view.container, 'child', 350);
  expect(isDiscussionExpanded('parent')).toBe(false);
  expect(row(view.container, 'child')).toBeTruthy();

  pane.replaceThread(threads[2]);
  await settled(view.container);
  expect(view.container.querySelector('[data-sidebar-thread-id="child"]')).toBeNull();
  pane.replaceThread(threads[1]);
  await settled(view.container);
  expect(isDiscussionExpanded('parent')).toBe(false);
  expect(view.container.querySelectorAll('[data-sidebar-thread-id="child"]')).toHaveLength(1);
  if (!grouped) return;

  view.getByTestId('thread-group-row-expand').click();
  await tick();
  await sampleFocusedRow(view.container, 'child', 350);
  expect(view.container.querySelectorAll('[data-sidebar-thread-id]')).toHaveLength(1);
  expect(row(view.container, 'child').querySelector('[data-testid="thread-row-pin"]')).toBeNull();
  view.getByTestId('thread-group-row-expand').click();
  await settled(view.container);
  expect(isDiscussionExpanded('parent')).toBe(false);
  expect(view.container.querySelectorAll('[data-sidebar-thread-id="child"]')).toHaveLength(1);
});
