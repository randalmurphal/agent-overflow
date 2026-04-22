import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import ProviderStatusBanner from './ProviderStatusBanner.svelte';
import {
  resetForTest as resetProviderStatuses,
  type ProviderStatusEvent,
} from '../../stores/providerStatus.svelte';
import { setupEventListeners } from '../../stores/events';
import { buildPane } from '../../../test/helpers/chat';
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

  it('reconnects through the binding from the session banner', async () => {
    const pane = await buildPane();
    pane.setGeneralError('session exploded');
    const reconnect = setBindingMock('ReconnectSession', async () => {});

    const { getByText } = render(ProviderStatusBanner, { props: { pane } });
    await fireEvent.click(getByText('Reconnect'));

    expect(reconnect).toHaveBeenCalledWith('thread-1');
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
});
