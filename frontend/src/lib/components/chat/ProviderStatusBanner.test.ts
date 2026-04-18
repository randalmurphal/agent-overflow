import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ProviderStatusBanner from './ProviderStatusBanner.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import {
  emitWailsEvent,
  resetWailsMocks,
} from '../../../test/mocks/wailsio-runtime';
import {
  setupProviderStatusListener,
  resetForTest,
  type ProviderStatusEvent,
} from '../../stores/providerStatus.svelte';
import type { Thread } from '../../types/models';

// Element.animate polyfill — happy-dom doesn't ship one and
// svelte/transition touches it for the slide transition used in the banner.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Example thread',
    provider: 'claude',
    workspacePath: '/workspace',
    projectPath: '/workspace',
    model: 'claude-sonnet-4-6',
    interactionMode: 'default',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

function statusEvent(overrides: Partial<ProviderStatusEvent> = {}): ProviderStatusEvent {
  return {
    provider: 'claude',
    status: 'not_found',
    message: 'Claude CLI not found at /usr/local/bin/claude.',
    version: '',
    actionable: true,
    actionUrl: 'https://docs.example/install',
    ...overrides,
  };
}

describe('<ProviderStatusBanner>', () => {
  let cleanup: () => void;

  beforeEach(() => {
    resetWailsMocks();
    resetForTest();
    cleanup = setupProviderStatusListener();
  });

  afterEach(() => {
    cleanup();
    resetForTest();
  });

  describe('happy path', () => {
    it('renders nothing when there is no status event and the session is healthy', async () => {
      const pane = await buildPane();
      const { queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      expect(queryByTestId('provider-status-banner')).toBeNull();
    });

    it('renders nothing for a ready status', async () => {
      const pane = await buildPane();
      const { queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', { provider: 'claude', status: 'ready', actionable: false });
      expect(queryByTestId('provider-status-banner')).toBeNull();
    });
  });

  describe('not_found', () => {
    it('renders the provider message and an Install action when status is not_found', async () => {
      const pane = await buildPane();
      const { findByTestId, getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', statusEvent({
        status: 'not_found',
        message: 'Claude CLI not found at /usr/local/bin/claude.',
      }));

      const banner = await findByTestId('provider-status-banner');
      expect(banner.getAttribute('data-status')).toBe('not_found');
      expect(banner.textContent).toContain('Claude CLI not found');

      const button = getByTestId('provider-status-action');
      expect(button.textContent).toContain('Install Claude CLI');
    });

    it('opens the docs URL in a new window when the Install button is clicked', async () => {
      const pane = await buildPane();
      const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null);

      const { findByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', statusEvent({
        status: 'not_found',
        actionUrl: 'https://docs.example/install',
      }));

      const button = await findByTestId('provider-status-action');
      await fireEvent.click(button);

      expect(openSpy).toHaveBeenCalledWith(
        'https://docs.example/install',
        '_blank',
        'noopener,noreferrer',
      );
      openSpy.mockRestore();
    });

    it('uses the Codex label when the pane thread is a Codex thread', async () => {
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { findByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', statusEvent({
        provider: 'codex',
        status: 'not_found',
        message: 'Codex CLI not found at /usr/local/bin/codex.',
      }));
      const button = await findByTestId('provider-status-action');
      expect(button.textContent).toContain('Install Codex CLI');
    });
  });

  describe('version_too_old', () => {
    it('renders the version message with no primary action', async () => {
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { findByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', statusEvent({
        provider: 'codex',
        status: 'version_too_old',
        message: 'Codex CLI v0.36.0 is too old for Agent Overflow. Upgrade to v0.37.0 or newer and restart the app.',
        version: 'codex 0.36.0',
        actionUrl: '',
      }));

      const banner = await findByTestId('provider-status-banner');
      expect(banner.getAttribute('data-status')).toBe('version_too_old');
      expect(banner.textContent).toContain('Upgrade to v0.37.0');
      // version_too_old carries no URL — no Install button.
      expect(queryByTestId('provider-status-action')).toBeNull();
    });
  });

  describe('unauthenticated', () => {
    it('renders the login prompt with a Recheck button that calls ProbeClaudeAccount', async () => {
      const pane = await buildPane();
      const probe = setBindingMock('ProbeClaudeAccount', async () => ({
        subscriptionType: '',
        tokenSource: '',
        apiProvider: '',
        model: '',
        version: '',
      }));

      const { findByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', statusEvent({
        status: 'unauthenticated',
        message: 'Claude is not authenticated. Run `claude login` to sign in.',
        actionUrl: 'https://docs.example/install',
      }));

      const banner = await findByTestId('provider-status-banner');
      expect(banner.textContent).toContain('not authenticated');

      const button = await findByTestId('provider-status-action');
      expect(button.textContent).toContain('Recheck');

      await fireEvent.click(button);
      // Await microtasks so the async onclick handler resolves.
      await Promise.resolve();
      await Promise.resolve();

      expect(probe).toHaveBeenCalledTimes(1);
      // After the probe resolves we also exercise the getBindingMock
      // assertion so a missing export would surface as a test failure.
      expect(getBindingMock('ProbeClaudeAccount')).toBeDefined();
    });

    it('surfaces probe failures without crashing the banner', async () => {
      const pane = await buildPane();
      setBindingMock('ProbeClaudeAccount', async () => { throw new Error('probe failed'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

      const { findByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', statusEvent({ status: 'unauthenticated' }));

      const button = await findByTestId('provider-status-action');
      await fireEvent.click(button);
      await Promise.resolve();
      await Promise.resolve();

      expect(consoleErr).toHaveBeenCalled();
      consoleErr.mockRestore();
    });
  });

  describe('session banner coexistence', () => {
    it('still renders the session banner on session error alongside the provider banner', async () => {
      const pane = await buildPane();
      pane.setSessionStatus('error');
      pane.setError('some session failure');

      const { findByTestId } = render(ProviderStatusBanner, { props: { pane } });
      emitWailsEvent('provider:status', statusEvent({ status: 'not_found' }));

      // Both banners exist simultaneously — the provider banner is the
      // actionable status and the session banner retains the Reconnect
      // flow for transient errors.
      await findByTestId('provider-status-banner');
      const alerts = document.querySelectorAll('[role="alert"]');
      expect(alerts.length).toBe(2);
      // Session banner's "some session failure" text is present too.
      const textContent = Array.from(alerts).map((a) => a.textContent ?? '').join(' ');
      expect(textContent).toContain('some session failure');
      expect(textContent).toContain('Claude CLI not found');
    });

    it('shows only the session banner when no provider:status event is active', async () => {
      const pane = await buildPane();
      pane.setSessionStatus('error');
      pane.setError('some session failure');

      const { queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      expect(queryByTestId('provider-status-banner')).toBeNull();
      const alerts = document.querySelectorAll('[role="alert"]');
      expect(alerts.length).toBe(1);
    });
  });
});
