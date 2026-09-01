import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ProviderStatusBanner from './ProviderStatusBanner.svelte';
import {
  resetForTest as resetProviderStatuses,
  type ProviderStatusEvent,
} from '../../stores/providerStatus.svelte';
import { setupEventListeners } from '../../stores/events';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { emitWailsEvent, resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

function statusEvent(overrides: Partial<ProviderStatusEvent> = {}): ProviderStatusEvent {
  return {
    provider: 'claude',
    status: 'not_found',
    message: 'Claude CLI not found',
    actionable: true,
    actionUrl: 'https://example.com/install',
    ...overrides,
  };
}

describe('<ProviderStatusBanner>', () => {
  let cleanupStatus: () => void;

  beforeEach(() => {
    resetWailsMocks();
    resetBindingMocks();
    resetProviderStatuses();
    cleanupStatus = setupEventListeners();
  });

  afterEach(() => {
    cleanupStatus();
  });

  it('renders provider-level status events', async () => {
    const pane = await buildPane();
    emitWailsEvent('provider:status', statusEvent());

    const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });

    expect(getByTestId('provider-status-banner').textContent).toContain('Claude CLI not found');
  });

  it('renders a session-error banner from pane.generalError and can dismiss it', async () => {
    const pane = await buildPane();
    pane.setGeneralError('session exploded');

    const { getByText, queryByText } = render(ProviderStatusBanner, { props: { pane } });
    expect(getByText('session exploded')).toBeInTheDocument();

    await fireEvent.click(getByText('Dismiss'));
    expect(pane.generalError).toBeNull();
    expect(queryByText('session exploded')).toBeNull();
  });

  it('does not offer provider reconnect for an unrelated pane error', async () => {
    const pane = await buildPane();
    pane.setGeneralError('rename failed');

    const { queryByText } = render(ProviderStatusBanner, { props: { pane } });
    expect(queryByText('Reconnect')).toBeNull();
    expect(queryByText('Retry')).toBeNull();
  });

  it('reconnects through the binding from the session banner', async () => {
    const pane = await buildPane();
    pane.setSessionError('session exploded');
    const reconnect = setBindingMock('ReconnectSession', async () => {});

    const { getByText } = render(ProviderStatusBanner, { props: { pane } });
    await fireEvent.click(getByText('Reconnect'));

    expect(reconnect).toHaveBeenCalledWith('thread-1');
  });

  // Regression: a successful Reconnect must also pull the backend's
  // current state into the pane. The backend's CleanupThread synthesizes
  // a truncated provider:turn_completed, but events that fire during the
  // round-trip can race the pane's reactive state — without the refresh,
  // a user who had a stuck working indicator before reconnecting can
  // still see stale activeTurn / streaming flags afterward.
  it('refreshes pane state from backend after a successful reconnect', async () => {
    const pane = await buildPane();
    pane.setSessionError('session exploded');
    setBindingMock('ReconnectSession', async () => {});
    const refresh = vi.spyOn(pane, 'refreshFromBackend').mockResolvedValue();

    const { getByText } = render(ProviderStatusBanner, { props: { pane } });
    await fireEvent.click(getByText('Reconnect'));

    await waitFor(() => {
      expect(refresh).toHaveBeenCalledOnce();
    });
  });

  it('offers an in-place retry for a history-load error', async () => {
    const pane = await buildPane();
    pane.setHistoryLoadError('Thread history took too long to load.');
    const retry = vi.spyOn(pane, 'retryHistoryLoad').mockResolvedValue();

    const { getByText, queryByText } = render(ProviderStatusBanner, { props: { pane } });
    expect(queryByText('Reconnect')).toBeNull();
    await fireEvent.click(getByText('Retry'));

    expect(retry).toHaveBeenCalledOnce();
  });

  // Regression: when the backend emits `status='not_found'` without an
  // `actionUrl`, the "Install Claude/Codex CLI" button used to render but
  // silently no-op on click (handlePrimaryAction falls through with no
  // branch that matches). Hide the affordance entirely instead so we
  // don't lie to the user.
  it('omits the Install button when the not-found event has no actionUrl', async () => {
    const pane = await buildPane();
    emitWailsEvent(
      'provider:status',
      statusEvent({ actionUrl: '', message: 'Claude CLI not found' }),
    );

    const { queryByTestId, getByTestId } = render(ProviderStatusBanner, { props: { pane } });
    expect(getByTestId('provider-status-banner').textContent).toContain('Claude CLI not found');
    expect(queryByTestId('provider-status-action')).toBeNull();
  });

  it('still renders the Install button when actionUrl is present', async () => {
    const pane = await buildPane();
    emitWailsEvent('provider:status', statusEvent());

    const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
    expect(getByTestId('provider-status-action').textContent).toMatch(/Install Claude CLI/);
  });

  it('rechecks the matching provider account from unauthenticated banners', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex', model: 'gpt-5.4-mini' }));
    const recheckClaude = setBindingMock('RecheckClaudeAccount', async () => {});
    const recheckCodex = setBindingMock('RecheckCodexAccount', async () => {});
    emitWailsEvent(
      'provider:status',
      statusEvent({
        provider: 'codex',
        status: 'unauthenticated',
        message: 'Codex authentication expired',
        actionUrl: '',
      }),
    );

    const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
    await fireEvent.click(getByTestId('provider-status-action'));

    expect(recheckCodex).toHaveBeenCalledOnce();
    expect(recheckClaude).not.toHaveBeenCalled();
  });

  it('clears the unauthenticated banner after a successful provider recheck', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex', model: 'gpt-5.4-mini' }));
    setBindingMock('RecheckCodexAccount', async () => ({
      subscriptionType: 'pro',
      tokenSource: 'login',
    }));
    emitWailsEvent(
      'provider:status',
      statusEvent({
        provider: 'codex',
        status: 'unauthenticated',
        message: 'Codex authentication expired',
        actionUrl: '',
      }),
    );

    const { getByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
    await fireEvent.click(getByTestId('provider-status-action'));

    await waitFor(() => {
      expect(queryByTestId('provider-status-banner')).toBeNull();
    });
  });

  // The provider CLI was replaced under a live session. Warning severity,
  // and the action is the existing reconnect — nothing is broken, the
  // session is just pinned to the binary it started on.
  describe('binary_stale', () => {
    function emitStale(overrides: Record<string, unknown> = {}): void {
      emitWailsEvent('provider:status', {
        kind: 'binary_stale',
        provider: 'claude',
        threadId: 'thread-1',
        // The backend marks this actionable — the action is a button
        // (ReconnectSession), not a URL, so there is no actionUrl and no
        // primary-action label for it.
        actionable: true,
        sessionVersion: '2.1.219',
        installedVersion: '2.1.257',
        ...overrides,
      } as unknown as ProviderStatusEvent);
    }

    it('renders both versions with a restart action', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitStale();

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const banner = getByTestId('provider-status-banner');

      expect(banner.dataset.status).toBe('binary_stale');
      expect(banner.textContent).toContain('Claude CLI updated (2.1.219 → 2.1.257)');
      expect(banner.textContent).toContain('restart the session to use it');
      expect(getByTestId('provider-status-restart').textContent).toContain('Restart session');
    });

    // The provider comes off the event, so a Codex session never reads
    // "Claude CLI".
    it('names the provider the event is for', async () => {
      const pane = await buildPane(
        makeThread({ id: 'thread-1', provider: 'codex', model: 'gpt-5.4-mini' }),
      );
      emitStale({ provider: 'codex' });

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      expect(getByTestId('provider-status-banner').textContent).toContain('Codex CLI updated');
    });

    // Either version can be missing when the backend could not read one.
    // Half an arrow is worse than no parenthetical at all.
    it('degrades the parenthetical when a version is missing', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitStale({ sessionVersion: '' });

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const text = getByTestId('provider-status-banner').textContent ?? '';
      expect(text).toContain('Claude CLI updated (2.1.257)');
      expect(text).not.toContain('→');
    });

    it('drops the parenthetical entirely when neither version is known', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitStale({ sessionVersion: '', installedVersion: '' });

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const text = getByTestId('provider-status-banner').textContent ?? '';
      expect(text).toContain('Claude CLI updated — restart the session to use it');
      expect(text).not.toContain('(');
    });

    it('restarts the session through the existing reconnect binding', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      const reconnect = setBindingMock('ReconnectSession', async () => {});
      vi.spyOn(pane, 'refreshFromBackend').mockResolvedValue();
      emitStale();

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('provider-status-restart'));

      expect(reconnect).toHaveBeenCalledWith('thread-1');
    });

    // No install / recheck affordance leaks onto this row: the only thing
    // to do about a stale binary is restart the session.
    it('offers no other action', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitStale();

      const { queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      expect(queryByTestId('provider-status-recheck')).toBeNull();
      expect(queryByTestId('provider-status-action')).toBeNull();
    });
  });

  // `{#if providerStatus?.actionable && primaryActionLabel}` sits inside
  // `{#if providerStatus && pane.thread}`, and its deref is the FIRST operand
  // of a two-dep condition. Nulling the status in the same flush that moves
  // the action label's inputs is the shape that crashed MessageNavRail on
  // 2026-08-29; the condition has to answer false rather than throw.
  // (Ordinary Svelte flushes are parent-first, so this pins the outcome
  // rather than staging the tree-order violation — `nullableGuardTotality`
  // is what holds the shape.)
  it('survives the status clearing in the same flush that moves the action label', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    emitWailsEvent(
      'provider:status',
      statusEvent({ provider: 'claude', status: 'unauthenticated', actionUrl: '' }),
    );

    const { getByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
    expect(getByTestId('provider-status-action')).toBeInTheDocument();

    // One task, so both writes land in one flush: `ready` nulls
    // providerStatus while a second, unrelated write re-renders the rest of
    // the overlay around the dying branch.
    emitWailsEvent('provider:status', statusEvent({ provider: 'claude', status: 'ready' }));
    pane.setGeneralError('unrelated failure');
    await tick();
    // The banner fades out, so its removal lands a flush after the branch
    // condition answered false. What this pins is the condition itself.
    await waitFor(() => expect(queryByTestId('provider-status-action')).toBeNull());
    expect(queryByTestId('provider-status-banner')).toBeNull();
  });
});
