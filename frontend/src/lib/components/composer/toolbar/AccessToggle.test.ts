import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import AccessToggle from './AccessToggle.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Project, RuntimeMode, Thread } from '../../../types/models';
import {
  getBindingMock,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import { buildPane as buildRegisteredPane } from '../../../../test/helpers/chat';
import { modelCatalog } from '../../../../test/helpers/modelCatalog';
import {
  ensureProviderModels,
} from '../../../stores/providerModels.svelte';
import type { ModelInfo } from '../../../types/settings';
import { withEmptyDraftCleanup } from '../../../stores/emptyDraftCleanup';

function makeThread(runtimeMode: RuntimeMode): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    runtimeMode,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function buildPane(mode: RuntimeMode) {
  return buildRegisteredPane(makeThread(mode));
}

function makeProject(): Project {
  return {
    id: 'project-1',
    path: '/tmp/project',
    name: 'Project',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

describe('<AccessToggle>', () => {
  beforeEach(() => {
    setBindingMock('GetModelsForProvider', async () => modelCatalog([], 'shipped'));
  });

  // A workflow phase declaring `access: read-only` persists this mode on its
  // thread row, so any surface that renders a thread must display it rather
  // than silently falling back to the full-access label — which would show an
  // unattended, restricted session as unrestricted.
  it.each([
    { name: 'renders the current tier label', mode: 'approval-required', label: 'Supervised' },
    { name: 'exposes the current mode as a data attribute', mode: 'auto-accept-edits', label: 'Auto-accept edits' },
    { name: 'renders the read-only tier rather than falling back', mode: 'read-only', label: 'Read-only' },
  ] as const)('$name', async ({ mode, label }) => {
    const pane = await buildPane(mode);
    const { getByTestId } = render(AccessToggle, { props: { pane } });

    const trigger = getByTestId('composer-access-toggle');
    expect(trigger.getAttribute('data-mode')).toBe(mode);
    expect(trigger.textContent).toContain(label);
    expect(trigger.getAttribute('aria-label')).toBe(`Runtime Access Mode: ${label}`);
  });

  it.each([
    { name: 'persists a selected mode on a materialized thread', from: 'approval-required', to: 'auto-accept-edits', item: /Auto-accept edits/ },
    { name: 'persists a read-only selection', from: 'full-access', to: 'read-only', item: /Read-only/ },
    // Exact name, not /Auto/ — that substring also matches "Auto-accept
    // edits", and a picker that silently selected the neighbouring tier is
    // precisely the bug the tier ordering could introduce.
    { name: 'persists the auto tier on a materialized thread', from: 'approval-required', to: 'auto', item: /^Auto A model reviews/ },
  ] as const)('$name', async ({ from, to, item }) => {
    const pane = await buildPane(from);
    const update = setBindingMock('UpdateThreadRuntimeMode', async () => makeThread(to));
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: item }));

    expect(update).toHaveBeenCalledWith('thread-1', to);
    await waitFor(() => {
      expect(getByTestId('composer-access-toggle').getAttribute('data-mode')).toBe(to);
    });
    expect(pane.thread?.runtimeMode).toBe(to);
  });

  it('applies a mode picked during an empty-draft delete to the placeholder it leaves', async () => {
    const pane = await buildRegisteredPane({ ...makeThread('approval-required'), id: 'emptied', isDraft: true, projectId: 'project-1' });
    const update = setBindingMock('UpdateThreadRuntimeMode', async () => makeThread('auto-accept-edits'));
    const defaults = setBindingMock('UpdateNewThreadDefaults', async (input: Record<string, unknown>) => ({ ...input }));
    let answer!: () => void;
    const answered = new Promise<void>((resolve) => { answer = resolve; });
    const cleanup = withEmptyDraftCleanup('emptied', async () => { await answered; return true; }, async () => {
      pane.dematerializeEmptyDraftThread();
    });
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Auto-accept edits/ }));
    expect(update).not.toHaveBeenCalled();
    answer();
    await cleanup;

    await waitFor(() => expect(defaults).toHaveBeenCalledWith(expect.objectContaining({ runtimeMode: 'auto-accept-edits' })));
    expect(update).not.toHaveBeenCalled();
    expect(pane.hasDraftPlaceholder).toBe(true);
  });

  it('updates new-thread defaults on a placeholder', async () => {
    const pane = createThreadPane();
    pane.startDraftPlaceholder(makeProject(), 'chat', {
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      reasoningEffort: '',
      fastMode: false,
      contextWindow: 200000,
      runtimeMode: 'approval-required',
      branch: 'main',
      workspacePath: '/tmp/project',
    });
    const update = setBindingMock('UpdateNewThreadDefaults', async () => ({
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      reasoningEffort: '',
      fastMode: false,
      contextWindow: 200000,
      runtimeMode: 'auto-accept-edits',
      branch: 'main',
      workspacePath: '/tmp/project',
    }));
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Auto-accept edits/ }));

    expect(update).toHaveBeenCalledWith(expect.objectContaining({
      projectId: 'project-1',
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      runtimeMode: 'auto-accept-edits',
    }));
    await waitFor(() => {
      expect(getByTestId('composer-access-toggle').getAttribute('data-mode')).toBe(
        'auto-accept-edits',
      );
    });
    expect(pane.threadId).toBeNull();
  });

  it('does not persist a no-op current mode selection', async () => {
    const pane = await buildPane('approval-required');
    setBindingMock('UpdateThreadRuntimeMode', async () => makeThread('approval-required'));
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Supervised/ }));

    expect(getBindingMock('UpdateThreadRuntimeMode')).not.toHaveBeenCalled();
  });

  it('shows tier descriptions in the access menu', async () => {
    const pane = await buildPane('full-access');
    const { getByText, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));

    expect(getByText('Deny edits and mutating commands instead of asking.')).toBeTruthy();
    expect(getByText('Ask before commands and file changes.')).toBeTruthy();
    expect(getByText('Auto-approve edits, ask before other actions.')).toBeTruthy();
    expect(
      getByText('A model reviews each action instead of you. Costs extra tokens, and it can refuse.'),
    ).toBeTruthy();
    expect(getByText('Allow commands and edits without prompts.')).toBeTruthy();
  });

  // The order is the product decision, not incidental array order: the menu
  // reads most- to least-restrictive on mutation, and `auto` belongs after
  // auto-accept-edits because it lets strictly more through unprompted (
  // auto-accept-edits still stops at every shell command). Go's
  // TestAllRuntimeModesOrdering pins the same sequence on provider
  // .AllRuntimeModes; a divergence between the two is a UI that misrepresents
  // how permissive a tier is relative to its neighbours.
  it('orders the tiers most- to least-restrictive with auto between auto-accept-edits and full access', async () => {
    const pane = await buildPane('full-access');
    const { getAllByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));

    const labels = getAllByRole('menuitem').map((el) =>
      (el.textContent ?? '').replace(/\s+/g, ' ').trim(),
    );
    expect(labels.map((l) => l.split('.')[0].trim())).toEqual([
      'Read-only Deny edits and mutating commands instead of asking',
      'Supervised Ask before commands and file changes',
      'Auto-accept edits Auto-approve edits, ask before other actions',
      'Auto A model reviews each action instead of you',
      'Full access Allow commands and edits without prompts',
    ]);
  });

  // The trigger's tooltip is where a user who never opens the menu meets the
  // two caveats. Auto is the only tier whose label undersells what it does, so
  // the description reaching the tooltip is the surface that carries them.
  it('surfaces both auto caveats on the collapsed trigger', async () => {
    const pane = await buildPane('auto');
    const { getByTestId } = render(AccessToggle, { props: { pane } });

    const trigger = getByTestId('composer-access-toggle');
    expect(trigger.textContent ?? '').toMatch(/Auto/);
    const title = trigger.getAttribute('title') ?? '';
    expect(title).toMatch(/Costs extra tokens/);
    expect(title).toMatch(/it can refuse/);
  });

  // The Auto tier is withheld ONLY on the model's explicit
  // `supportsAutoMode: false` — the CLI's own per-model answer, three-state
  // end to end (internal/claudemodels/AGENTS.md). Anything short of that
  // explicit denial (absent key, unlisted model, catalog not loaded) must
  // leave Auto exactly as selectable as before the field existed.
  describe('supportsAutoMode gate', () => {
    function catalogRow(supportsAutoMode?: boolean | null): ModelInfo {
      return {
        slug: 'claude-sonnet-4-6',
        name: 'Claude Sonnet 4.6',
        provider: 'claude',
        ...(supportsAutoMode === undefined ? {} : { supportsAutoMode }),
      };
    }

    it('disables Auto when the model explicitly refuses it', async () => {
      setBindingMock('GetModelsForProvider', async () => modelCatalog([catalogRow(false)]));
      await ensureProviderModels('claude');
      const pane = await buildPane('approval-required');
      setBindingMock('UpdateThreadRuntimeMode', async () => makeThread('auto'));
      const { getByRole, getByText, getByTestId } = render(AccessToggle, { props: { pane } });

      await fireEvent.click(getByTestId('composer-access-toggle'));

      const autoRow = getByRole('menuitem', { name: /^Auto\b(?!-)/ });
      expect(autoRow.getAttribute('aria-disabled')).toBe('true');
      expect(getByText('Not supported by the current model.')).toBeTruthy();

      await fireEvent.click(autoRow);
      expect(getBindingMock('UpdateThreadRuntimeMode')).not.toHaveBeenCalled();
    });

    it.each<{ name: string; model: ModelInfo }>([
      { name: 'when the wire never answered', model: catalogRow() },
      { name: 'for a model the catalog does not list', model: { slug: 'some-other-model', name: 'Other', provider: 'claude', supportsAutoMode: false } },
    ])('keeps Auto selectable $name', async ({ model }) => {
      setBindingMock('GetModelsForProvider', async () => modelCatalog([model]));
      await ensureProviderModels('claude');
      const pane = await buildPane('approval-required');
      const update = setBindingMock('UpdateThreadRuntimeMode', async () => makeThread('auto'));
      const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

      await fireEvent.click(getByTestId('composer-access-toggle'));

      const autoRow = getByRole('menuitem', { name: /^Auto\b(?!-)/ });
      expect(autoRow.getAttribute('aria-disabled')).toBeNull();

      await fireEvent.click(autoRow);
      expect(update).toHaveBeenCalledWith('thread-1', 'auto');
    });
  });
});
