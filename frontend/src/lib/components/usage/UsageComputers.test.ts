import { afterEach, beforeEach, expect, it } from 'vitest';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import UsageFooter from '../sidebar/UsageFooter.svelte';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { takePinnedBackend } from '../../transport/backends';
import { resetTelemetryForTest } from '../../stores/telemetryComputers.svelte';
import { UsageBucket, CodexAccountUsage, type UsageQuery } from '../../stores/bindings';

beforeEach(resetTelemetryForTest);
afterEach(() => { resetTelemetryForTest(); resetStagedBackends(); });

it('shares the modal multi-computer selection with footer totals and names separate account reports', async () => {
  stageBackend({ id: 'gpu', name: 'Nexus' });
  setBindingMock('GetUsageStats', async (query: UsageQuery) => {
    const owner = takePinnedBackend();
    if (query.groupBy === 'project' || query.groupBy === 'day') return [];
    return [new UsageBucket({ bucket: query.groupBy === 'provider' ? 'codex' : '', outputTokens: owner === 'gpu' ? 200 : 100, costUsd: owner === 'gpu' ? 2 : 1 })];
  });
  setBindingMock('GetCodexAccountUsage', async () => {
    const owner = takePinnedBackend();
    return new CodexAccountUsage({ accountEmail: owner === 'gpu' ? 'gpu@example.test' : 'local@example.test' });
  });
  const view = render(UsageFooter);
  await waitFor(() => expect(view.getByTestId('usage-footer-row')).toHaveTextContent('300 · $3.00'));
  await fireEvent.click(view.getByTestId('sidebar-usage-footer'));
  const dialog = await view.findByRole('dialog');
  await fireEvent.click(within(dialog).getByRole('radio', { name: 'Codex' }));
  await waitFor(() => expect(within(dialog).getAllByTestId('usage-codex-account')).toHaveLength(2));
  expect(within(dialog).getByText('Codex Account · Nexus')).toBeInTheDocument();
  expect(within(dialog).getByText('Codex Account · This computer')).toBeInTheDocument();
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Computers included in usage' }));
  await fireEvent.click(await view.findByRole('menuitemcheckbox', { name: 'This computer' }));
  await waitFor(() => expect(view.getByTestId('usage-footer-row')).toHaveTextContent('200 · $2.00'));
  await waitFor(() => expect(within(dialog).getAllByTestId('usage-codex-account')).toHaveLength(1));
  expect(view.getByTestId('sidebar-usage-footer')).toHaveTextContent('Nexus');
});
