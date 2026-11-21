import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import ProjectThreadList from './ProjectThreadList.svelte';
import AssistantsSection from './AssistantsSection.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest } from '../../stores/panes.svelte';
import { getCollapsedGroups, resetSidebarForTest } from '../../stores/sidebar.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { replaceAllThreads, touchThreadActivity } from '../../stores/threads.svelte';
import { resetProjectsForTest } from '../../stores/projects.svelte';
import { resetThreadWakesForTest } from '../../stores/threadWakes.svelte';
import { refreshThreadWorkerSummaries, resetThreadWorkerSummariesForTest } from '../../stores/threadWorkerSummaries.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
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
  const view = render(ProjectThreadList, { projectId: 'project', listKey: 'project', threads, groups: [group], pane });
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
  const view = render(ProjectThreadList, { projectId: 'project', listKey: 'project', threads, groups: [group], pane: createThreadPane() });
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

// An assistant is one row (docs/specs/standing-agents.md, "Sidebar"): its
// glyph in the pin's column, its title where any title starts, and its state
// and projects lines under the title.
describe('assistant rows', () => {
  beforeEach(() => {
    resetThreadWakesForTest();
    resetProjectsForTest();
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: false }));
  });

  const centre = (el: Element) => { const r = el.getBoundingClientRect(); return r.left + r.width / 2; };
  const fromRow = (container: HTMLElement, id: string, x: number) => x - row(container, id).getBoundingClientRect().left;

  it.each([false, true])('puts the glyph in the pin column and the lines under the title (compact=%s)', async (compact) => {
    await page.viewport(compact ? 390 : 800, 700);
    setCompactLayoutForTest(compact);
    setBindingMock('ListProjects', async () => [
      { project: { id: 'p-a', path: '/repo/a', name: 'Alpha', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 0 },
      { project: { id: 'p-b', path: '/repo/b', name: 'Beta', sortPosition: 1, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 0 },
    ]);
    await readSidebarCatalogs();
    const agent = thread('agent', { agentId: 'agent', projectId: '', agentProjectIds: ['p-a', 'p-b'], updatedAt: 5 });
    const loose = thread('loose', { pinnedAt: 1 });
    replaceAllThreads([agent, loose]);
    setBindingMock('ListThreadWakes', async (threadId: unknown) => ({
      moreReminders: false,
      rows: threadId === 'agent' ? [{
        id: 'w1', kind: 'loop', label: 'board check', state: 'armed',
        intervalSeconds: 900, nextAt: Date.now() + 12 * 60_000, missed: 1,
      }] : [],
    }));
    const props = (assistant: Thread) => ({ assistants: [assistant], pane: createThreadPane(), searching: false });
    const view = render(AssistantsSection, props(agent));
    view.container.style.width = '360px';
    const plain = render(ProjectThreadList, { projectId: 'project', listKey: 'project', threads: [loose], groups: [], pane: createThreadPane() });
    plain.container.style.width = '360px';
    await waitFor(() => expect(view.queryByTestId('thread-row-agent-state')).not.toBeNull());
    await settled(view.container);

    const assistantRow = row(view.container, 'agent');
    expect(view.container.querySelectorAll('[data-sidebar-thread-id]')).toHaveLength(1);
    expect(assistantRow.querySelector('[data-testid="thread-row-pin"]')).toBeNull();
    const glyph = assistantRow.querySelector('[data-testid="thread-row-agent-icon"]')!;
    const pin = row(plain.container, 'loose').querySelector('[data-testid="thread-row-pin"]')!;
    expect(Math.abs(fromRow(view.container, 'agent', centre(glyph)) - fromRow(plain.container, 'loose', centre(pin)))).toBeLessThanOrEqual(1);
    const titleLeft = (container: HTMLElement, id: string) =>
      row(container, id).querySelector('[data-testid="thread-row-title"]')!.getBoundingClientRect().left;
    expect(Math.abs(fromRow(view.container, 'agent', titleLeft(view.container, 'agent')) - fromRow(plain.container, 'loose', titleLeft(plain.container, 'loose')))).toBeLessThanOrEqual(1);

    const line = view.getByTestId('thread-row-agent-state');
    expect(line.textContent?.trim()).toBe('loop board check · next 12m · 1 missed');
    const projects = view.getByTestId('thread-row-agent-projects');
    expect(projects.textContent?.trim()).toBe('Alpha, Beta');
    for (const el of [line, projects]) {
      expect(Math.abs(el.querySelector('span')!.getBoundingClientRect().left - titleLeft(view.container, 'agent'))).toBeLessThanOrEqual(1);
    }
    plain.unmount();

    await view.rerender(props({ ...agent, agentAllProjects: true, agentProjectIds: [] }));
    await settled(view.container);
    expect(view.getByTestId('thread-row-agent-projects').textContent?.trim()).toBe('All projects');
    await view.rerender(props({ ...agent, agentProjectIds: [] }));
    await settled(view.container);
    expect(view.getByTestId('thread-row-agent-projects').textContent?.trim()).toBe('No projects');
  });
});

// A row's worker count and needs-you badge sit between its title and its
// time, on the title's line, at every density.
describe('worker counts', () => {
  beforeEach(() => {
    resetThreadWorkerSummariesForTest();
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: false }));
  });

  it.each([false, true])('fit between the title and the time without overlapping either (compact=%s)', async (compact) => {
    await page.viewport(compact ? 390 : 800, 700);
    setCompactLayoutForTest(compact);
    const spawner = thread('spawner', { title: 'A spawner with a title long enough to be cut short', updatedAt: 5 });
    replaceAllThreads([spawner]);
    setBindingMock('ListThreadWorkerSummaries', async () => ({ summaries: [{ threadId: 'spawner', open: 12, needYou: 3 }], next: '' }));
    await refreshThreadWorkerSummaries();
    const view = render(ProjectThreadList, { projectId: 'project', listKey: 'project', threads: [spawner], groups: [], pane: createThreadPane() });
    view.container.style.width = '300px';
    await settled(view.container);

    const rowRect = row(view.container, 'spawner').getBoundingClientRect();
    const title = view.getByTestId('thread-row-title').getBoundingClientRect();
    const badge = view.getByTestId('thread-row-workers-need-you').getBoundingClientRect();
    const count = view.getByTestId('thread-row-workers').getBoundingClientRect();
    const time = view.getByTestId('thread-row-time').getBoundingClientRect();
    expect(title.right).toBeLessThanOrEqual(badge.left);
    expect(badge.right).toBeLessThanOrEqual(count.left);
    expect(count.right).toBeLessThanOrEqual(time.left);
    expect(time.right).toBeLessThanOrEqual(rowRect.right);
    // One line: each sits on the title's centre line.
    const middle = (r: DOMRect) => r.top + r.height / 2;
    for (const r of [badge, count]) expect(Math.abs(middle(r) - middle(title))).toBeLessThanOrEqual(1);
  });
});
