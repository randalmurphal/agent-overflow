import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { within } from '@testing-library/svelte';
import SystemsSection from './SystemsSection.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetRunMode, setRunMode } from '../../../test/runMode';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { SCOPES } from '../../transport/scopes';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { __resetManifestBackendsForTest } from '../../transport/manifestBackends';
import { __resetPendingAttachmentsForTest } from '../../transport/backendAttach';
import { hasPairedSession } from '../../transport/deviceSession';
import {
  __resetHomeEndpointForTest,
  storeBackendEndpoint,
  storedBackendEndpoint,
} from '../../transport/homeEndpoint';
import { __resetSystemsForTest } from '../../stores/systems.svelte';

const LAPTOP = {
  id: 'laptop',
  backendId: '99999999-8888-4777-8666-555555555555',
  name: 'Laptop',
  nickname: '',
  endpoint: 'https://laptop.example:8123',
  lastReachedMs: 0,
};

describe('<SystemsSection>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    __resetSystemsForTest();
    resetStagedBackends();
    __resetManifestBackendsForTest();
  });

  afterEach(() => {
    cleanup();
    resetRunMode();
    resetStagedBackends();
    __resetManifestBackendsForTest();
  });

  it('lists attached systems with live reachability', async () => {
    const staged = stageBackend({ status: 'reconnecting' });
    setBindingMock('ListBackends', async () => [LAPTOP]);
    const { findByTestId, getByTestId } = render(SystemsSection);
    const row = await findByTestId('attached-system');
    expect(row.textContent).toMatch(/Laptop/);
    expect(row.textContent).toMatch(/Unreachable/);
    staged.setStatus('connected');
    await waitFor(() => expect(getByTestId('attached-system').textContent).toMatch(/Connected/));
  });

  it('shows the empty state once the list has answered', async () => {
    setBindingMock('ListBackends', async () => []);
    const { findByTestId } = render(SystemsSection);
    expect(await findByTestId('systems-empty')).toBeTruthy();
  });

  it('starts a pairing from a link and shows the verification number until confirmed', async () => {
    setBindingMock('ListBackends', async () => []);
    const add = setBindingMock('AddBackend', async () => ({
      id: 'laptop', name: 'Laptop', endpoint: LAPTOP.endpoint, verificationNumber: '73',
    }));
    const { getByLabelText, getByText, findByTestId } = render(SystemsSection);
    await fireEvent.input(getByLabelText('Pairing link'), { target: { value: ' https://laptop.example/pair#t ' } });
    await fireEvent.click(getByText('Connect'));
    await waitFor(() => expect(add).toHaveBeenCalledWith('https://laptop.example/pair#t'));
    const pending = await findByTestId('pending-attachment');
    expect(pending.textContent).toMatch(/Waiting for Laptop/);
    expect(pending.textContent).toMatch(/73/);
  });

  it('detaches only on the second press', async () => {
    stageBackend();
    setBindingMock('ListBackends', async () => [LAPTOP]);
    const remove = setBindingMock('RemoveBackend', async () => {});
    const { getByText, findByTestId, queryByTestId } = render(SystemsSection);
    await fireEvent.click(within(await findByTestId('attached-system')).getByText('Remove'));
    expect(remove).not.toHaveBeenCalled();
    await fireEvent.click(getByText('Confirm remove'));
    await waitFor(() => expect(remove).toHaveBeenCalledWith('laptop'));
    await waitFor(() => expect(queryByTestId('attached-system')).toBeNull());
  });

  it('renames inline on Enter', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    const rename = setBindingMock('RenameBackend', async () => {});
    const { findByText, getByLabelText, findByTestId } = render(SystemsSection);
    await fireEvent.click(await findByText('Rename'));
    const input = getByLabelText('System name') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Work laptop' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(rename).toHaveBeenCalledWith('laptop', 'Work laptop'));
    expect((await findByTestId('attached-system')).textContent).toMatch(/Work laptop/);
  });

  it('explains rather than loading in a --connect window', async () => {
    setRunMode('client');
    const list = setBindingMock('ListBackends', async () => []);
    const { getByTestId, queryByLabelText } = render(SystemsSection);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(list).not.toHaveBeenCalled();
    expect(getByTestId('systems-section-unavailable')).toBeTruthy();
    expect(queryByLabelText('Pairing link')).toBeNull();
  });

  // The other realization. A phone holds the credential itself, so its
  // list is its own transport registry and its removal is the transport's
  // three stores rather than an RPC. It holds no `host` scope and must
  // ask this backend for nothing at all — the passive-load rule read from
  // the side where the answer is "there is nothing to ask".
  describe('on the phone shell', () => {
    beforeEach(async () => {
      localStorage.clear();
      __resetHomeEndpointForTest();
      __resetPendingAttachmentsForTest();
      vi.stubGlobal('Capacitor', { isNativePlatform: () => true, getPlatform: () => 'android' });
      // A phone is a paired device: full access, and never `host`.
      await pairWithScopes(SCOPES);
    });

    afterEach(() => {
      vi.unstubAllGlobals();
      resetToLocalPage();
      __resetPendingAttachmentsForTest();
      __resetHomeEndpointForTest();
      localStorage.clear();
    });

    it('lists the machines it attached, with live reachability, and asks the backend nothing', async () => {
      storeBackendEndpoint('laptop', LAPTOP.endpoint);
      const staged = stageBackend({ status: 'reconnecting' });
      const list = setBindingMock('ListBackends', async () => [LAPTOP]);
      const { findByTestId, getByTestId } = render(SystemsSection);

      const row = await findByTestId('attached-machine');
      expect(row.textContent).toMatch(/Laptop/);
      expect(row.textContent).toMatch(/laptop\.example:8123/);
      expect(row.textContent).toMatch(/Unreachable/);
      staged.setStatus('connected');
      await waitFor(() =>
        expect(getByTestId('attached-machine').textContent).toMatch(/Connected/),
      );
      // The host list is the desktop's profiles. A phone asking for it
      // would spend one refusal per open.
      expect(list).not.toHaveBeenCalled();
    });

    it('detaches on the second press, taking the socket, the credential and the address', async () => {
      storeBackendEndpoint('laptop', LAPTOP.endpoint);
      localStorage.setItem(
        'agent-overflow:deviceSession:laptop',
        JSON.stringify({ sessionId: 's', credential: 'c', expiresAtMs: Date.now() + 60_000 }),
      );
      stageBackend();
      const { getByText, getByTestId, queryByTestId } = render(SystemsSection);

      await fireEvent.click(within(getByTestId('attached-machine')).getByText('Remove'));
      expect(queryByTestId('attached-machine')).not.toBeNull();
      await fireEvent.click(getByText('Confirm remove'));

      await waitFor(() => expect(queryByTestId('attached-machine')).toBeNull());
      expect(hasPairedSession('laptop')).toBe(false);
      expect(storedBackendEndpoint('laptop')).toBe('');
    });
  });
});
