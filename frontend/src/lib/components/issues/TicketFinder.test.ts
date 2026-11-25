// The ticket finder over mocked bindings: what it offers before typing,
// a key looked up and words searched on each connection's computer, an
// older text's late answer, a failure in the list, and the pick.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { CardView, LookupView, SearchPageView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TicketFinder, { isKeyShaped, type FinderSource, type FoundTicket } from './TicketFinder.svelte';
import { getPinnedBackend } from '../../transport/backends';
import { TransportError } from '../../transport/wsClient';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const ONE: FinderSource[] = [{ backend: 'peer-1', connection: 'c1', name: 'Work Jira' }];
const card = (id: string, key: string, title: string) => ({ id, key, title }) as CardView;
const body = () => within(document.body);
const rows = () => body().queryAllByTestId('ticket-picker-item').map((i) => i.textContent?.replace(/\s+/g, ' ').trim());

let anchor: HTMLButtonElement;
beforeEach(() => {
  anchor = document.createElement('button');
  document.body.append(anchor);
});
afterEach(() => {
  anchor.remove();
});

function mount(props: Partial<{ sources: FinderSource[]; before: FoundTicket[]; onPick: (t: FoundTicket) => void }> = {}) {
  // `anchor` is also a mount option: the props go under `props`.
  return render(TicketFinder, { props: { anchor, open: true, sources: ONE, onClose: () => {}, onPick: () => {}, ...props } });
}

async function type(text: string): Promise<void> {
  await fireEvent.input(body().getByTestId('ticket-picker-find'), { target: { value: text } });
}

describe('isKeyShaped', () => {
  it('takes a key in any case and nothing else', () => {
    expect(['WEB-1', 'web-12', 'A1_B-300', ' WEB-7 '].map(isKeyShaped)).toEqual([true, true, true, true]);
    expect(['WEB', 'WEB-', '12', 'WEB-1a', 'fix the login', '-1'].map(isKeyShaped)).toEqual([false, false, false, false, false, false]);
  });
});

describe('TicketFinder', () => {
  it('offers the tickets it was given before anything is typed, asking nothing', async () => {
    const search = setBindingMock('TrackerSearch', async () => ({ cards: [] }));
    mount({ before: [{ backend: 'peer-1', connection: 'c1', issue: '10001', key: 'WEB-1', title: 'First story' }] });
    await waitFor(() => expect(rows()).toEqual(['WEB-1 First story']));
    expect(search).not.toHaveBeenCalled();
  });

  it('looks a key up, in capitals, on the connection\'s computer, and picks what it found', async () => {
    const pins: (string | null)[] = [];
    const lookup = setBindingMock('TrackerLookup', async (): Promise<LookupView> => {
      pins.push(getPinnedBackend());
      return { found: [{ key: 'WEB-12', card: card('10012', 'WEB-12', 'Twelfth') }] } as LookupView;
    });
    const onPick = vi.fn();
    mount({ onPick });
    await type('web-12');
    await waitFor(() => expect(rows()).toEqual(['WEB-12 Twelfth']));
    expect(lookup.mock.calls).toEqual([['c1', ['WEB-12']]]);
    expect(pins).toEqual(['peer-1']);
    await fireEvent.click(body().getByTestId('ticket-picker-item'));
    expect(onPick).toHaveBeenCalledWith({ backend: 'peer-1', connection: 'c1', issue: '10012', key: 'WEB-12', title: 'Twelfth' });
  });

  it('searches words with its limit, and shows no late answer for older text', async () => {
    const answers = new Map<string, (v: SearchPageView) => void>();
    const search = setBindingMock('TrackerSearch', (_c: string, req: { text: string }) => new Promise<SearchPageView>((r) => answers.set(req.text, r)));
    mount();
    await type('login');
    await waitFor(() => expect(answers.has('login')).toBe(true));
    await type('login page');
    await waitFor(() => expect(answers.has('login page')).toBe(true));
    expect(search.mock.calls.map((c) => c[1])).toEqual([{ text: 'login', limit: 10 }, { text: 'login page', limit: 10 }]);
    answers.get('login page')!({ cards: [card('1', 'WEB-1', 'Login page')] } as SearchPageView);
    await waitFor(() => expect(rows()).toEqual(['WEB-1 Login page']));
    answers.get('login')!({ cards: [card('2', 'WEB-2', 'Login API')] } as SearchPageView);
    await new Promise((r) => setTimeout(r, 50));
    expect(rows()).toEqual(['WEB-1 Login page']);
  });

  it('asks every connection and names each beside its tickets', async () => {
    setBindingMock('TrackerSearch', async (connection: string) => ({ cards: [card(connection === 'c1' ? '1' : '2', connection === 'c1' ? 'WEB-1' : 'APP-2', 'Login')] }));
    mount({ sources: [...ONE, { backend: 'home', connection: 'c2', name: 'Side Linear' }] });
    await type('login');
    await waitFor(() => expect(rows()).toEqual(['WEB-1 Login · Work Jira', 'APP-2 Login · Side Linear']));
  });

  it('says in the list why a search failed', async () => {
    setBindingMock('TrackerSearch', async () => { throw new TransportError('tracker_unreachable', 'Jira could not be reached; try again'); });
    mount();
    await type('login');
    expect(await body().findByTestId('ticket-picker-error')).toHaveTextContent('Jira could not be reached; try again');
  });
});
