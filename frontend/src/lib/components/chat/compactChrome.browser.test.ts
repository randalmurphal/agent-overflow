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

it.each([320, 360, 412])('keeps complete title and review badge visible at %ipx', async (width) => {
  host.style.width = `${width}px`;
  const title = 'Prepare Remote Access Deployment and Validate Every Connected Device IncludingVeryLongUnbrokenNames';
  const pane = await buildPane(makeThread({ title }));
  mounted.push(mount(ChatHeader, { target: host, props: { pane } }));
  await tick();
  const titleEl = host.querySelector('[data-testid="chat-header-title"]') as HTMLElement;
  const badge = host.querySelector('[data-testid="review-toggle"]') as HTMLElement;
  const rect = host.getBoundingClientRect();
  expect(titleEl.innerText).toBe(title);
  expect(titleEl.scrollWidth).toBeLessThanOrEqual(titleEl.clientWidth + 1);
  expect(titleEl.getBoundingClientRect().height).toBeGreaterThan(35);
  expect(badge.getBoundingClientRect().top).toBeGreaterThanOrEqual(titleEl.getBoundingClientRect().bottom);
  for (const button of host.querySelectorAll('button')) {
    expect(button.getBoundingClientRect().right).toBeLessThanOrEqual(rect.right + 1);
  }
});

it.each([320, 360, 412])('keeps workspace and usage inside a %ipx footer', async (width) => {
  host.style.width = `${width}px`;
  stageBackend();
  const pane = await buildPane(makeThread({ branch: 'feature/remote-access-with-a-very-long-branch-name' }));
  mounted.push(mount(ComposerWorkspaceStrip, { target: host, props: { pane, readonly: true, usageLabel: '2.5M · $120.25' } }));
  await tick();
  const workspace = host.querySelector('[data-testid="workspace-picker-trigger"]') as HTMLElement;
  const project = host.querySelector('[data-testid="project-picker-trigger"]') as HTMLElement;
  const usage = host.querySelector('[data-testid="workspace-strip-usage"]') as HTMLElement;
  expect(workspace.getBoundingClientRect().top).toBeGreaterThanOrEqual(project.getBoundingClientRect().bottom);
  expect(workspace.scrollWidth).toBeLessThanOrEqual(workspace.clientWidth + 1);
  expect(workspace.getBoundingClientRect().right).toBeLessThanOrEqual(usage.getBoundingClientRect().left);
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
  setCompactLayoutForTest(false);
  await tick();
  const titleRow = host.querySelector('[data-testid="chat-header-title-row"]')!;
  const actionsRow = host.querySelector('[data-testid="chat-header-actions-row"]')!;
  expect(getComputedStyle(titleRow).display).toBe('contents');
  expect(getComputedStyle(actionsRow).display).toBe('contents');
  const strip = host.querySelector('[data-testid="composer-workspace-strip"]')!;
  const project = strip.querySelector('[data-testid="project-picker-trigger"]')!;
  const branch = strip.querySelector('[data-testid="branch-picker-trigger"]')!;
  expect(branch.getBoundingClientRect().top).toBe(project.getBoundingClientRect().top);
  expect(host.querySelector('[data-testid="compact-back"]')).toBeNull();
  expect(host.scrollWidth).toBeLessThanOrEqual(1101);
});
