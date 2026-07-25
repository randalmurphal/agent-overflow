import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ComposerDraftStore } from './composerDraft.svelte';
import type { CommandContext } from './commandRegistry.svelte';
import { clearCommandRegistry, getCommand, listCommands, runCommand } from './commandRegistry.svelte';
import { registerComposerDraft, resetComposerDraftRegistryForTest } from './composerDraftRegistry.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { setViewOnlySessionFromBootstrap } from '../transport/runMode';
import {
  getWorkflowsActionTargetForTest,
  insertWorkflowComposerContext,
  registerWorkflowCommands,
  registerWorkflowsActionTarget,
} from './workflowCommands.svelte';
import {
  getWorkflowsOverlayRunId,
  getWorkflowsOverlayTop,
  isWorkflowsOverlayOpen,
  openWorkflowsOverlay,
  pushWorkflowRunDetail,
  resetWorkflowsOverlayForTest,
  setWorkflowArmedAction,
} from './workflowsOverlay.svelte';
import { resetAppStorageForTest } from './appStorage';

const BLOCK = '## Workflows\n\nport → check';

function draft(initial = ''): ComposerDraftStore & { content: string } {
  let content = initial;
  return {
    get content() { return content; },
    setContent(next: string) { content = next; },
  } as unknown as ComposerDraftStore & { content: string };
}

function ctx(over: Record<string, unknown> = {}): CommandContext {
  const flags = {
    workflowsOverlayOpen: true,
    workflowsRunDetail: true,
    hasActiveThread: true,
    ...(over.flags as Record<string, boolean> | undefined),
  };
  return {
    pane: { paneId: 'pane-1', threadId: 'thread-1' },
    paneId: 'pane-1',
    ...over,
    flags,
  } as unknown as CommandContext;
}

function clearToasts(): void {
  for (const toast of [...getToasts()]) removeToast(toast.id);
}

describe('/workflow composer command', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetComposerDraftRegistryForTest();
    setViewOnlySessionFromBootstrap(false);
    clearToasts();
    setBindingMock('WorkflowComposerContext', async () => BLOCK);
  });

  afterEach(() => {
    setViewOnlySessionFromBootstrap(false);
    resetBindingMocks();
    clearToasts();
  });

  it('injects the block into an empty composer', async () => {
    const store = draft();
    registerComposerDraft('pane-1', store);
    await insertWorkflowComposerContext(ctx());
    expect(store.content).toBe(BLOCK);
  });

  it('appends below an in-progress prompt instead of clobbering it', async () => {
    const store = draft('  half a thought  ');
    registerComposerDraft('pane-1', store);
    await insertWorkflowComposerContext(ctx());
    expect(store.content).toBe(`half a thought\n\n${BLOCK}`);
  });

  it('warns and injects nothing when no thread is open', async () => {
    registerComposerDraft('pane-1', draft());
    await insertWorkflowComposerContext(ctx({ pane: { paneId: 'pane-1', threadId: '' } }));
    expect(getToasts().at(-1)).toMatchObject({
      type: 'warning', message: 'Open a thread before inserting workflow context.',
    });
  });

  it('warns and injects nothing when the pane has no composer', async () => {
    await insertWorkflowComposerContext(ctx());
    expect(getToasts().at(-1)).toMatchObject({ type: 'warning', message: 'This pane has no composer.' });
  });

  it('refuses in a view-only session (§10) without calling the RPC', async () => {
    const store = draft();
    registerComposerDraft('pane-1', store);
    setViewOnlySessionFromBootstrap(true);
    await insertWorkflowComposerContext(ctx());
    expect(store.content).toBe('');
    expect(getToasts().at(-1)).toMatchObject({ type: 'warning', message: 'Local only' });
  });

  it('says so when the project has no workflow context yet', async () => {
    const store = draft();
    registerComposerDraft('pane-1', store);
    setBindingMock('WorkflowComposerContext', async () => '   ');
    await insertWorkflowComposerContext(ctx());
    expect(store.content).toBe('');
    expect(getToasts().at(-1)).toMatchObject({ type: 'info' });
  });

  it('surfaces a backend failure as an error toast, never a silent no-op', async () => {
    const store = draft();
    registerComposerDraft('pane-1', store);
    setBindingMock('WorkflowComposerContext', async () => { throw new Error('no such thread'); });
    await insertWorkflowComposerContext(ctx());
    expect(store.content).toBe('');
    expect(getToasts().at(-1)).toMatchObject({ type: 'error' });
  });
});

describe('overlay commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetAppStorageForTest();
    resetWorkflowsOverlayForTest();
    registerWorkflowCommands();
  });

  it('registers exactly the §8 vocabulary plus the composer command', () => {
    expect(listCommands().map((command) => command.id).sort()).toEqual([
      'workflow.composerContext',
      'workflows.action.enter',
      'workflows.action.primary',
      'workflows.action.reject',
      'workflows.action.thread',
      'workflows.back',
      'workflows.escape',
      'workflows.sweep.next',
      'workflows.sweep.prev',
      'workflows.toggle',
    ]);
  });

  it('scopes every overlay command to the overlay, and none of them to a text field', () => {
    for (const command of listCommands()) {
      if (!command.id.startsWith('workflows.')) continue;
      expect(command.when === 'workflowsOverlayOpen' || command.when === 'workflowsRunDetail' || command.id === 'workflows.toggle').toBe(true);
      // §8: suppressed while a text field has focus — App.svelte only dispatches
      // editable-target chords for editableReachable commands.
      expect(command.editableReachable ?? false).toBe(false);
    }
    expect(getCommand('workflow.composerContext')?.when).toBe('hasActiveThread');
  });

  it('toggles the overlay from the one unscoped command', () => {
    runCommand('workflows.toggle', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(true);
    runCommand('workflows.toggle', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(false);
  });

  it('walks the escape ladder: disarm, then back, then close', () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1');
    setWorkflowArmedAction('cancel:run-1');
    runCommand('workflows.escape', ctx());
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
    runCommand('workflows.escape', ctx());
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    runCommand('workflows.escape', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(false);
  });

  it('backs out of a run and then closes the overlay', () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1');
    runCommand('workflows.back', ctx());
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    runCommand('workflows.back', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(false);
  });

  it('routes the resolution keys through whichever run detail is mounted', () => {
    const action = vi.fn();
    const enter = vi.fn();
    const dispose = registerWorkflowsActionTarget({ action, enter });
    runCommand('workflows.action.primary', ctx());
    runCommand('workflows.action.reject', ctx());
    runCommand('workflows.action.thread', ctx());
    runCommand('workflows.action.enter', ctx());
    expect(action.mock.calls.map(([key]) => key)).toEqual(['a', 'r', 't']);
    expect(enter).toHaveBeenCalledTimes(1);

    dispose();
    expect(getWorkflowsActionTargetForTest()).toBeNull();
    // No target mounted: the keys are inert rather than throwing.
    expect(() => runCommand('workflows.action.primary', ctx())).not.toThrow();
  });

  it('leaves a newer target in place when an older one disposes', () => {
    const first = { action: vi.fn(), enter: vi.fn() };
    const second = { action: vi.fn(), enter: vi.fn() };
    const disposeFirst = registerWorkflowsActionTarget(first);
    registerWorkflowsActionTarget(second);
    disposeFirst();
    expect(getWorkflowsActionTargetForTest()).toBe(second);
  });
});
