import { beforeAll, beforeEach, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import UnifiedThreadPicker from './UnifiedThreadPicker.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { refreshThreads, updateThreadTitle } from '../../stores/threads.svelte';
import { isHiddenThreadMode } from '../../utils/threadModes';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';
import type { Thread } from '../../types/models';

vi.mock('../../utils/threadModes', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../utils/threadModes')>();
  return { ...actual, isHiddenThreadMode: vi.fn(actual.isHiddenThreadMode) };
});

beforeAll(installAnimateShim);

const thread = (id: string, title: string): Thread => ({
  id, title, provider: 'claude', workspacePath: '/w', projectPath: '/w', mode: 'chat',
  model: 'm', createdAt: 0, updatedAt: 0, archived: false,
});

beforeEach(async () => {
  setBindingMock('ListThreads', async () => [thread('a', 'Alpha'), thread('b', 'Beta'), thread('c', 'Gamma')]);
  await refreshThreads();
});

it('filters no threads while closed, and lists the current ones when it opens', async () => {
  const pane = createThreadPane();
  const view = render(UnifiedThreadPicker, { props: { open: false, pane, onClose: () => {} } });
  await tick();
  vi.mocked(isHiddenThreadMode).mockClear();

  updateThreadTitle('a', 'Renamed');
  await tick();
  expect(isHiddenThreadMode).not.toHaveBeenCalled();

  await view.rerender({ open: true, pane, onClose: () => {} });
  await waitFor(() => expect(view.getByText('Renamed')).toBeTruthy());
  expect(view.getByText('Beta')).toBeTruthy();
  expect(isHiddenThreadMode).toHaveBeenCalled();
});
