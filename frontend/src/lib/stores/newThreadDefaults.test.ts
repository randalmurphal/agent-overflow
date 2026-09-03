import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { applyNewThreadDefaults, updatePlaceholderDefaults } from './newThreadDefaults';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Project } from '../types/models';
import type { ThreadDefaults } from './bindings';

function project(id: string): Project {
  return { id, name: id, path: `/${id}`, createdAt: 0, updatedAt: 0 } as Project;
}

function placeholderPane(paneId: string, projectId: string, register = true): ThreadPane {
  const pane = createThreadPane({ paneId });
  pane.startDraftPlaceholder(project(projectId), 'chat', {
    provider: 'claude',
    model: 'sonnet',
  });
  if (register) registerPaneForTest(paneId, pane);
  return pane;
}

beforeEach(() => {
  resetPanesForTest();
  setBindingMock('UpdateNewThreadDefaults', async (input: { model?: string }) => ({
    provider: 'claude',
    model: input.model ?? 'sonnet',
    reasoningEffort: '',
    fastMode: false,
    contextWindow: 0,
    runtimeMode: 'full-access',
  }));
});

describe('updatePlaceholderDefaults', () => {
  it('fans the new defaults out to every draft placeholder on the project', async () => {
    const acting = placeholderPane('main', 'proj-1');
    const sibling = placeholderPane('pane-1', 'proj-1');

    await updatePlaceholderDefaults(acting, { model: 'opus' });

    expect(acting.thread?.model).toBe('opus');
    expect(sibling.thread?.model).toBe('opus');
  });

  it('leaves placeholders on other projects alone', async () => {
    const acting = placeholderPane('main', 'proj-1');
    const elsewhere = placeholderPane('pane-1', 'proj-2');

    await updatePlaceholderDefaults(acting, { model: 'opus' });

    expect(acting.thread?.model).toBe('opus');
    expect(elsewhere.thread?.model).toBe('sonnet');
  });

  it('still applies to the acting pane when it is not in the registry', async () => {
    const acting = placeholderPane('main', 'proj-1', false);

    await updatePlaceholderDefaults(acting, { model: 'opus' });

    expect(acting.thread?.model).toBe('opus');
  });

  it('writes nothing when the pane has no project', async () => {
    const pane = createThreadPane({ paneId: 'main' });
    expect(await updatePlaceholderDefaults(pane, { model: 'opus' })).toBeNull();
  });
});

// `chatbar:new-thread-defaults` is the same fan-out, driven by another
// client's write. Before it, UpdateNewThreadDefaults answered its caller and
// told nobody: a second device's placeholder toolbar kept the superseded
// model and would have created a thread with it.
describe('applyNewThreadDefaults', () => {
  const defaults: ThreadDefaults = {
    provider: 'claude',
    model: 'opus',
    reasoningEffort: '',
    fastMode: false,
    contextWindow: 0,
    runtimeMode: 'full-access',
    branch: '',
    workspacePath: '',
  };

  it('converges every draft placeholder on the project it names', () => {
    const a = placeholderPane('main', 'proj-1');
    const b = placeholderPane('pane-1', 'proj-1');

    applyNewThreadDefaults({ projectId: 'proj-1', defaults });

    expect(a.thread?.model).toBe('opus');
    expect(b.thread?.model).toBe('opus');
  });

  // The persisted row is app-wide, but the SET a write converges is the
  // project's: choosing a model in one project's composer is not a statement
  // about another's open placeholder.
  it('leaves another project’s placeholder alone', () => {
    const elsewhere = placeholderPane('pane-1', 'proj-2');

    applyNewThreadDefaults({ projectId: 'proj-1', defaults });

    expect(elsewhere.thread?.model).toBe('sonnet');
  });

  it('ignores a frame with no project or no defaults', () => {
    const pane = placeholderPane('main', 'proj-1');

    applyNewThreadDefaults({ projectId: '', defaults });
    applyNewThreadDefaults({
      projectId: 'proj-1',
      defaults: null as unknown as ThreadDefaults,
    });

    expect(pane.thread?.model).toBe('sonnet');
  });
});
