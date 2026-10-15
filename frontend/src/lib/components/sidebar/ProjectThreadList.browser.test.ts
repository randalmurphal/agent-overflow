import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import ProjectThreadList from './ProjectThreadList.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest } from '../../stores/panes.svelte';
import { getCollapsedAgents, getCollapsedGroups, resetSidebarForTest } from '../../stores/sidebar.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { replaceAllThreads, touchThreadActivity } from '../../stores/threads.svelte';
import { resetAgentWakesForTest } from '../../stores/agentWakes.svelte';
import { resetArchivedChildrenForTest } from '../../stores/agentArchivedChildren.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { THREAD_PREVIEW_LIMIT } from '../../utils/sidebarThreadLimits';
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

// Settled once a full frame passes with no finite animation running. An
// animation's finish handler runs in the frame after it finishes, and
// Svelte's transitions act there: the zero-length lead animation's handler
// starts the real one, and an exit transition's removes its element. So a
// check between a finish and its frame sees nothing running while the DOM
// is still about to change.
async function settled(container: HTMLElement): Promise<void> {
  await tick();
  for (;;) {
    await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
    await tick();
    const running = container.getAnimations({ subtree: true }).filter(a =>
      a.playState === 'running' && a.effect?.getComputedTiming().endTime !== Infinity,
    );
    if (running.length === 0) return;
    await Promise.all(running.map(a => a.finished.catch(() => undefined)));
  }
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
  // Nothing but the section divider (1px rule, 4px margin each side) sits
  // between the previewed member and the unpinned section below it.
  const activeRect = row(view.container, 'alpha').getBoundingClientRect();
  const divider = view.container.querySelector('[data-testid="thread-section-divider"]')!.getBoundingClientRect();
  expect(divider.top).toBeGreaterThanOrEqual(activeRect.bottom);
  expect(Math.abs(row(view.container, 'loose').getBoundingClientRect().top - activeRect.bottom)).toBeLessThanOrEqual(2 + 9);

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

it('aligns the group chevron with top-level pins and member pins with the folder glyph', async () => {
  const threads = [
    thread('member', { groupId: group.id, pinnedAt: 1 }),
    thread('plain-member', { groupId: group.id }),
    thread('loose', { pinnedAt: 1 }),
  ];
  const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [group], pane: createThreadPane() });
  view.container.style.width = '360px';
  await settled(view.container);

  const centre = (el: Element) => { const r = el.getBoundingClientRect(); return r.left + r.width / 2; };
  const pinOf = (id: string) => row(view.container, id).querySelector('[data-testid="thread-row-pin"]')!;
  const chevron = view.getByTestId('thread-group-row-expand');
  const folder = view.getByTestId('thread-group-row-folder');

  // The group chevron sits on the top-level pin column.
  expect(Math.abs(centre(chevron) - centre(pinOf('loose')))).toBeLessThanOrEqual(1);
  // A member's pin sits under the group's folder glyph.
  expect(Math.abs(centre(pinOf('member')) - centre(folder))).toBeLessThanOrEqual(1);
  // The member rail drops from the chevron's centre.
  const railX = view.container.querySelector<HTMLElement>('[data-group-member]')!.getBoundingClientRect().left;
  expect(Math.abs(railX - centre(chevron))).toBeLessThanOrEqual(1);
  // A member's title lines up with the group name.
  const titleLeft = (id: string) => row(view.container, id).querySelector('[data-testid="thread-row-title"]')!.getBoundingClientRect().left;
  const groupName = view.getByTestId('thread-group-row-name').getBoundingClientRect();
  expect(Math.abs(titleLeft('plain-member') - groupName.left)).toBeLessThanOrEqual(1);
  expect(Math.abs(titleLeft('member') - groupName.left)).toBeLessThanOrEqual(1);
});

// A standing agent's row holds the threads it spawned behind the group
// rail (docs/specs/standing-agents.md, "Sidebar"): same geometry as a
// group, a fold on the chevron, and a `N done` footer for the archived ones.
describe('standing agent rows', () => {
  beforeEach(() => {
    resetAgentWakesForTest();
    resetArchivedChildrenForTest();
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: false }));
  });

  function agentThreads(): Thread[] {
    const threads = [
      thread('agent', { agentPromotedAt: 10, archivedChildCount: 2, updatedAt: 5 }),
      thread('worker-a', { spawnedByThreadId: 'agent', updatedAt: 4 }),
      thread('worker-b', { spawnedByThreadId: 'agent', updatedAt: 3 }),
      thread('loose', { pinnedAt: 1 }),
    ];
    replaceAllThreads(threads);
    return threads;
  }

  it('nests the spawned threads under the agent with a group\'s geometry and folds them on the chevron', async () => {
    const threads = agentThreads();
    const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [], pane: createThreadPane() });
    view.container.style.width = '360px';
    await settled(view.container);

    const agent = row(view.container, 'agent');
    expect(agent.dataset.sidebarAgent).toBe('');
    expect(agent.dataset.expanded).toBe('true');
    expect(agent.querySelector('[data-testid="thread-row-pin"]')).toBeNull();
    expect(agent.querySelector('[data-testid="thread-row-agent-icon"]')).not.toBeNull();
    const members = view.container.querySelectorAll('[data-agent-member] [data-sidebar-thread-id]');
    expect(Array.from(members, (el) => (el as HTMLElement).dataset.sidebarThreadId)).toEqual(['worker-a', 'worker-b']);
    // Both workers are pinned nowhere and the loose pinned thread follows the agent block.
    const order = Array.from(view.container.querySelectorAll<HTMLElement>('[data-sidebar-thread-id]'), (el) => el.dataset.sidebarThreadId);
    expect(order).toEqual(['agent', 'worker-a', 'worker-b', 'loose']);

    const centre = (el: Element) => { const r = el.getBoundingClientRect(); return r.left + r.width / 2; };
    const chevron = agent.querySelector('[data-testid="thread-row-agent-expand"]')!;
    const pinOf = (id: string) => row(view.container, id).querySelector('[data-testid="thread-row-pin"]')!;
    // The agent chevron sits on the top-level pin column, like a group's.
    expect(Math.abs(centre(chevron) - centre(pinOf('loose')))).toBeLessThanOrEqual(1);
    // The rail drops from the chevron's centre.
    const railX = view.container.querySelector<HTMLElement>('[data-agent-member]')!.getBoundingClientRect().left;
    expect(Math.abs(railX - centre(chevron))).toBeLessThanOrEqual(1);
    // A worker's title lines up with the agent's name.
    const titleLeft = (id: string) => row(view.container, id).querySelector('[data-testid="thread-row-title"]')!.getBoundingClientRect().left;
    expect(Math.abs(titleLeft('worker-a') - titleLeft('agent'))).toBeLessThanOrEqual(1);
    // The footer's label starts where a worker's title does.
    const footer = view.getByTestId('thread-row-agent-done');
    expect(footer.textContent).toContain('2 done');
    expect(footer.dataset.expanded).toBe('false');
    const footerLabel = view.getByTestId('thread-row-agent-done-label').getBoundingClientRect();
    expect(Math.abs(footerLabel.left - titleLeft('worker-a'))).toBeLessThanOrEqual(1);
    // Its chevron centres on the workers' pin column.
    const footerChevron = view.getByTestId('thread-row-agent-done-toggle').querySelector('.lucide-icon')!;
    expect(Math.abs(centre(footerChevron) - centre(pinOf('worker-a')))).toBeLessThanOrEqual(1);

    (chevron as HTMLElement).click();
    await settled(view.container);
    expect(agent.dataset.expanded).toBe('false');
    expect(view.container.querySelectorAll('[data-agent-member]')).toHaveLength(0);
    expect(agent.querySelector('[data-testid="thread-row-agent-count"]')!.textContent?.trim()).toBe('2');
    expect([...getCollapsedAgents()]).toEqual(['agent']);
    expect(JSON.parse(appStorageGet('sidebar:collapsedAgents')!)).toEqual(['agent']);

    // Keyboard: Right unfolds, Left folds, on the row itself.
    agent.focus();
    agent.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
    await settled(view.container);
    expect(agent.dataset.expanded).toBe('true');
    expect(view.container.querySelectorAll('[data-agent-member] [data-sidebar-thread-id]')).toHaveLength(2);
    agent.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true }));
    await settled(view.container);
    expect(agent.dataset.expanded).toBe('false');
  });

  it('previews the focused worker beneath a collapsed agent', async () => {
    const threads = agentThreads();
    const pane = createThreadPane();
    registerPaneForTest('main', pane);
    pane.replaceThread(threads[2]);
    const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [], pane });
    view.container.style.width = '360px';
    await settled(view.container);
    row(view.container, 'agent').querySelector<HTMLElement>('[data-testid="thread-row-agent-expand"]')!.click();
    await settled(view.container);
    const members = view.container.querySelectorAll<HTMLElement>('[data-agent-member] [data-sidebar-thread-id]');
    expect(Array.from(members, (el) => el.dataset.sidebarThreadId)).toEqual(['worker-b']);
  });

  it('opens the done footer to the archived workers, newest first, and reloads when the count moves', async () => {
    const threads = agentThreads();
    const archived = [
      thread('done-new', { spawnedByThreadId: 'agent', archived: true, updatedAt: 9 }),
      thread('done-old', { spawnedByThreadId: 'agent', archived: true, updatedAt: 8 }),
    ];
    const list = setBindingMock('ListArchivedSpawnedThreads', async () => archived);
    const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [], pane: createThreadPane() });
    view.container.style.width = '360px';
    await settled(view.container);

    view.getByTestId('thread-row-agent-done-toggle').click();
    await waitFor(() => expect(view.container.querySelectorAll('[data-archived-child]')).toHaveLength(2));
    expect(list).toHaveBeenCalledWith('agent');
    const rows = Array.from(view.container.querySelectorAll<HTMLElement>('[data-sidebar-thread-id]'), (el) => el.dataset.sidebarThreadId);
    expect(rows).toEqual(['agent', 'worker-a', 'worker-b', 'done-new', 'done-old', 'loose']);
    expect(view.getByTestId('thread-row-agent-done').dataset.expanded).toBe('true');
    expect(view.queryByTestId('thread-row-agent-done-more')).toBeNull();
    // The archived rows sit behind the agent's rail, dimmed.
    for (const id of ['done-new', 'done-old']) {
      expect(row(view.container, id).closest('[data-agent-member]')).not.toBeNull();
      expect(Number(getComputedStyle(row(view.container, id)).opacity)).toBeLessThan(1);
    }

    // A third child archived: the row's count moves and the list is read again.
    list.mockImplementation(async () => [thread('done-newest', { spawnedByThreadId: 'agent', archived: true, updatedAt: 11 }), ...archived]);
    await view.rerender({
      projectId: 'project', groups: [], pane: createThreadPane(),
      threads: threads.map((t) => t.id === 'agent' ? { ...t, archivedChildCount: 3 } : t),
    });
    await waitFor(() => expect(view.container.querySelectorAll('[data-archived-child]')).toHaveLength(3));
    expect(list).toHaveBeenCalledTimes(2);
    expect(view.getByTestId('thread-row-agent-done').textContent).toContain('3 done');

    // Closing the footer drops the rows and the count stands alone again.
    view.getByTestId('thread-row-agent-done-toggle').click();
    await settled(view.container);
    expect(view.container.querySelectorAll('[data-archived-child]')).toHaveLength(0);
    expect(view.getByTestId('thread-row-agent-done').dataset.expanded).toBe('false');
  });

  it('pages a long done list by the preview limit', async () => {
    const threads = agentThreads().map((t) => t.id === 'agent' ? { ...t, archivedChildCount: THREAD_PREVIEW_LIMIT + 3 } : t);
    replaceAllThreads(threads);
    const archived = Array.from({ length: THREAD_PREVIEW_LIMIT + 3 }, (_, i) =>
      thread(`done-${i}`, { spawnedByThreadId: 'agent', archived: true, updatedAt: 100 - i }));
    setBindingMock('ListArchivedSpawnedThreads', async () => archived);
    const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [], pane: createThreadPane() });
    view.container.style.width = '360px';
    await settled(view.container);
    view.getByTestId('thread-row-agent-done-toggle').click();
    await waitFor(() => expect(view.container.querySelectorAll('[data-archived-child]')).toHaveLength(THREAD_PREVIEW_LIMIT));
    const more = view.getByTestId('thread-row-agent-done-more');
    expect(more.textContent?.trim()).toBe('Show 3 More');
    more.click();
    await settled(view.container);
    expect(view.container.querySelectorAll('[data-archived-child]')).toHaveLength(THREAD_PREVIEW_LIMIT + 3);
    expect(view.queryByTestId('thread-row-agent-done-more')).toBeNull();
  });

  it('shows the agent\'s soonest loop under its name and keeps it off plain rows', async () => {
    const threads = agentThreads();
    setBindingMock('ListThreadWakes', async (threadId: unknown) => ({
      moreReminders: false,
      rows: threadId === 'agent' ? [{
        id: 'w1', kind: 'loop', label: 'board check', state: 'armed',
        intervalSeconds: 900, nextAt: Date.now() + 12 * 60_000, missed: 1,
      }] : [],
    }));
    const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [], pane: createThreadPane() });
    view.container.style.width = '360px';
    await waitFor(() => expect(view.queryByTestId('thread-row-agent-state')).not.toBeNull());
    const line = view.getByTestId('thread-row-agent-state');
    expect(line.textContent?.trim()).toBe('loop board check · next 12m · 1 missed');
    // The line starts under the person glyph, as the mockup draws it.
    const glyphLeft = row(view.container, 'agent').querySelector('[data-testid="thread-row-agent-icon"]')!.getBoundingClientRect().left;
    expect(Math.abs(line.querySelector('span')!.getBoundingClientRect().left - glyphLeft)).toBeLessThanOrEqual(1);
    expect(view.container.querySelectorAll('[data-testid="thread-row-agent-state"]')).toHaveLength(1);
  });
});
