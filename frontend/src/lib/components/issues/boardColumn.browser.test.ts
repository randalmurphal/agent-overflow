// Real-Chromium suite for a Board column: a column of 2,000 cards mounts
// the cards near the viewport only, and a column held in part asks for its
// next page as the reader nears the end of what is held, not before, and
// not while a page is being read or a filter reads the column whole.

import { afterEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';
import BoardColumn from './BoardColumn.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { ScreenColumn } from '../../stores/trackerBoardModel';
import type { CardField } from '../../stores/issuesPane.svelte';
import { waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';

const VIEWPORT_PX = 600;
// The viewport plus the virtualizer's overscan on each side, in cards of
// at least 70px, with slack: far below the thousands held.
const MOUNTED_BOUND = 60;

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function card(i: number): RowView {
  return {
    order: `k${String(i).padStart(5, '0')}`,
    estimate: 3,
    card: {
      id: String(i), key: `WEB-${i}`, title: `Issue ${i} with a title long enough to wrap onto a second line of the card`,
      status: { id: 's1', name: 'To Do' }, created: 0, updated: 0,
      assignee: { id: 'acc-user1', name: 'User 1' }, labels: [{ id: 'l', name: 'backend' }],
    },
  } as RowView;
}

function column(rows: RowView[], more: boolean): ScreenColumn {
  return { name: 'To Do', statusGroups: ['todo'], rows, moreFrom: more ? 0 : null, total: { rows: more ? rows.length + 500 : rows.length, estimate: 0 }, counted: rows.length, unread: false };
}

function mountColumn(rows: RowView[], more: boolean) {
  const host = document.createElement('div');
  host.style.cssText = `position: fixed; top: 0; left: 0; width: 320px; height: ${VIEWPORT_PX}px; display: flex;`;
  document.body.appendChild(host);
  const asks: number[] = [];
  const props = rawProps({
    column: column(rows, more),
    rows: rows as readonly RowView[],
    count: String(rows.length),
    filtered: false,
    unit: 'points',
    hidden: new Set<CardField>() as ReadonlySet<CardField>,
    now: Date.now(),
    paging: false,
    failed: false,
    onNearEnd: () => { asks.push(Date.now()); },
    shown: true,
    openIssue: null,
    boardRef: { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: 'b1' },
    typed: true,
    compact: false,
    carriedIssue: null as string | null,
    editing: null,
    dropNote: null as string | null,
    slot: null as number | null,
    onOpenCard: () => {},
    onCardMenu: () => {},
    onEditCard: () => {},
    onMoveCard: () => {},
  });
  const app = mount(BoardColumn, { target: host, props, context: ticketWriteSurfaceContext(() => true) });
  mounted.push({ app, host });
  const scrollEl = host.querySelector<HTMLElement>('[data-testid="issues-column-cards"]')!;
  return { props, asks, scrollEl };
}

function mountedCards(scrollEl: HTMLElement): string[] {
  return [...scrollEl.querySelectorAll<HTMLElement>('[data-testid="issues-card"]')].map((el) => el.dataset.issue!);
}

async function settle(scrollEl: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${scrollEl.scrollTop}:${scrollEl.scrollHeight}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable geometry');
}

describe('BoardColumn', () => {
  it('mounts the cards near the viewport of a 2,000-card column, at the top and at the end', async () => {
    const rows = Array.from({ length: 2_000 }, (_, i) => card(i));
    const { scrollEl } = mountColumn(rows, false);
    await settle(scrollEl);
    await waitFor(() => mountedCards(scrollEl).length > 0, 'cards mounted');
    expect(mountedCards(scrollEl).length).toBeLessThan(MOUNTED_BOUND);
    expect(mountedCards(scrollEl)[0]).toBe('0');

    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    await waitFor(() => mountedCards(scrollEl).includes('1999'), 'the last card mounted');
    expect(mountedCards(scrollEl).length).toBeLessThan(MOUNTED_BOUND);
  });

  it('asks for the next page near the end of what is held, once per page', async () => {
    const rows = Array.from({ length: 100 }, (_, i) => card(i));
    const { props, asks, scrollEl } = mountColumn(rows, true);
    await settle(scrollEl);
    expect(asks).toHaveLength(0);

    // A page in flight holds the ask back.
    props.paging = true;
    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    expect(asks).toHaveLength(0);
    props.paging = false;
    await waitFor(() => asks.length === 1, 'the next page asked for');

    // The page lands: the reader is no longer near the end.
    props.paging = true;
    const more = Array.from({ length: 200 }, (_, i) => card(i));
    props.column = column(more, true);
    props.rows = more;
    props.paging = false;
    await settle(scrollEl);
    expect(asks).toHaveLength(1);

    // A filter reads the column whole: nothing pages on scroll.
    props.filtered = true;
    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    expect(asks).toHaveLength(1);
  });
});
