import { beforeEach, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import * as selection from './selectedBackend.svelte';
import * as index from '../transport/entityIndex';

// Keep one Svelte runtime: cold-start tests reset the module graph, which would
// separate a statically compiled effect from dynamically reimported signals.
beforeEach(() => {
  selection.__resetSelectedBackendForTest();
  selection.setFocusedThreadResolver(() => null);
  selection.setActiveBackendPaneResolver(() => null);
  index.__resetEntityIndexForTest();
});

it.each(['thread', 'project'] as const)('updates mounted selection when the focused %s owner arrives after hydration', (kind) => {
  const thread = { id: 'restored-thread', projectId: 'restored-project' };
  selection.setFocusedThreadResolver(() => thread);
  const seen: string[] = [];
  const stop = $effect.root(() => {
    const backend = $derived(selection.selectedBackend());
    $effect(() => { seen.push(backend); });
  });
  try {
    flushSync();
    expect(seen).toEqual(['']);
    if (kind === 'thread') index.noteThread(thread.id, 'gpu', 0);
    else index.noteProject(thread.projectId, 'gpu');
    expect(selection.selectedBackend()).toBe('gpu');
    flushSync();
    expect(seen).toEqual(['', 'gpu']);
  } finally {
    stop();
  }
});

it('tracks moves and owner removal in the same pane without invalidating unrelated indexed readers', () => {
  const thread = { id: 'thread', projectId: 'project' };
  index.noteProject(thread.projectId, 'mac');
  index.noteThread(thread.id, 'mac', 0);
  selection.setSelectedBackend('removed-gpu');
  selection.setFocusedThreadResolver(() => thread);
  const seen: string[] = [];
  let reads = 0;
  const stop = $effect.root(() => {
    const backend = $derived.by(() => { reads += 1; return selection.selectedBackend(); });
    $effect(() => { seen.push(backend); });
  });
  try {
    flushSync();
    index.noteThread(thread.id, 'mac', 0);
    index.noteProject(thread.projectId, 'mac');
    index.noteThread('another-thread', 'gpu', 0);
    index.noteProject('another-project', 'gpu');
    flushSync();
    expect(reads).toBe(1);
    index.noteThread(thread.id, 'gpu', 1);
    flushSync();
    expect(seen).toEqual(['mac', 'gpu']);
    index.forgetThread(thread.id);
    flushSync();
    expect(seen).toEqual(['mac', 'gpu', 'mac']);
    index.forgetProject(thread.projectId);
    flushSync();
    // Losing catalog ownership never selects an arbitrary available computer.
    expect(seen).toEqual(['mac', 'gpu', 'mac', 'removed-gpu']);
    selection.initializeSelectedBackend([{ id: 'mac' }]);
    flushSync();
    expect(selection.selectedBackend()).toBe('removed-gpu');
  } finally {
    stop();
  }
});

it('tracks project list hydration and backend-wide removal for an existing draft', () => {
  selection.setFocusedThreadResolver(() => ({ id: 'draft:project', projectId: 'project' }));
  selection.setSelectedBackend('saved-gpu');
  const seen: string[] = [];
  const stop = $effect.root(() => {
    const backend = $derived(selection.selectedBackend());
    $effect(() => { seen.push(backend); });
  });
  try {
    flushSync();
    index.noteRowsFromCall(2721360259, [{ project: { id: 'project' } }], 'mac');
    flushSync();
    expect(seen).toEqual(['saved-gpu', 'mac']);
    index.forgetBackendEntities('mac');
    flushSync();
    expect(seen).toEqual(['saved-gpu', 'mac', 'saved-gpu']);
  } finally {
    stop();
  }
});

it('tracks an existing draft pane’s chosen computer', () => {
  selection.setActiveBackendPaneResolver(() => 'draft-pane');
  const seen: string[] = [];
  const stop = $effect.root(() => {
    const backend = $derived(selection.selectedBackend());
    $effect(() => { seen.push(backend); });
  });
  try {
    flushSync();
    selection.setPaneBackend('draft-pane', 'gpu');
    flushSync();
    selection.setPaneBackend('draft-pane', 'mac');
    flushSync();
    selection.setPaneBackend('draft-pane', null);
    flushSync();
    expect(seen).toEqual(['', 'gpu', 'mac', '']);
  } finally {
    stop();
  }
});
