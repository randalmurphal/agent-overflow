import { describe, expect, it, afterEach, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ProviderPromptSection from './ProviderPromptSection.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import {
  ensureProviderModels,
  resetProviderModelsForTest,
} from '../../stores/providerModels.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { modelCatalog } from '../../../test/helpers/modelCatalog';
import { getProviderDefinition } from '../../providers/catalog';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import type { ModelInfo, PromptOverride, Settings } from '../../types/settings';

const CLAUDE = getProviderDefinition('claude');
const CODEX = getProviderDefinition('codex');

const CLAUDE_CATALOG: ModelInfo[] = [
  { slug: 'claude-fable-5', name: 'Claude Fable 5', provider: 'claude' },
  { slug: 'claude-opus-4-8', name: 'Claude Opus 4.8', provider: 'claude' },
];

const CODEX_CATALOG: ModelInfo[] = [
  { slug: 'gpt-5.6-sol', name: 'GPT-5.6-Sol', provider: 'codex' },
];

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged: Settings = { ...makeSettings(), ...overrides };
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => ({
    ...merged,
    ...(patch as Record<string, unknown>),
  }));
  setBindingMock('GetModelsForProvider', async (provider: unknown) =>
    modelCatalog(provider === 'codex' ? CODEX_CATALOG : CLAUDE_CATALOG),
  );
  await loadSettings();
  return merged;
}

/** The most recent settings patch, which is what the UI persisted. */
function lastPatch(): Record<string, unknown> {
  const mock = getBindingMock('UpdateSettings');
  expect(mock).toBeDefined();
  const calls = mock!.mock.calls;
  expect(calls.length).toBeGreaterThan(0);
  return calls.at(-1)![0] as Record<string, unknown>;
}

function claudeEntries(): PromptOverride[] {
  return lastPatch().claudePromptOverrides as PromptOverride[];
}

const ID = '0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c';
const PATH = `/home/u/.config/agent-overflow/prompts/${ID}.md`;

// A saved entry whose file has text: the common case, so each test below
// changes one thing about it.
const ENTRY = (over: Partial<PromptOverride> = {}): PromptOverride => ({
  id: ID,
  enabled: true,
  models: ['claude-fable-5'],
  path: PATH,
  ...over,
});

describe('<ProviderPromptSection>', () => {
  beforeEach(() => {
    resetProviderModelsForTest();
  });

  it('renders the configured entries with their file, switch, and models', async () => {
    await seed({
      claudePromptOverrides: [ENTRY(), ENTRY({ id: 'b', path: '/p/b.md', enabled: false, models: [] })],
    });
    const { getByTestId, findByTestId, queryByRole } = render(ProviderPromptSection, { props: { provider: CLAUDE } });

    expect(queryByRole('textbox')).toBeNull();
    const path = getByTestId('settings-prompt-path-claude-0');
    expect(path.textContent).toBe(PATH);
    expect(path.getAttribute('title')).toBe(PATH);
    expect(getByTestId('settings-prompt-entry-claude-0').dataset.enabled).toBe('true');
    expect(getByTestId('settings-prompt-entry-claude-1').dataset.enabled).toBe('false');

    const selected = await findByTestId('settings-prompt-model-claude-0-claude-fable-5');
    expect(selected.dataset.selected).toBe('true');
    expect(selected.textContent?.trim()).toBe('Fable 5');
    expect(
      (await findByTestId('settings-prompt-model-claude-0-claude-opus-4-8')).dataset.selected,
    ).toBe('false');
  });

  it('renders no legend and an empty note until an entry exists', async () => {
    await seed();
    const { getByTestId, queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(getByTestId('settings-prompts-claude-empty')).toBeTruthy();
    expect(queryByTestId('settings-prompt-legend-claude')).toBeNull();
  });

  it('appends a disabled empty entry from the add control', async () => {
    await seed();
    const { getByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    await fireEvent.click(getByTestId('settings-prompt-add-claude'));

    await waitFor(() => {
      expect(claudeEntries()).toEqual([{ id: '', enabled: false, models: [] }]);
    });
  });

  it('removes the entry that was clicked', async () => {
    await seed({
      claudePromptOverrides: [ENTRY({ id: 'first' }), ENTRY({ id: 'second' })],
    });
    const { getByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    await fireEvent.click(getByTestId('settings-prompt-remove-claude-0'));

    await waitFor(() => {
      expect(claudeEntries().map((e) => e.id)).toEqual(['second']);
    });
  });

  it('persists the enable switch per entry', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ enabled: false })] });
    const { getByLabelText } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    await fireEvent.click(getByLabelText('Enable claude override 1'));

    await waitFor(() => expect(claudeEntries()[0].enabled).toBe(true));
  });

  it('round-trips a model chip selection', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: [] })] });
    const { findByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });

    const chip = await findByTestId('settings-prompt-model-claude-0-claude-opus-4-8');
    await fireEvent.click(chip);
    await waitFor(() => expect(claudeEntries()[0].models).toEqual(['claude-opus-4-8']));

    await fireEvent.click(await findByTestId('settings-prompt-model-claude-0-claude-opus-4-8'));
    await waitFor(() => expect(claudeEntries()[0].models).toEqual([]));
  });

  it('keeps a selected slug the catalog no longer offers so it can be removed', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: ['claude-retired-1'] })] });
    const { findByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });

    const chip = await findByTestId('settings-prompt-model-claude-0-claude-retired-1');
    expect(chip.dataset.selected).toBe('true');
    await waitFor(() => expect(chip.dataset.missing).toBe('true'));
    await fireEvent.click(chip);
    await waitFor(() => expect(claudeEntries()[0].models).toEqual([]));
  });

  it('does not call a selected slug missing when the catalog came back empty', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: ['claude-retired-1'] })] });
    setBindingMock('GetModelsForProvider', async () => modelCatalog([], 'shipped'));
    // Awaited here so the catalog is definitively loaded-and-empty before the
    // chip renders — an unsettled load would pass this assertion for the
    // wrong reason.
    await ensureProviderModels('claude');

    const { findByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    const chip = await findByTestId('settings-prompt-model-claude-0-claude-retired-1');
    expect(chip.dataset.missing).toBe('false');
    expect(chip.className).not.toContain('border-dashed');
  });

  it('does not call a selected slug missing when the catalog failed to load', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: ['claude-retired-1'] })] });
    setBindingMock('GetModelsForProvider', async () => {
      throw new Error('claude CLI not found');
    });

    const { findByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    await findByTestId('settings-prompts-claude-catalog-error');
    const chip = await findByTestId('settings-prompt-model-claude-0-claude-retired-1');
    expect(chip.dataset.missing).toBe('false');
  });

  it('surfaces a failed model catalog as state, not as silence', async () => {
    await seed({ claudePromptOverrides: [ENTRY()] });
    setBindingMock('GetModelsForProvider', async () => {
      throw new Error('claude CLI not found');
    });
    const { findByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(
      (await findByTestId('settings-prompts-claude-catalog-error')).textContent,
    ).toContain('Could not load the Claude model catalog.');
  });

  it('warns when an enabled entry applies to no model', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: [] })] });
    const { getByTestId, queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(getByTestId('settings-prompt-nomodels-claude-0')).toBeTruthy();
    expect(queryByTestId('settings-prompt-blank-claude-0')).toBeNull();
  });

  it('warns when an enabled entry has a blank file, which the backend skips too', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ blank: true })] });
    const { getByTestId, queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(getByTestId('settings-prompt-blank-claude-0').textContent).toContain('file is empty');
    expect(queryByTestId('settings-prompt-nomodels-claude-0')).toBeNull();
  });

  it('names both causes when an enabled entry has neither', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: [], blank: true })] });
    const { getByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(getByTestId('settings-prompt-nomodels-claude-0')).toBeTruthy();
    expect(getByTestId('settings-prompt-blank-claude-0')).toBeTruthy();
  });

  it('warns about neither when the entry is switched off', async () => {
    await seed({
      claudePromptOverrides: [ENTRY({ enabled: false, models: [], blank: true })],
    });
    const { queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(queryByTestId('settings-prompt-nomodels-claude-0')).toBeNull();
    expect(queryByTestId('settings-prompt-blank-claude-0')).toBeNull();
  });

  it('warns that an earlier enabled entry already covers a model', async () => {
    await seed({
      claudePromptOverrides: [ENTRY(), ENTRY({ id: 'second', path: '/p/second.md' })],
    });
    const { getByTestId, queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(queryByTestId('settings-prompt-shadowed-claude-0')).toBeNull();
    expect(getByTestId('settings-prompt-shadowed-claude-1').textContent).toContain('Fable 5');
  });
});

describe('<ProviderPromptSection> placeholder legend', () => {
  beforeEach(() => {
    resetProviderModelsForTest();
  });

  it('lists every placeholder as a read-only reference', async () => {
    await seed({ claudePromptOverrides: [ENTRY()] });
    const { getByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    const legend = getByTestId('settings-prompt-legend-claude');
    expect(legend.querySelectorAll('button')).toHaveLength(0);
    expect(getByTestId('settings-prompt-token-claude-WORKDIR').textContent?.trim()).toBe('{{WORKDIR}}');
    expect(legend.textContent).toContain("The session's working directory.");
  });

  it('offers the Claude-only memory placeholder to Claude alone', async () => {
    await seed({
      claudePromptOverrides: [ENTRY()],
      codexPromptOverrides: [ENTRY({ models: ['gpt-5.6-sol'] })],
    });
    const claude = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(claude.getByTestId('settings-prompt-token-claude-MEMORY_DIR')).toBeTruthy();

    const codex = render(ProviderPromptSection, { props: { provider: CODEX } });
    expect(codex.getByTestId('settings-prompt-token-codex-WORKDIR')).toBeTruthy();
    expect(codex.queryByTestId('settings-prompt-token-codex-MEMORY_DIR')).toBeNull();
  });
});

describe('<ProviderPromptSection> prompt file', () => {
  beforeEach(() => {
    resetProviderModelsForTest();
    setPageGrantsFromBootstrap(false);
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
  });

  it('opens the file in the editor', async () => {
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    await seed({ claudePromptOverrides: [ENTRY()] });
    const { getByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });

    const edit = getByTestId('settings-prompt-open-claude-0');
    expect(edit.getAttribute('aria-label')).toBe(`Open ${ID}.md in editor`);
    await fireEvent.click(edit);
    await waitFor(() => expect(open).toHaveBeenCalledWith(PATH, 0, 0, '', ''));
  });

  it('shows a saving note for an entry the backend has not named yet', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ id: '', path: undefined })] });
    const { getByTestId, queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(getByTestId('settings-prompt-unsaved-claude-0')).toBeTruthy();
    expect(queryByTestId('settings-prompt-file-claude-0')).toBeNull();
    expect(queryByTestId('settings-prompt-copy-instructions-claude')).toBeNull();
  });

  it('keeps the path but offers no open where the page is not on the computer', async () => {
    setPageGrantsFromBootstrap(true);
    await seed({ claudePromptOverrides: [ENTRY()] });
    const { getByTestId, queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(getByTestId('settings-prompt-path-claude-0').textContent).toBe(PATH);
    expect(queryByTestId('settings-prompt-open-claude-0')).toBeNull();
    expect(getByTestId('settings-prompt-copy-instructions-claude')).toBeTruthy();
  });

  it('shows the problem in place of the blank-file warning', async () => {
    const problem = `${PATH} is 70000 bytes, over the 64000-byte limit for one prompt; the entry is not applied until the file is shortened`;
    await seed({ claudePromptOverrides: [ENTRY({ blank: true, problem })] });
    const { getByTestId, queryByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
    expect(getByTestId('settings-prompt-problem-claude-0').textContent?.trim()).toBe(problem);
    expect(queryByTestId('settings-prompt-blank-claude-0')).toBeNull();
  });

  it('copies the instructions an agent needs to edit the files', async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>(async () => undefined);
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    try {
      await seed({ claudePromptOverrides: [ENTRY(), ENTRY({ id: 'b', path: '/p/b.md' })] });
      const { getByTestId } = render(ProviderPromptSection, { props: { provider: CLAUDE } });
      await fireEvent.click(getByTestId('settings-prompt-copy-instructions-claude'));
      await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
      const text = writeText.mock.calls[0]?.[0] ?? '';
      expect(text).toContain('/home/u/.config/agent-overflow/prompts/README.md');
      expect(text).toContain(`- ${PATH}`);
      expect(text).toContain('- /p/b.md');
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
