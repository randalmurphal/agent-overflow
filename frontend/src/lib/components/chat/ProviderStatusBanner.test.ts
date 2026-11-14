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
      expect(banner.textContent).toContain('Restart the session to use it');
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
      expect(text).toContain('Claude CLI updated. Restart the session to use it');
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

    it('dismisses through the backend and drops the row', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      const dismiss = setBindingMock('DismissSessionBanner', async () => {});
      emitStale();

      const { getByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('provider-status-dismiss'));

      expect(dismiss).toHaveBeenCalledWith('thread-1', 'binary_stale');
      await waitFor(() => expect(queryByTestId('provider-status-banner')).toBeNull());
    });
  });

  // `prompt_stale`: an override file the running session launched with was
  // edited (or its entry toggled or rescoped), so the next session start
  // would use a different system prompt. Same row shape as binary_stale.
  describe('prompt_stale', () => {
    function emitPromptStale(overrides: Record<string, unknown> = {}): void {
      emitWailsEvent('provider:status', {
        kind: 'prompt_stale',
        provider: 'claude',
        threadId: 'thread-1',
        actionable: true,
        revision: 1,
        ...overrides,
      } as unknown as ProviderStatusEvent);
    }

    it('renders its own copy with a restart action', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitPromptStale();

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const banner = getByTestId('provider-status-banner');
      expect(banner.dataset.kind).toBe('prompt_stale');
      expect(banner.textContent).toContain('System prompt changed. Restart the session to use it.');
      expect(getByTestId('provider-status-restart').textContent).toContain('Restart session');
    });

    it('restarts the session through the existing reconnect binding', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      const reconnect = setBindingMock('ReconnectSession', async () => {});
      vi.spyOn(pane, 'refreshFromBackend').mockResolvedValue();
      emitPromptStale();

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('provider-status-restart'));
      expect(reconnect).toHaveBeenCalledWith('thread-1');
    });

    it('dismisses through the backend and shows again on a later change', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      const dismiss = setBindingMock('DismissSessionBanner', async () => {});
      emitPromptStale();

      const { getByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('provider-status-dismiss'));
      expect(dismiss).toHaveBeenCalledWith('thread-1', 'prompt_stale');
      await waitFor(() => expect(queryByTestId('provider-status-banner')).toBeNull());

      emitPromptStale({ revision: 2 });
      await waitFor(() => expect(getByTestId('provider-status-banner').dataset.kind).toBe('prompt_stale'));
    });

    it('keeps a failed dismiss visible as a pane error', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      setBindingMock('DismissSessionBanner', async () => {
        throw new Error('backend away');
      });
      emitPromptStale();

      const { getByTestId, getAllByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('provider-status-dismiss'));
      await waitFor(() => expect(pane.generalError).toBe('Backend away.'));
      expect(getAllByTestId('provider-status-banner')).toHaveLength(1);
    });

    // Both claims can be true of one session: each gets its own row, the
    // prompt one first, and dismissing one leaves the other.
    it('stacks beside a stale binary and dismisses independently', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      setBindingMock('DismissSessionBanner', async () => {});
      emitWailsEvent('provider:status', {
        kind: 'binary_stale',
        provider: 'claude',
        threadId: 'thread-1',
        actionable: true,
        sessionVersion: '2.1.219',
        installedVersion: '2.1.257',
      } as unknown as ProviderStatusEvent);
      emitPromptStale();

      const { getAllByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const rows = getAllByTestId('provider-status-banner');
      expect(rows.map((row) => row.dataset.kind)).toEqual(['prompt_stale', 'binary_stale']);

      await fireEvent.click(getAllByTestId('provider-status-dismiss')[0]);
      await waitFor(() =>
        expect(getAllByTestId('provider-status-banner').map((row) => row.dataset.kind)).toEqual(['binary_stale']),
      );
    });
  });

  // Thread errors and notices the backend could not save to history arrive
  // live on `thread:unsaved_rows`, each frame the thread's whole list. They
  // read as not saved, say why, keep their own tone, outlive the session,
  // and clear only on dismiss.
  describe('unsaved rows', () => {
    type Entry = { id: string; message: string; saveError: string; severity?: string };
    function emitUnsaved(rows: Entry[], threadId = 'thread-1', omitted = 0): void {
      emitWailsEvent('thread:unsaved_rows', {
        threadId,
        omitted,
        rows: rows.map((entry) => ({ severity: 'error', ...entry, occurredAt: 1 })),
      });
    }

    it('renders each entry marked as not saved, with why', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitUnsaved(
        [
          { id: 'unsaved-1', message: 'session could not be resumed: boom', saveError: 'database or disk is full' },
          { id: 'unsaved-2', message: 'discussion sync failed', saveError: 'disk I/O error' },
        ],
        'thread-1',
        3,
      );

      const { getByTestId, getAllByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const banner = getByTestId('unsaved-rows-banner');
      expect(banner.textContent).toContain("Not saved to this thread's history");
      const rows = getAllByTestId('unsaved-row');
      expect(rows).toHaveLength(2);
      expect(rows[0].textContent).toContain('session could not be resumed: boom');
      expect(rows[0].textContent).toContain('Could not save: database or disk is full');
      expect(rows[1].textContent).toContain('Could not save: disk I/O error');
      expect(getByTestId('unsaved-rows-omitted').textContent).toContain('3 earlier entries not shown.');
      // Not a saved error row and not a session claim.
      expect(queryByTestId('pane-error-banner')).toBeNull();
      expect(queryByTestId('provider-status-banner')).toBeNull();
    });

    it('keeps the error tone for errors and the warning tone for notices', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitUnsaved([
        { id: 'error:1:0', message: 'reconnect failed', saveError: 'full', severity: 'error' },
        { id: 'notification:1:0', message: 'Kept worktree feature.', saveError: 'full', severity: 'warning' },
      ]);

      const { getByTestId, getAllByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const [error, notice] = getAllByTestId('unsaved-row');
      expect(error.dataset.severity).toBe('error');
      expect(error.className).toContain('text-error');
      expect(notice.dataset.severity).toBe('warning');
      expect(notice.className).toContain('text-warning');
      // The frame takes the most severe entry's tone.
      expect(getByTestId('unsaved-rows-banner').className).toContain('bg-error/15');
    });

    it('frames a list of notices in the warning tone', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitUnsaved([{ id: 'session_died:2', message: 'Provider session exited with code 1', saveError: 'full', severity: 'warning' }]);

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      const banner = getByTestId('unsaved-rows-banner');
      expect(banner.className).toContain('bg-warning/15');
      expect(banner.className).not.toContain('bg-error/15');
    });

    it("ignores another thread's list", async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitUnsaved([{ id: 'unsaved-1', message: 'elsewhere', saveError: 'full' }], 'thread-2');

      const { queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      expect(queryByTestId('unsaved-rows-banner')).toBeNull();
    });

    // The backend answers a dismiss with the push of what remains, before
    // the RPC response; the push is what clears the screen.
    it('dismisses the displayed rows by id through the backend', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      const dismiss = setBindingMock('DismissUnsavedThreadRows', async () => {
        emitUnsaved([]);
      });
      emitUnsaved([
        { id: 'unsaved-1', message: 'boom', saveError: 'full' },
        { id: 'unsaved-2', message: 'bang', saveError: 'full' },
      ]);

      const { getByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('unsaved-rows-dismiss'));
      expect(dismiss).toHaveBeenCalledWith('thread-1', ['unsaved-1', 'unsaved-2']);
      await waitFor(() => expect(queryByTestId('unsaved-rows-banner')).toBeNull());
    });

    // A row that arrives after the click is not named and its push is the
    // newest state, so the RPC response does not clear it.
    it('keeps a row pushed between the dismiss and its response', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      let respond: (() => void) | null = null;
      setBindingMock('DismissUnsavedThreadRows', () =>
        new Promise<void>((resolve) => {
          respond = resolve;
        }),
      );
      emitUnsaved([{ id: 'unsaved-1', message: 'boom', saveError: 'full' }]);

      const { getByTestId, getAllByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('unsaved-rows-dismiss'));
      await waitFor(() => expect(respond).not.toBeNull());
      emitUnsaved([{ id: 'unsaved-2', message: 'arrived after the click', saveError: 'full' }]);
      respond!();
      // Let the click handler run past its await.
      await new Promise((resolve) => setTimeout(resolve, 0));
      await tick();

      const rows = getAllByTestId('unsaved-row');
      expect(rows).toHaveLength(1);
      expect(rows[0].textContent).toContain('arrived after the click');
    });

    it('clears when another screen dismisses', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitUnsaved([{ id: 'unsaved-1', message: 'boom', saveError: 'full' }]);
      const { queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
      expect(queryByTestId('unsaved-rows-banner')).not.toBeNull();

      emitUnsaved([]);
      await waitFor(() => expect(queryByTestId('unsaved-rows-banner')).toBeNull());
    });

    it('stays when the session disconnects', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      emitUnsaved([{ id: 'unsaved-1', message: 'boom', saveError: 'full' }]);
      const { queryByTestId } = render(ProviderStatusBanner, { props: { pane } });

      emitWailsEvent('provider:session_account', { threadId: 'thread-1', connected: false });
      await tick();
      expect(queryByTestId('unsaved-rows-banner')).not.toBeNull();
    });

    it('keeps a failed dismiss visible as a pane error', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-1', provider: 'claude' }));
      setBindingMock('DismissUnsavedThreadRows', async () => {
        throw new Error('backend away');
      });
      emitUnsaved([{ id: 'unsaved-1', message: 'boom', saveError: 'full' }]);

      const { getByTestId } = render(ProviderStatusBanner, { props: { pane } });
      await fireEvent.click(getByTestId('unsaved-rows-dismiss'));
      await waitFor(() => expect(pane.generalError).toBe('Backend away.'));
      expect(getByTestId('unsaved-rows-banner')).toBeInTheDocument();
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

  it('offers details and a copy for an error row that carries its failure', async () => {
    setBindingMock('Version', async () => '1.2.3');
    const pane = await buildPane();
    pane.setGeneralError('Could not fork the thread', new Error('copy session', { cause: new Error('disk full') }));
    pane.setHistoryLoadError('Failed to load thread items');

    const { getAllByTestId, getByTestId, queryByTestId } = render(ProviderStatusBanner, { props: { pane } });
    const [historyRow, generalRow] = getAllByTestId('pane-error-banner');
    expect(historyRow.querySelector('[aria-label="Copy error for an agent"]')).toBeNull();
    expect(generalRow.querySelector('[aria-label="Copy error for an agent"]')).not.toBeNull();

    await fireEvent.click(getByTestId('error-details-toggle'));
    expect(getByTestId('error-details').textContent).toContain('disk full');

    await fireEvent.click(generalRow.querySelector('[aria-label="Dismiss Banner"]')!);
    await waitFor(() => expect(queryByTestId('error-details')).toBeNull());
  });
});

