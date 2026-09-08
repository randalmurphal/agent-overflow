import { afterEach, beforeEach, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { cleanup, fireEvent, render, waitFor, within } from '@testing-library/svelte';
import '../../../app.css';
import SystemStatsFooter from '../sidebar/SystemStatsFooter.svelte';
import UsageModal from './UsageModal.svelte';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { setSystemStats, resetForTest } from '../../stores/systemStats.svelte';
import { resetTelemetryForTest, setTelemetrySelection } from '../../stores/telemetryComputers.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket } from '../../stores/bindings';

beforeEach(() => { resetTelemetryForTest(); resetForTest(); __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null }); });
afterEach(() => { cleanup(); resetTelemetryForTest(); resetStagedBackends(); resetForTest(); });

it.each([360, 1280])('fits multiple named computer stats and usage filters at %ipx', async (width) => {
  await page.viewport(width, 800);
  stageBackend({ id: 'gpu', name: 'GPU computer with a very long friendly descriptive nickname' });
  setTelemetrySelection('system', ['', 'gpu']);
  const stats = { cpuPercent: 100, memUsedBytes: 1024 ** 4, memTotalBytes: 2 * 1024 ** 4, isWsl: true };
  setSystemStats(stats);
  setSystemStats(stats, 'gpu');
  const host = document.createElement('div');
  host.style.width = '280px';
  document.body.append(host);
  const footer = render(SystemStatsFooter, { target: host });
  for (const element of footer.container.querySelectorAll('[data-testid="system-stats-computer"] span')) {
    const rect = element.getBoundingClientRect();
    expect(rect.left).toBeGreaterThanOrEqual(0);
    expect(rect.right).toBeLessThanOrEqual(280);
  }
  await fireEvent.click(footer.getByRole('button', { name: 'Computers shown in system stats' }));
  const option = await within(document.body).findByRole('menuitemcheckbox', { name: /GPU computer/ });
  expect(option.getBoundingClientRect().right).toBeLessThanOrEqual(width);
  await fireEvent.click(option);
  await waitFor(() => expect(footer.getAllByTestId('system-stats-computer')).toHaveLength(1));
  footer.unmount();
  host.remove();

  setBindingMock('GetUsageStats', async () => [new UsageBucket({ outputTokens: 1000, costUsd: 1 })]);
  const modal = render(UsageModal, { open: true, onClose() {} });
  await waitFor(() => expect(modal.getAllByTestId('usage-totals-value').some((el) => el.textContent?.includes('2.00'))).toBe(true));
  const dialog = modal.getByRole('dialog');
  expect(dialog.scrollWidth).toBeLessThanOrEqual(dialog.clientWidth + 1);
  for (const element of [modal.getByRole('button', { name: 'Computers included in usage' }), modal.getByTestId('usage-project-select'), ...modal.getAllByTestId('usage-totals-value')]) {
    const rect = element.getBoundingClientRect();
    expect(rect.left).toBeGreaterThanOrEqual(0);
    expect(rect.right).toBeLessThanOrEqual(width);
  }
});
