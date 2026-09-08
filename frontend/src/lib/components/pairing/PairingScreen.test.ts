import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import PairingScreen from './PairingScreen.svelte';
import type { PairingPayload } from '../../transport/deviceSession';
import { __resetHomeEndpointForTest, storedBackendEndpoint } from '../../transport/homeEndpoint';

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
  __resetHomeEndpointForTest();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
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

  it('offers Try again when the request never reached the computer, keeping the same link', async () => {
    let attempts = 0;
    const fetchMock = vi.fn(async (path: string) => {
      if (++attempts === 1) throw new TypeError('Failed to fetch');
      return path === '/auth/pair' ? grant() : new Response('not found', { status: 404 });
    });
    vi.stubGlobal('fetch', fetchMock);
    const { getByRole, getByText, findByText, getByLabelText, queryByRole } = render(PairingScreen, {
      props: { payload: PAYLOAD, onDone: () => {} },
    });
    await fireEvent.click(getByRole('button', { name: 'Pair' }));
    await findByText('Pairing did not go through.');
    // A browser has no scan screen to start over from; the link is still
    // good, so the way forward is the same link again.
    expect(queryByRole('button', { name: 'Start over' })).toBeNull();
    await fireEvent.click(getByRole('button', { name: 'Try again' }));
    getByText('Pair this device');
    await fireEvent.click(getByRole('button', { name: 'Pair' }));
    await waitFor(() => getByLabelText('Verification number'));
    expect(attempts).toBe(2);
    const [, init] = fetchMock.mock.calls[1] as unknown as [string, RequestInit];
    expect((JSON.parse(init.body as string) as Record<string, string>).token).toBe('link-token');
  });

  it('offers no retry for a link that cannot work again', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ reason: 'unknown_credential' }), { status: 401 })),
    );
    const { getByRole, findByText, queryByRole } = render(PairingScreen, {
      props: { payload: PAYLOAD, onDone: () => {} },
    });
    await fireEvent.click(getByRole('button', { name: 'Pair' }));
    await findByText('This pairing could not be completed.');
    expect(queryByRole('button', { name: 'Try again' })).toBeNull();
    expect(queryByRole('button', { name: 'Start over' })).toBeNull();
  });

  it('starts over on the shell by closing the slot the failed link opened and reloading to the scan screen', async () => {
    vi.stubGlobal('Capacitor', { isNativePlatform: () => true, getPlatform: () => 'android' });
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ reason: 'unknown_credential' }), { status: 401 })),
    );
    const reload = vi.spyOn(location, 'reload').mockImplementation(() => {});
    location.hash = '#pair=abc';
    const { getByRole, findByText, queryByRole } = render(PairingScreen, {
      props: { payload: PAYLOAD, backend: 'backend-1', onDone: () => {} },
    });
    await fireEvent.click(getByRole('button', { name: 'Pair' }));
    await findByText('This pairing could not be completed.');
    expect(queryByRole('button', { name: 'Try again' })).toBeNull();
    // The shell adopted the link's address into a slot of its own before
    // the refusal; a boot that finds it would attach a computer that
    // never confirmed instead of showing the scan screen.
    expect(storedBackendEndpoint('backend-1')).toBe('http://localhost:3000');
    await fireEvent.click(getByRole('button', { name: 'Start over' }));
    expect(storedBackendEndpoint('backend-1')).toBe('');
    expect(location.hash).toBe('');
    expect(reload).toHaveBeenCalledTimes(1);
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
