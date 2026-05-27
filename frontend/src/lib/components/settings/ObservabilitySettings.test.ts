import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ObservabilitySettings from './ObservabilitySettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';

const BASE_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  sansFont: 'geist',
  monoFont: 'geist',
  fontSize: 13,
  recentWorkspaces: [],
  diffWordWrap: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  defaultThreadEnvMode: 'local',
  worktreeBranchPrefix: 'ao-',
  paneDensity: 'compact',
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
  claudeAutoCompactStandardPercent: 90,
  claudeAutoCompactExtendedPercent: 90,
  codexAutoCompactStandardPercent: 90,
  codexAutoCompactExtendedPercent: 90,
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
  network: { bindAll: false },
  retention: { days: 30 },
  gitlabSelfHostedHosts: [],
  projectSortMode: 'lastActivity',
  collapsedProjects: [],
  paneLayout: { version: 1, panes: [], focusedPaneId: null },
};

async function seed(overrides: Partial<Settings> = {}): Promise<void> {
  const merged = { ...BASE_SETTINGS, ...overrides };
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

  it('shows a restart banner when tracing toggle diverges from initial state', async () => {
    await seed({ observabilityTracingEnabled: false });
    const { queryByRole, getAllByRole } = render(ObservabilitySettings);
    // Initially no banner: the two states agree.
    expect(queryByRole('status')).toBeNull();

    // Flip the toggle. The optimistic update should push us to "divergent
    // from initial" and the banner should appear.
    const tracingSwitch = getAllByRole('switch')[0];
    await fireEvent.click(tracingSwitch);

    // The banner appears via $derived after the settings store updates.
    // Give Svelte microtasks time to settle.
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(queryByRole('status')).not.toBeNull();
  });
});
