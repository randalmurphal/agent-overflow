import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { stageBackend, resetStagedBackends, REMOTE_BACKEND_UUID } from '../../test/helpers/backends';
import { setSelectedBackend, __resetSelectedBackendForTest } from './selectedBackend.svelte';
import { resetTelemetryForTest, selectedTelemetryComputers, setTelemetrySelection, telemetrySelection, toggleTelemetryComputer } from './telemetryComputers.svelte';
import { detachBackend } from '../transport/backends';

beforeEach(resetTelemetryForTest);
afterEach(() => { resetTelemetryForTest(); resetStagedBackends(); __resetSelectedBackendForTest(); });

describe('telemetry computer choices', () => {
  it('defaults usage to all computers and system stats to the local host, independent of thread selection', () => {
    stageBackend({ id: 'gpu', name: 'GPU' });
    setSelectedBackend('gpu');
    expect(selectedTelemetryComputers('usage').map((c) => c.key)).toEqual(['', 'gpu']);
    expect(selectedTelemetryComputers('system').map((c) => c.key)).toEqual(['']);
  });
  it('persists multiple choices per frontend, keeps offline choices and refuses an empty selection', () => {
    const gpu = stageBackend({ id: 'gpu', name: 'GPU' });
    toggleTelemetryComputer('system', 'gpu');
    expect(telemetrySelection('system')).toEqual(['@local', REMOTE_BACKEND_UUID]);
    expect(JSON.parse(localStorage.getItem('agent-overflow:frontend:system-stats-computers')!)).toEqual(['@local', REMOTE_BACKEND_UUID]);
    gpu.setStatus('reconnecting');
    expect(selectedTelemetryComputers('system').find((c) => c.key === 'gpu')?.connected).toBe(false);
    toggleTelemetryComputer('system', '');
    toggleTelemetryComputer('system', 'gpu');
    expect(telemetrySelection('system')).toEqual([REMOTE_BACKEND_UUID]);
    setTelemetrySelection('usage', ['gpu']);
    expect(selectedTelemetryComputers('usage').map((c) => c.key)).toEqual(['gpu']);
    detachBackend('gpu');
    expect(selectedTelemetryComputers('usage')).toEqual([]);
    expect(telemetrySelection('usage')).toEqual([REMOTE_BACKEND_UUID]);
    setTelemetrySelection('usage', null);
    expect(selectedTelemetryComputers('usage').map((c) => c.key)).toEqual(['']);
  });
});
