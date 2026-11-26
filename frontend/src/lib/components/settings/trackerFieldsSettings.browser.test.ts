// Settings > Issues > Fields in a real browser, at a phone's width: two
// fields of one long name each show their whole ID beside the name, which
// is cut short instead, pinned and among the matches.

import { beforeEach, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import '../../../app.css';
import type { Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerFieldsSettings from './TrackerFieldsSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';

const SITE = 'https://example.atlassian.net';
const LONG = `Customer${'_impact'.repeat(16)}`;
const WIDTH = 320;

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice()]);
  setBindingMock('ProjectTrackerSiteFields', async () => [
    { id: 'customfield_10001', name: LONG, kind: 'text' },
    { id: 'customfield_10002', name: LONG, kind: 'text' },
  ]);
  const stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_10001'] }], start_work: [] } } as SetupView;
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => ({ saved: true, stored: { revision: 'r1', setup } }));
});

/** Expects `chip` to draw the whole of `id` where it can be seen: inside
 * the chip and inside the box of the element that clips its text. */
function expectWholeID(chip: HTMLElement, id: string): void {
  const text = `(${id})`;
  const walker = document.createTreeWalker(chip, NodeFilter.SHOW_TEXT);
  let node: Text | null = null;
  while (walker.nextNode()) {
    if ((walker.currentNode as Text).data.includes(text)) node = walker.currentNode as Text;
  }
  expect(node, `${id} is drawn in the chip`).not.toBeNull();
  const range = document.createRange();
  const at = node!.data.indexOf(text);
  range.setStart(node!, at);
  range.setEnd(node!, at + text.length);
  const drawn = range.getBoundingClientRect();
  const clip = node!.parentElement!.getBoundingClientRect();
  const box = chip.getBoundingClientRect();
  expect(drawn.right, `${id} is cut short`).toBeLessThanOrEqual(Math.min(clip.right, box.right) + 0.5);
  expect(drawn.left).toBeGreaterThanOrEqual(box.left);
  expect(box.right).toBeLessThanOrEqual(WIDTH);
}

it('shows the whole ID of two fields of one long name, pinned and matched, at a phone\'s width', async () => {
  await page.viewport(360, 800);
  const host = document.createElement('div');
  host.style.width = `${WIDTH}px`;
  document.body.append(host);
  const view = render(TrackerFieldsSettings, { target: host });
  const fields = await waitFor(() => view.getAllByTestId('tracker-fields-list').find((el) => el.dataset.kind === 'fields')!);
  const find = await within(fields).findByTestId('tracker-fields-find');
  expectWholeID(within(fields).getByTestId('tracker-fields-pinned'), 'customfield_10001');
  await fireEvent.input(find, { target: { value: 'customer' } });
  expectWholeID(await within(fields).findByTestId('tracker-fields-match'), 'customfield_10002');
});
