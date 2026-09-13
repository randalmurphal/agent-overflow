import { noteThread } from '../../transport/entityIndex';
import { afterEach, beforeEach, expect, it } from 'vitest';
import { mount, unmount, tick } from 'svelte';
import '../../../app.css';
import ChatHeader from './ChatHeader.svelte';
import ComposerWorkspaceStrip from '../composer/ComposerWorkspaceStrip.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { idleWorkspaceActivity } from '../../../test/helpers/workspaceLock';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { setCompactLayoutForTest, showCompactThread, showCompactList } from '../../stores/layoutMode.svelte';

let host: HTMLDivElement;
const mounted: ReturnType<typeof mount>[] = [];
beforeEach(() => {
  setCompactLayoutForTest(true);
  host = document.createElement('div');
  document.body.append(host);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  setBindingMock('GitListBranches', async () => []);
  setBindingMock('SubscribeWorkspaceGitStatus', async () => ({ subscriptionId: 'git' }));
  setBindingMock('UnsubscribeWorkspaceGitStatus', async () => {});
  setBindingMock('GetThreadBrowserCompanionState', async () => null);
});
afterEach(async () => {
  for (const app of mounted.splice(0)) await unmount(app);
  host.remove();
  resetStagedBackends();
  setCompactLayoutForTest(false);
});

const LONG_TITLE = 'Prepare Remote Access Deployment and Validate Every Connected Device IncludingVeryLongUnbrokenNames';

it.each([320, 360, 412])('keeps the title on one swipeable line beside the badge and menu at %ipx', async (width) => {
  host.style.width = `${width}px`;
  const pane = await buildPane(makeThread({ title: LONG_TITLE }));
  mounted.push(mount(ChatHeader, { target: host, props: { pane } }));
  await tick();
  const titleEl = host.querySelector('[data-testid="chat-header-title"]') as HTMLElement;
  const scroller = host.querySelector('[data-testid="chat-header-title-scroller"]') as HTMLElement;
  const badge = host.querySelector('[data-testid="review-toggle"]') as HTMLElement;
  const more = host.querySelector('[data-testid="chat-header-more"]') as HTMLElement;
  const rect = host.getBoundingClientRect();
  // The whole title is in the DOM on one line, no ellipsis: the scroller
  // has text past its edge, fades that edge, and swipes to it.
  expect(scroller.innerText).toBe(LONG_TITLE);
  // One line: the row is its compact tap height (32px), and a wrapped
  // second line would push it past 40.
  expect(titleEl.getBoundingClientRect().height).toBeLessThan(40);
  expect(scroller.scrollWidth).toBeGreaterThan(scroller.clientWidth + 1);
  expect(scroller.style.getPropertyValue('--fade-right')).toBe('24px');
  expect(scroller.style.getPropertyValue('--fade-left')).toBe('0px');
  scroller.scrollLeft = scroller.scrollWidth;
  scroller.dispatchEvent(new Event('scroll'));
  expect(scroller.style.getPropertyValue('--fade-left')).toBe('24px');
  expect(scroller.style.getPropertyValue('--fade-right')).toBe('0px');
  // Badge and menu share the title's row and stay inside the viewport.
  const titleBox = titleEl.getBoundingClientRect();
  expect(badge.getBoundingClientRect().top).toBeLessThan(titleBox.bottom);
  expect(more.getBoundingClientRect().top).toBeLessThan(titleBox.bottom);
  for (const button of host.querySelectorAll('button')) {
    expect(button.getBoundingClientRect().right).toBeLessThanOrEqual(rect.right + 1);
  }
});

it.each([320, 360, 412])('keeps the facts line on its own row with the worktree icon pinned at %ipx', async (width) => {
  host.style.width = `${width}px`;
  stageBackend();
  noteThread('thread-1', '');
  const pane = await buildPane(makeThread({ title: LONG_TITLE, branch: 'feature/remote-access-with-a-very-long-branch-name' }));
  mounted.push(mount(ChatHeader, { target: host, props: { pane } }));
  await tick();
  const titleEl = host.querySelector('[data-testid="chat-header-title"]') as HTMLElement;
  const facts = host.querySelector('[data-testid="chat-header-facts"]') as HTMLElement;
  const machine = host.querySelector('[data-testid="chat-header-machine"]') as HTMLElement;
  const branch = host.querySelector('[data-testid="chat-header-branch"]') as HTMLElement;
  const worktree = host.querySelector('[data-testid="chat-header-worktree"]') as HTMLElement;
  const factsBox = facts.getBoundingClientRect();
  // A full-width row under the title row.
  expect(factsBox.top).toBeGreaterThanOrEqual(titleEl.getBoundingClientRect().bottom);
  expect(factsBox.height).toBeLessThan(40);
  // Nothing scrolls: the branch ellipsizes instead, and the icon keeps its
  // full box at the end of the line.
  expect(facts.scrollWidth).toBeLessThanOrEqual(facts.clientWidth + 1);
  const branchText = branch.querySelector('.truncate') as HTMLElement;
  expect(branchText.scrollWidth).toBeGreaterThan(branchText.clientWidth + 1);
  expect(machine.getBoundingClientRect().right).toBeLessThanOrEqual(branch.getBoundingClientRect().left + 1);
  const worktreeBox = worktree.getBoundingClientRect();
  expect(worktreeBox.left).toBeGreaterThanOrEqual(branch.getBoundingClientRect().right - 1);
  expect(worktreeBox.width).toBeGreaterThanOrEqual(20);
  expect(worktreeBox.right).toBeLessThanOrEqual(host.getBoundingClientRect().right + 1);
  expect(host.scrollWidth).toBeLessThanOrEqual(width + 1);
});

it('isolates inactive screen painting without zeroing its layout, through repeated screen changes', async () => {
  host.style.cssText = 'position:relative;width:360px;height:700px';
  host.innerHTML = '<aside class="compact-screen compact-screen-list"><div style="visibility:visible;transform:translateZ(0)">Animated sidebar row</div></aside><main class="compact-screen compact-screen-thread">Thread content</main>';
  const sidebar = host.querySelector('aside')!;
  const thread = host.querySelector('main')!;
  const initialHeight = sidebar.getBoundingClientRect().height;
  for (let lap = 0; lap < 3; lap++) {
    showCompactThread();
    await tick();
    expect(getComputedStyle(sidebar).opacity).toBe('0');
    expect(getComputedStyle(thread).opacity).toBe('1');
    expect(sidebar.getBoundingClientRect().height).toBe(initialHeight);
    showCompactList();
    await tick();
    expect(getComputedStyle(sidebar).opacity).toBe('1');
    expect(getComputedStyle(thread).opacity).toBe('0');
  }
});

it('restores the desktop header and single-row footer when leaving compact mode', async () => {
  host.style.width = '1100px';
  const pane = await buildPane(makeThread({ title: 'Desktop thread', branch: 'main' }));
  mounted.push(mount(ChatHeader, { target: host, props: { pane } }));
  mounted.push(mount(ComposerWorkspaceStrip, { target: host, props: { pane, readonly: true, usageLabel: '2.5M' } }));
  await tick();
  expect(host.querySelector('[data-testid="composer-workspace-strip"]')).toBeNull();
  setCompactLayoutForTest(false);
  await tick();
  expect(host.querySelector('[data-testid="chat-header-facts"]')).toBeNull();
  expect(host.querySelector('[data-testid="chat-header-title-scroller"]')).toBeNull();
  const crumb = host.querySelector('[data-testid="chat-header-project"]')!;
  const title = host.querySelector('[data-testid="chat-header-title"]')!;
  expect(crumb.getBoundingClientRect().right).toBeLessThanOrEqual(title.getBoundingClientRect().left + 1);
  const strip = host.querySelector('[data-testid="composer-workspace-strip"]')!;
  const branch = strip.querySelector('[data-testid="branch-picker-trigger"]')!;
  const env = strip.querySelector('[data-testid="env-picker-trigger"]')!;
  const usage = strip.querySelector('[data-testid="workspace-strip-usage"]')!;
  expect(env.getBoundingClientRect().top).toBe(branch.getBoundingClientRect().top);
  expect(usage.getBoundingClientRect().top).toBe(branch.getBoundingClientRect().top);
  expect(host.querySelector('[data-testid="compact-back"]')).toBeNull();
  expect(host.scrollWidth).toBeLessThanOrEqual(1101);
});
