import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import SystemStatsFooter from '../SystemStatsFooter.svelte';
import { stageBackend, resetStagedBackends } from '../../../../test/helpers/backends';
import { setSystemStats, resetForTest } from '../../../stores/systemStats.svelte';
import { resetTelemetryForTest } from '../../../stores/telemetryComputers.svelte';
import { setSelectedBackend, __resetSelectedBackendForTest } from '../../../stores/selectedBackend.svelte';

beforeEach(() => { resetForTest(); resetTelemetryForTest(); });
afterEach(() => { resetForTest(); resetTelemetryForTest(); resetStagedBackends(); __resetSelectedBackendForTest(); });
const sample = { isWsl: false, cpuPercent: 20, memUsedBytes: 4 * 1024 ** 3, memTotalBytes: 16 * 1024 ** 3 };

describe('system stats footer', () => {
  it('defaults to local and allows showing multiple named hosts without following thread focus', async () => {
    const gpu = stageBackend({ id: 'gpu', name: 'GPU computer' });
    setSystemStats(sample);
    setSystemStats({ ...sample, cpuPercent: 90 }, 'gpu');
    setSelectedBackend('gpu');
    const view = render(SystemStatsFooter);
    expect(view.getByTestId('sidebar-system-stats')).toHaveTextContent('CPU 20%');
    expect(view.queryByText(/90%/)).toBeNull();
    await fireEvent.click(view.getByRole('button', { name: 'Computers shown in system stats' }));
    await fireEvent.click(await view.findByRole('menuitemcheckbox', { name: 'GPU computer' }));
    await waitFor(() => expect(view.getAllByTestId('system-stats-computer')).toHaveLength(2));
    expect(view.getByTestId('sidebar-system-stats')).toHaveTextContent('GPU computer');
    expect(view.getByTestId('sidebar-system-stats')).toHaveTextContent('CPU 90%');
    gpu.setStatus('reconnecting');
    await waitFor(() => expect(view.getByTestId('sidebar-system-stats')).toHaveTextContent('Offline'));
    gpu.setStatus('connected');
    await waitFor(() => expect(view.getByTestId('sidebar-system-stats')).toHaveTextContent('Waiting for system stats'));
    expect(view.queryByText(/90%/)).toBeNull();
  });
});
