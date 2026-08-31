import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import PairingScreen from './PairingScreen.svelte';
import type { PairingPayload } from '../../transport/deviceSession';

// Driven through the REAL deviceSession module with a stubbed global
// fetch, not a module mock: the screen's contract is the flow those two
// walk together, and a mock that answered stages the component never
// reaches would pin the wrong thing.

const PAYLOAD: PairingPayload = {
  v: 1,
  backendId: 'backend-1',
  backendName: 'Home desk',
  // happy-dom's location.origin for component tests.
  endpoint: 'http://localhost:3000',
  token: 'link-token',
};

function grant(): Response {
  return new Response(
    JSON.stringify({
      sessionId: 'sess-1',
      credential: 'cred-1',
      expiresAtMs: Date.now() + 900_000,
      verificationNumber: '481523',
    }),
    { status: 200 },
  );
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('PairingScreen', () => {
  it('walks intro → verification number on a successful redemption', async () => {
    const fetchMock = vi.fn(async (path: string) => {
      if (path === '/auth/pair') return grant();
      return new Response('not found', { status: 404 });
    });
    vi.stubGlobal('fetch', fetchMock);

    const { getByText, getByRole, getByLabelText } = render(PairingScreen, {
      props: { payload: PAYLOAD, onDone: () => {} },
    });

    getByText('Pair this device');
    getByText('Home desk');
    await fireEvent.click(getByRole('button', { name: 'Pair' }));

    await waitFor(() => getByLabelText('Verification number'));
    getByText('481523');
    getByText('Waiting for confirmation');

    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    const body = JSON.parse(init.body as string) as Record<string, string>;
    expect(body.token).toBe('link-token');
    expect(body.keyThumbprint).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it('finishes once the owner confirms', async () => {
    vi.useFakeTimers();
    let admitted = false;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (path: string) => {
        if (path === '/auth/pair') return grant();
        return admitted
          ? new Response(JSON.stringify({ ticket: 'tik-1' }), { status: 200 })
          : new Response('not found', { status: 404 });
      }),
    );
    const onDone = vi.fn();
    const { getByRole, getByText } = render(PairingScreen, {
      props: { payload: PAYLOAD, onDone },
    });
    await fireEvent.click(getByRole('button', { name: 'Pair' }));
    await vi.waitFor(() => getByText('Waiting for confirmation'));

    // One probe answers pending; the owner then confirms; the next
    // probe finishes the flow after the hand-off beat.
    await vi.advanceTimersByTimeAsync(3_100);
    getByText('Waiting for confirmation');
    admitted = true;
    await vi.advanceTimersByTimeAsync(3_100);
    getByText('Paired');
    expect(onDone).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(800);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it('shows the refusal reason when the link no longer admits anything', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ reason: 'unknown_credential' }), { status: 401 })),
    );
    const { getByRole, getByText, queryByLabelText } = render(PairingScreen, {
      props: { payload: PAYLOAD, onDone: () => {} },
    });
    await fireEvent.click(getByRole('button', { name: 'Pair' }));
    // The sentences come from authReason — the ONE refusal vocabulary —
    // so the pin is that they are what renders, with no number shown.
    await waitFor(() => getByText('This pairing could not be completed.'));
    getByText('Start a new pairing from the app on your computer.');
    expect(queryByLabelText('Verification number')).toBeNull();
  });

  it('refuses a payload for a different address without spending anything', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const { getByRole, getByText } = render(PairingScreen, {
      props: {
        payload: { ...PAYLOAD, endpoint: 'http://somewhere-else:9' },
        onDone: () => {},
      },
    });
    await fireEvent.click(getByRole('button', { name: 'Pair' }));
    getByText('This link belongs to a different address.');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders the parse failure when the fragment could not be read', () => {
    const { getByText } = render(PairingScreen, {
      props: { payload: null, parseError: 'This pairing link is damaged. Ask for a new one.', onDone: () => {} },
    });
    getByText('This pairing link is damaged. Ask for a new one.');
    getByText('Ask for a new pairing link from the app on your computer.');
  });
});
