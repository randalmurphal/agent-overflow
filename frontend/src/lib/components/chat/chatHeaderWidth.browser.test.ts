// How the desktop chat header shares a narrowing row. The project crumb
// shows its whole name or is not there; the title is what truncates, down
// to a floor of 8rem or its own width; the cluster stays whole on the
// title's row until the title is at that floor, then takes a second row.
// Real layout: happy-dom reports no geometry.

import { afterEach, beforeEach, expect, it } from 'vitest';
import { mount, unmount, tick } from 'svelte';
import '../../../app.css';
import ChatHeader from './ChatHeader.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { __resetGitStatusStoreForTest, __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';

const PROJECT = 'agent-overflow';
const LONG_PROJECT = 'a-project-with-a-very-long-descriptive-name-indeed';
const LONG_TITLE = 'BLITZ-965 adversarial review (MR !515 + release two) and every follow-up it turned up';
const TITLE_FLOOR_PX = 128;
const ONE_ROW_MAX_PX = 48;

let host: HTMLDivElement;
const mounted: ReturnType<typeof mount>[] = [];

function addProject(name: string): void {
  resetProjectsForTest();
  addProjectLocal({
    id: 'project-1',
    path: '/tmp/workspace',
    name,
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  });
}

beforeEach(() => {
  setCompactLayoutForTest(false);
  host = document.createElement('div');
  document.body.append(host);
  addProject(PROJECT);
  // The cluster at its usual fullest: PR badge, diff badge, the git split
  // button and the actions menu.
  __seedGitStatusForTest('/tmp/workspace', {
    isRepo: true,
    branch: 'feature/header',
    isDefaultBranch: false,
    hasChanges: true,
    insertions: 128,
    deletions: 64,
    fileCount: 3,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'gitlab',
    openPrUrl: 'https://gitlab.example.com/o/r/-/merge_requests/515',
    openPrNumber: 515,
  });
  setBindingMock('ListAvailableEditors', async () => []);
  setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
});

afterEach(async () => {
  for (const app of mounted.splice(0)) await unmount(app);
  host.remove();
  __resetGitStatusStoreForTest();
});

async function mountHeader(width: number, title = LONG_TITLE) {
  host.style.width = `${width}px`;
  const pane = await buildPane(makeThread({ title }));
  mounted.push(mount(ChatHeader, { target: host, props: { pane } }));
  const q = (id: string) => host.querySelector<HTMLElement>(`[data-testid="${id}"]`)!;
  await tick();
  return {
    header: q('chat-header'),
    crumb: q('chat-header-project'),
    title: q('chat-header-title'),
    close: q('pane-close'),
    prBadge: q('chat-header-pr-badge'),
    more: q('chat-header-more'),
  };
}

function expectOneRow(header: HTMLElement): void {
  expect(header.getBoundingClientRect().height, 'the cluster shares the title row').toBeLessThan(ONE_ROW_MAX_PX);
}

function expectNothingPastTheHeader(header: HTMLElement): void {
  const right = header.getBoundingClientRect().right;
  for (const el of header.querySelectorAll('*')) {
    expect(el.getBoundingClientRect().right, el.outerHTML.slice(0, 80)).toBeLessThanOrEqual(right + 1);
  }
}

it.each([900, 720, 620])('shows the whole project name and truncates the title at %ipx', async (width) => {
  const { header, crumb, title } = await mountHeader(width);
  const label = crumb.querySelector('span')!;
  expect(label.textContent).toBe(PROJECT);
  expect(label.scrollWidth, 'the crumb is not truncated').toBeLessThanOrEqual(label.clientWidth);
  expect(title.scrollWidth, 'the title is what truncates').toBeGreaterThan(title.clientWidth);
  expect(getComputedStyle(title).textOverflow).toBe('ellipsis');
  expect(title.getBoundingClientRect().width, 'the title keeps its floor').toBeGreaterThanOrEqual(TITLE_FLOOR_PX);
  expectOneRow(header);
  expectNothingPastTheHeader(header);
});

it.each([600, 480])('leaves the crumb out of a header under 36rem and keeps one row at %ipx', async (width) => {
  const { header, crumb, title } = await mountHeader(width);
  expect(getComputedStyle(crumb).display).toBe('none');
  expect(title.getBoundingClientRect().width).toBeGreaterThanOrEqual(TITLE_FLOOR_PX);
  expectOneRow(header);
  expectNothingPastTheHeader(header);
});

it('moves the cluster to a second row, whole, once the title is at its floor', async () => {
  // A project name at the crumb's 14rem cap in a header just wide enough to
  // show it: the crumb, an 8rem title and the cluster do not fit one row.
  addProject(LONG_PROJECT);
  const { header, crumb, title, prBadge, more } = await mountHeader(640);
  expect(crumb.getBoundingClientRect().width).toBeCloseTo(224, 0);
  const titleBox = title.getBoundingClientRect();
  expect(crumb.getBoundingClientRect().top).toBeLessThan(titleBox.bottom);
  // The title has the first row to itself, so it is wider than its floor.
  expect(titleBox.width).toBeGreaterThan(TITLE_FLOOR_PX);
  expect(prBadge.getBoundingClientRect().top).toBeGreaterThanOrEqual(titleBox.bottom);
  expect(more.getBoundingClientRect().top).toBe(prBadge.getBoundingClientRect().top);
  expectNothingPastTheHeader(header);
});

it('wraps the cluster itself only in a pane narrower than the cluster', async () => {
  const { header, prBadge, more } = await mountHeader(240);
  expect(more.getBoundingClientRect().top).toBeGreaterThan(prBadge.getBoundingClientRect().top);
  expectNothingPastTheHeader(header);
});

async function startRename(title: HTMLElement): Promise<HTMLInputElement> {
  title.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true }));
  await tick();
  await tick();
  return host.querySelector<HTMLInputElement>('[data-testid="chat-header-title-input"]')!;
}

it('gives the rename editor the spare width of the row', async () => {
  const { header, title, close, prBadge } = await mountHeader(900, 'Ops');
  const input = await startRename(title);
  const inputBox = input.getBoundingClientRect();
  expect(prBadge.getBoundingClientRect().top).toBeLessThan(inputBox.bottom);
  // It fills the row up to the controls after it rather than hugging "Ops".
  expect(inputBox.width).toBeGreaterThan(300);
  expect(close.getBoundingClientRect().left).toBeGreaterThanOrEqual(inputBox.right);
  expectOneRow(header);
  expectNothingPastTheHeader(header);
});

it('keeps the rename editor at the title floor, with the cluster where it was', async () => {
  // The wrapped header: entering and leaving rename must not pull the
  // cluster back onto the first row and squeeze the editor.
  addProject(LONG_PROJECT);
  const { header, title, prBadge } = await mountHeader(640);
  expect(prBadge.getBoundingClientRect().top).toBeGreaterThanOrEqual(title.getBoundingClientRect().bottom);
  const input = await startRename(title);
  const inputBox = input.getBoundingClientRect();
  expect(inputBox.width).toBeGreaterThanOrEqual(TITLE_FLOOR_PX);
  expect(prBadge.getBoundingClientRect().top).toBeGreaterThanOrEqual(inputBox.bottom);
  expectNothingPastTheHeader(header);

  input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
  await tick();
  const restored = host.querySelector<HTMLElement>('[data-testid="chat-header-title"]')!;
  expect(prBadge.getBoundingClientRect().top).toBeGreaterThanOrEqual(restored.getBoundingClientRect().bottom);
  expectNothingPastTheHeader(header);
});

it.each([900, 480])('leaves a short title its own width, with the close control beside it, at %ipx', async (width) => {
  const { header, title, close } = await mountHeader(width, 'Ops');
  expect(title.scrollWidth).toBeLessThanOrEqual(title.clientWidth);
  expect(title.getBoundingClientRect().width).toBeLessThan(64);
  expect(close.getBoundingClientRect().left - title.getBoundingClientRect().right).toBeLessThan(16);
  expectOneRow(header);
});
