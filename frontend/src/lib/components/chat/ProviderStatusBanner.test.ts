import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import ProviderStatusBanner from './ProviderStatusBanner.svelte';
import {
  resetForTest as resetProviderStatuses,
  setupProviderStatusListener,
  type ProviderStatusEvent,
} from '../../stores/providerStatus.svelte';
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
    cleanupStatus = setupProviderStatusListener();
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

  it('renders a session-error banner from pane.error and can dismiss it', async () => {
    const pane = await buildPane();
    pane.setError('session exploded');

    const { getByText, queryByText } = render(ProviderStatusBanner, { props: { pane } });
    expect(getByText('session exploded')).toBeInTheDocument();

    await fireEvent.click(getByText('Dismiss'));
    expect(pane.error).toBeNull();
    expect(queryByText('session exploded')).toBeNull();
  });

  it('reconnects through the binding from the session banner', async () => {
    const pane = await buildPane();
    pane.setError('session exploded');
    const reconnect = setBindingMock('ReconnectSession', async () => {});

    const { getByText } = render(ProviderStatusBanner, { props: { pane } });
    await fireEvent.click(getByText('Reconnect'));

    expect(reconnect).toHaveBeenCalledWith('thread-1');
  });
});
