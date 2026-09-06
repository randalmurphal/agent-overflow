import { beforeEach, expect, it, vi } from 'vitest';

const launch = vi.hoisted(() => ({ computer: '' }));
vi.mock('../transport/runMode', () => ({ initialComputer: () => launch.computer }));

beforeEach(() => {
  vi.resetModules();
  localStorage.clear();
  launch.computer = '';
});

it('remembers this frontend’s explicit computer across launches and catalog ordering', async () => {
  const first = await import('./selectedBackend.svelte');
  first.initializeSelectedBackend([{ id: 'mac' }, { id: 'gpu' }]);
  expect(first.selectedBackend()).toBe('mac');
  first.setSelectedBackend('gpu');
  vi.resetModules();
  const reopened = await import('./selectedBackend.svelte');
  reopened.initializeSelectedBackend([{ id: 'mac' }, { id: 'gpu' }]);
  expect(reopened.selectedBackend()).toBe('gpu');
  // Catalog removal is not permission to send a command to a different host.
  reopened.initializeSelectedBackend([{ id: 'mac' }]);
  expect(reopened.selectedBackend()).toBe('gpu');
});

it('honors a named launch computer over the remembered default even while absent', async () => {
  const first = await import('./selectedBackend.svelte');
  first.setSelectedBackend('mac');
  launch.computer = 'gpu';
  vi.resetModules();
  const reopened = await import('./selectedBackend.svelte');
  reopened.initializeSelectedBackend([{ id: 'mac' }]);
  expect(reopened.selectedBackend()).toBe('gpu');
});

it('keeps a local execution home and can open a frontend with no computers', async () => {
  const selection = await import('./selectedBackend.svelte');
  selection.initializeSelectedBackend([]);
  expect(selection.selectedBackend()).toBe('');
  selection.initializeSelectedBackend([{ id: 'gpu' }, { id: '' }]);
  expect(selection.selectedBackend()).toBe('');
});
