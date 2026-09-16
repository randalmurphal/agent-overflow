import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import BrowserLockSection from './BrowserLockSection.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { browserLock, installBrowserLock } from '../../stores/browserLock.svelte';
import { setPasskeysAvailableFromBootstrap } from '../../transport/passkey';
import { BROWSER_LOCK_KEY } from '../../utils/browserLock';

let dispose: (() => void) | undefined;
const originalCredentials = Object.getOwnPropertyDescriptor(navigator, 'credentials');
beforeEach(() => {
  localStorage.clear();
  resetBindingMocks();
  resetToLocalPage();
  setPasskeysAvailableFromBootstrap(false);
});
afterEach(() => {
  cleanup();
  dispose?.();
  dispose = undefined;
  vi.unstubAllGlobals();
  if (originalCredentials) Object.defineProperty(navigator, 'credentials', originalCredentials);
  else Reflect.deleteProperty(navigator, 'credentials');
});

it('does not add a lock control to the host’s local development surface', () => {
  expect(render(BrowserLockSection).queryByRole('switch')).toBeNull();
});

it('explains unavailable setup and leaves ordinary paired LAN access usable', async () => {
  await pairViewOnly();
  const view = render(BrowserLockSection);
  expect(view.getByRole('switch')).toBeDisabled();
  expect(view.getByText(/configured HTTPS passkey domain/)).toBeTruthy();
  expect(browserLock.locked).toBe(false);
});

it('lets a view-only browser enable after verification and disable without changing its grants', async () => {
  await pairViewOnly();
  const saved = localStorage.getItem('agent-overflow:deviceSession');
  vi.stubGlobal('PublicKeyCredential', class {});
  Object.defineProperty(navigator, 'credentials', { configurable: true, value: {
    get: vi.fn().mockResolvedValue({
      id: 'credential', rawId: new ArrayBuffer(1), type: 'public-key',
      response: { clientDataJSON: new ArrayBuffer(1), authenticatorData: new ArrayBuffer(1), signature: new ArrayBuffer(1) },
      getClientExtensionResults: () => ({}),
    }),
    create: vi.fn(),
  } });
  setPasskeysAvailableFromBootstrap(true);
  setBindingMock('BeginPasskeyStepUp', async () => ({ ceremonyId: 'unlock', options: { challenge: 'AA' } }));
  const verify = setBindingMock('VerifyBrowserUnlock', async () => undefined);
  dispose = installBrowserLock(() => {});
  const view = render(BrowserLockSection);
  await fireEvent.click(view.getByRole('switch'));
  await waitFor(() => expect(view.getByRole('switch')).toHaveAttribute('aria-checked', 'true'));
  expect(verify).toHaveBeenCalledTimes(1);
  expect(verify).toHaveBeenCalledWith('unlock', expect.objectContaining({ id: 'credential' }));
  expect(localStorage.getItem(BROWSER_LOCK_KEY)).toBe('enabled');
  expect(localStorage.getItem('agent-overflow:deviceSession')).toBe(saved);
  await fireEvent.click(view.getByRole('switch'));
  expect(localStorage.getItem(BROWSER_LOCK_KEY)).toBeNull();
  expect(browserLock.locked).toBe(false);
});
