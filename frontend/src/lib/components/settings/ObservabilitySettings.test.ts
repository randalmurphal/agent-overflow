import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ObservabilitySettings from './ObservabilitySettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { HOST_TIER_REASON } from './settingsComputer';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { getToasts, removeToast } from '../../stores/toast.svelte';

const BASE_SETTINGS: Settings = makeSettings();

/**
 * Seeds the saved settings and the backend's tracing answer. `launched` is
 * the value this session's tracing started with; it defaults to the saved
 * one, a session with no change waiting.
 */
async function seed(overrides: Partial<Settings> = {}, launched?: boolean): Promise<void> {
  const merged = { ...BASE_SETTINGS, ...overrides };
  setBindingMock('GetObservabilityTracing', async () => ({
    value: merged.observabilityTracingEnabled,
    launched: launched ?? merged.observabilityTracingEnabled,
  }));
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  await loadSettings();
}

describe('<ObservabilitySettings>', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the tracing toggle and replay toggle', async () => {
    const { getAllByRole } = render(ObservabilitySettings);
    const switches = getAllByRole('switch');
    // We expect exactly two toggles: tracing + event log.
    expect(switches.length).toBe(2);
    expect(switches[0].getAttribute('aria-label')).toBe('Toggle OpenTelemetry tracing');
    expect(switches[1].getAttribute('aria-label')).toBe('Toggle Event Replay Log');
  });

  it('renders every control inert, saying why, off the host without a passkey', async () => {
    // The observability* keys are host tier (internal/settings/tier.go).
    await seed({ observabilityTracingEnabled: true });
    setPageGrantsFromBootstrap(true);
    try {
      const { getAllByRole, getByLabelText } = render(ObservabilitySettings);
      for (const toggle of getAllByRole('switch') as HTMLButtonElement[]) {
        expect(toggle.disabled).toBe(true);
        expect(toggle.title).toBe(HOST_TIER_REASON);
      }
      const input = getByLabelText('OTLP endpoint') as HTMLInputElement;
      expect(input.disabled).toBe(true);
      expect(input.title).toBe(HOST_TIER_REASON);
      await fireEvent.click(getAllByRole('switch')[0]);
      expect(getBindingMock('UpdateSettings')?.mock.calls).toHaveLength(0);
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });

  it('disables the OTLP endpoint input when tracing is off', async () => {
    await seed({ observabilityTracingEnabled: false });
    const { getByLabelText } = render(ObservabilitySettings);
    const input = getByLabelText('OTLP endpoint') as HTMLInputElement;
    expect(input.disabled).toBe(true);
  });

  it('enables the OTLP endpoint input when tracing is on', async () => {
    await seed({ observabilityTracingEnabled: true });
    const { getByLabelText } = render(ObservabilitySettings);
    const input = getByLabelText('OTLP endpoint') as HTMLInputElement;
    expect(input.disabled).toBe(false);
  });

  it('sends the endpoint patch on change', async () => {
    await seed({ observabilityTracingEnabled: true });
    const { getByLabelText } = render(ObservabilitySettings);
    const input = getByLabelText('OTLP endpoint') as HTMLInputElement;

    input.value = 'jaeger:4317';
    await fireEvent.change(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ observabilityOtlpEndpoint: 'jaeger:4317' });
  });

  it('sends the tracing toggle patch when flipped', async () => {
    await seed({ observabilityTracingEnabled: false });
    const { getAllByRole } = render(ObservabilitySettings);
    const tracingSwitch = getAllByRole('switch')[0];
    await fireEvent.click(tracingSwitch);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ observabilityTracingEnabled: true });
  });

  it('sends the event log toggle patch when flipped', async () => {
    await seed({ observabilityEventLogEnabled: false });
    const { getAllByRole } = render(ObservabilitySettings);
    const eventLogSwitch = getAllByRole('switch')[1];
    await fireEvent.click(eventLogSwitch);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ observabilityEventLogEnabled: true });
  });

  const NOTICE = 'Tracing changes take effect on next app restart.';

  it('shows the restart notice while the saved value differs from the launch value', async () => {
    await seed({ observabilityTracingEnabled: false });
    const { queryByText, getAllByRole } = render(ObservabilitySettings);
    await waitFor(() => expect(getBindingMock('GetObservabilityTracing')).toHaveBeenCalled());
    expect(queryByText(NOTICE)).toBeNull();

    const tracingSwitch = getAllByRole('switch')[0];
    await fireEvent.click(tracingSwitch);
    await waitFor(() => expect(queryByText(NOTICE)).not.toBeNull());

    // Back to what this session runs with: nothing is waiting.
    await fireEvent.click(tracingSwitch);
    await waitFor(() => expect(queryByText(NOTICE)).toBeNull());
  });

  it('keeps the restart notice when Settings is closed and reopened before a restart', async () => {
    // Saved on, but this session's tracing started off.
    await seed({ observabilityTracingEnabled: true }, false);
    const { findByText } = render(ObservabilitySettings);
    expect(await findByText(NOTICE)).not.toBeNull();
  });

  it('shows no notice while the launch value is unknown', async () => {
    await seed({ observabilityTracingEnabled: true });
    setBindingMock('GetObservabilityTracing', async () => {
      throw new Error('backend unreachable');
    });
    try {
      const { queryByText, getAllByRole } = render(ObservabilitySettings);
      await waitFor(() =>
        expect(getToasts().some((t) => t.message.includes('backend unreachable'))).toBe(true),
      );
      await fireEvent.click(getAllByRole('switch')[0]);
      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(queryByText(NOTICE)).toBeNull();
    } finally {
      for (const toast of getToasts()) removeToast(toast.id);
    }
  });

  describe('without settings:read', () => {
    afterEach(() => resetToLocalPage());

    it('asks nothing', async () => {
      await pairWithScopes(['threads:read']);
      render(ObservabilitySettings);
      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(getBindingMock('GetObservabilityTracing')).not.toHaveBeenCalled();
    });
  });
});
