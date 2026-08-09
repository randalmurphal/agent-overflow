import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { updatePlaceholderDefaults } from './newThreadDefaults';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Project } from '../types/models';

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
