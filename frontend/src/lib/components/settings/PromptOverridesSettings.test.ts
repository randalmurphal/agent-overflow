import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import PromptOverridesSettings from './PromptOverridesSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import {
  ensureProviderModels,
  resetProviderModelsForTest,
} from '../../stores/providerModels.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import type { ModelInfo, PromptOverride, Settings } from '../../types/settings';

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
    ...((patch as Record<string, unknown>) ?? {}),
  }));
  setBindingMock('GetModelsForProvider', async (provider: unknown) =>
    provider === 'codex' ? CODEX_CATALOG : CLAUDE_CATALOG,
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

const ENTRY = (over: Partial<PromptOverride> = {}): PromptOverride => ({
  enabled: true,
  models: ['claude-fable-5'],
  prompt: 'You are a helpful agent.',
  ...over,
});

describe('<PromptOverridesSettings>', () => {
  beforeEach(() => {
    resetProviderModelsForTest();
  });

  it('renders a block per enabled provider and skips a disabled one', async () => {
    await seed({ codexEnabled: false });
    const { getByTestId, queryByTestId } = render(PromptOverridesSettings);
    expect(getByTestId('settings-prompts-claude')).toBeTruthy();
    expect(queryByTestId('settings-prompts-codex')).toBeNull();
  });

  it('tells the user to enable a provider when none is on', async () => {
    await seed({ claudeEnabled: false, codexEnabled: false });
    const { getByText, queryByTestId } = render(PromptOverridesSettings);
    expect(queryByTestId('settings-prompts-claude')).toBeNull();
    expect(getByText(/Enable a provider under Settings/)).toBeTruthy();
  });

  // The header is the only place the section states what a save does, so it
  // has to be true of all four combinations. It used to say every change was
  // spawn-only, which stopped being true when a Claude prompt edit started
  // converging live through `set_model.system_prompt`.
  it('states per provider and axis when a change takes effect', async () => {
    await seed();
    const { getByText } = render(PromptOverridesSettings);
    expect(
      getByText(/A Claude prompt edit reaches running Claude sessions right away/),
    ).toBeTruthy();
    expect(getByText(/turning one off applies when the session restarts/)).toBeTruthy();
    expect(
      getByText(
        /Codex prompts, Claude TUI sessions, and both tool lists apply to sessions started later\./,
      ),
    ).toBeTruthy();
  });

  it('renders the configured entries with their prompt, switch, and models', async () => {
    await seed({
      claudePromptOverrides: [ENTRY(), ENTRY({ enabled: false, models: [], prompt: 'draft' })],
    });
    const { getByTestId, findByTestId } = render(PromptOverridesSettings);

    const first = getByTestId('settings-prompt-text-claude-0') as HTMLTextAreaElement;
    expect(first.value).toBe('You are a helpful agent.');
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
    const { getByTestId, queryByTestId } = render(PromptOverridesSettings);
    expect(getByTestId('settings-prompts-claude-empty')).toBeTruthy();
    expect(queryByTestId('settings-prompt-legend-claude')).toBeNull();
  });

  it('appends a disabled empty entry from the add control', async () => {
    await seed();
    const { getByTestId } = render(PromptOverridesSettings);
    await fireEvent.click(getByTestId('settings-prompt-add-claude'));

    await waitFor(() => {
      expect(claudeEntries()).toEqual([{ enabled: false, models: [], prompt: '' }]);
    });
  });

  it('removes the entry that was clicked', async () => {
    await seed({
      claudePromptOverrides: [ENTRY({ prompt: 'first' }), ENTRY({ prompt: 'second' })],
    });
    const { getByTestId } = render(PromptOverridesSettings);
    await fireEvent.click(getByTestId('settings-prompt-remove-claude-0'));

    await waitFor(() => {
      expect(claudeEntries().map((e) => e.prompt)).toEqual(['second']);
    });
  });

  it('persists the enable switch per entry', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ enabled: false })] });
    const { getByLabelText } = render(PromptOverridesSettings);
    await fireEvent.click(getByLabelText('Enable claude override 1'));

    await waitFor(() => expect(claudeEntries()[0].enabled).toBe(true));
  });

  it('round-trips a model chip selection', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: [] })] });
    const { findByTestId } = render(PromptOverridesSettings);

    const chip = await findByTestId('settings-prompt-model-claude-0-claude-opus-4-8');
    await fireEvent.click(chip);
    await waitFor(() => expect(claudeEntries()[0].models).toEqual(['claude-opus-4-8']));

    await fireEvent.click(await findByTestId('settings-prompt-model-claude-0-claude-opus-4-8'));
    await waitFor(() => expect(claudeEntries()[0].models).toEqual([]));
  });

  it('keeps a selected slug the catalog no longer offers so it can be removed', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: ['claude-retired-1'] })] });
    const { findByTestId } = render(PromptOverridesSettings);

    const chip = await findByTestId('settings-prompt-model-claude-0-claude-retired-1');
    expect(chip.dataset.selected).toBe('true');
    await waitFor(() => expect(chip.dataset.missing).toBe('true'));
    await fireEvent.click(chip);
    await waitFor(() => expect(claudeEntries()[0].models).toEqual([]));
  });

  it('does not call a selected slug missing when the catalog came back empty', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: ['claude-retired-1'] })] });
    setBindingMock('GetModelsForProvider', async () => []);
    // Awaited here so the catalog is definitively loaded-and-empty before the
    // chip renders — an unsettled load would pass this assertion for the
    // wrong reason.
    await ensureProviderModels('claude');

    const { findByTestId } = render(PromptOverridesSettings);
    const chip = await findByTestId('settings-prompt-model-claude-0-claude-retired-1');
    expect(chip.dataset.missing).toBe('false');
    expect(chip.className).not.toContain('border-dashed');
  });

  it('does not call a selected slug missing when the catalog failed to load', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: ['claude-retired-1'] })] });
    setBindingMock('GetModelsForProvider', async () => {
      throw new Error('claude CLI not found');
    });

    const { findByTestId } = render(PromptOverridesSettings);
    await findByTestId('settings-prompts-claude-catalog-error');
    const chip = await findByTestId('settings-prompt-model-claude-0-claude-retired-1');
    expect(chip.dataset.missing).toBe('false');
  });

  it('surfaces a failed model catalog as state, not as silence', async () => {
    await seed({ claudePromptOverrides: [ENTRY()] });
    setBindingMock('GetModelsForProvider', async () => {
      throw new Error('claude CLI not found');
    });
    const { findByTestId } = render(PromptOverridesSettings);
    expect(
      (await findByTestId('settings-prompts-claude-catalog-error')).textContent,
    ).toContain('Could not load the Claude model catalog.');
  });

  it('commits the prompt on change, not on every keystroke', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ prompt: 'old' })] });
    const { getByTestId } = render(PromptOverridesSettings);
    const textarea = getByTestId('settings-prompt-text-claude-0') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'new prompt' } });
    expect(getBindingMock('UpdateSettings')?.mock.calls.length ?? 0).toBe(0);

    await fireEvent.change(textarea);
    await waitFor(() => expect(claudeEntries()[0].prompt).toBe('new prompt'));
  });

  it('does not write when a blur leaves the prompt unchanged', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ prompt: 'same' })] });
    const { getByTestId } = render(PromptOverridesSettings);
    await fireEvent.change(getByTestId('settings-prompt-text-claude-0'));
    expect(getBindingMock('UpdateSettings')?.mock.calls.length ?? 0).toBe(0);
  });

  it('keeps a prompt typed just before a remove shifts the indexes', async () => {
    // The ordering the section depends on: a textarea commits on blur, and
    // blur fires before the click of the button that took the focus. The
    // remove therefore operates on a list that already carries the typing,
    // and the typed entry survives its own index changing.
    await seed({
      claudePromptOverrides: [ENTRY({ prompt: 'first' }), ENTRY({ prompt: 'second' })],
    });
    const { getByTestId } = render(PromptOverridesSettings);
    const second = getByTestId('settings-prompt-text-claude-1') as HTMLTextAreaElement;

    await fireEvent.input(second, { target: { value: 'edited second' } });
    await fireEvent.change(second);
    await fireEvent.click(getByTestId('settings-prompt-remove-claude-0'));

    await waitFor(() =>
      expect(claudeEntries()).toEqual([
        { enabled: true, models: ['claude-fable-5'], prompt: 'edited second' },
      ]),
    );
  });

  it('warns when an enabled entry applies to no model', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: [] })] });
    const { getByTestId, queryByTestId } = render(PromptOverridesSettings);
    expect(getByTestId('settings-prompt-nomodels-claude-0')).toBeTruthy();
    expect(queryByTestId('settings-prompt-noprompt-claude-0')).toBeNull();
  });

  it('warns when an enabled entry has no prompt, which the backend skips too', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ prompt: '   ' })] });
    const { getByTestId, queryByTestId } = render(PromptOverridesSettings);
    expect(getByTestId('settings-prompt-noprompt-claude-0').textContent).toContain(
      'write a prompt',
    );
    expect(queryByTestId('settings-prompt-nomodels-claude-0')).toBeNull();
  });

  it('names both causes when an enabled entry has neither', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ models: [], prompt: '' })] });
    const { getByTestId } = render(PromptOverridesSettings);
    expect(getByTestId('settings-prompt-nomodels-claude-0')).toBeTruthy();
    expect(getByTestId('settings-prompt-noprompt-claude-0')).toBeTruthy();
  });

  it('warns about neither when the entry is switched off', async () => {
    await seed({
      claudePromptOverrides: [ENTRY({ enabled: false, models: [], prompt: '' })],
    });
    const { queryByTestId } = render(PromptOverridesSettings);
    expect(queryByTestId('settings-prompt-nomodels-claude-0')).toBeNull();
    expect(queryByTestId('settings-prompt-noprompt-claude-0')).toBeNull();
  });

  it('warns that an earlier enabled entry already covers a model', async () => {
    await seed({
      claudePromptOverrides: [ENTRY(), ENTRY({ prompt: 'second' })],
    });
    const { getByTestId, queryByTestId } = render(PromptOverridesSettings);
    expect(queryByTestId('settings-prompt-shadowed-claude-0')).toBeNull();
    expect(getByTestId('settings-prompt-shadowed-claude-1').textContent).toContain('Fable 5');
  });
});

describe('<PromptOverridesSettings> placeholder legend', () => {
  beforeEach(() => {
    resetProviderModelsForTest();
  });

  it('inserts the token at the caret of the focused prompt', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ prompt: 'cwd:  here' })] });
    const { getByTestId } = render(PromptOverridesSettings);
    const textarea = getByTestId('settings-prompt-text-claude-0') as HTMLTextAreaElement;

    textarea.focus();
    await fireEvent.focus(textarea);
    textarea.setSelectionRange(5, 5);

    await fireEvent.click(getByTestId('settings-prompt-token-claude-WORKDIR'));

    await waitFor(() =>
      expect(claudeEntries()[0].prompt).toBe('cwd: {{WORKDIR}} here'),
    );
    expect(textarea.value).toBe('cwd: {{WORKDIR}} here');
    await waitFor(() => expect(textarea.selectionStart).toBe(16));
  });

  it('falls back to the first prompt when none was focused', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ prompt: 'AB' })] });
    const { getByTestId } = render(PromptOverridesSettings);

    await fireEvent.click(getByTestId('settings-prompt-token-claude-MODEL_NAME'));

    await waitFor(() =>
      expect(claudeEntries()[0].prompt).toBe('AB{{MODEL_NAME}}'),
    );
  });

  it('leaves the prompt alone when the insert fails to save', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ prompt: 'AB' })] });
    const { getByTestId } = render(PromptOverridesSettings);
    const textarea = getByTestId('settings-prompt-text-claude-0') as HTMLTextAreaElement;

    textarea.focus();
    await fireEvent.focus(textarea);
    textarea.setSelectionRange(2, 2);

    setBindingMock('UpdateSettings', async () => {
      throw new Error('rpc fail');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    await fireEvent.click(getByTestId('settings-prompt-token-claude-MODEL_NAME'));

    // Nothing else writes this textarea, so a token left behind by a failed
    // save would be visible text belonging to no state.
    await waitFor(() => expect(textarea.value).toBe('AB'));
    expect(textarea.selectionStart).toBe(2);
    consoleErr.mockRestore();
  });

  it('shows the insertion before the save lands, through the store', async () => {
    await seed({ claudePromptOverrides: [ENTRY({ prompt: 'AB' })] });
    const { getByTestId } = render(PromptOverridesSettings);
    const textarea = getByTestId('settings-prompt-text-claude-0') as HTMLTextAreaElement;

    textarea.focus();
    await fireEvent.focus(textarea);
    textarea.setSelectionRange(2, 2);

    let land: (() => void) | undefined;
    const inFlight = new Promise<void>((resolve) => {
      land = resolve;
    });
    setBindingMock('UpdateSettings', async (patch: unknown) => {
      await inFlight;
      return { ...makeSettings(), ...(patch as Record<string, unknown>) };
    });

    await fireEvent.click(getByTestId('settings-prompt-token-claude-MODEL_NAME'));
    // The optimistic settings write is what makes it visible — the component
    // never writes the textarea itself, so there is nothing to reconcile if
    // the save then fails.
    expect(textarea.value).toBe('AB{{MODEL_NAME}}');

    land!();
    await waitFor(() => expect(textarea.selectionStart).toBe(16));
    expect(textarea.value).toBe('AB{{MODEL_NAME}}');
  });

  it('offers the Claude-only memory placeholder to Claude alone', async () => {
    await seed({
      claudePromptOverrides: [ENTRY()],
      codexPromptOverrides: [ENTRY({ models: ['gpt-5.6-sol'] })],
    });
    const { getByTestId, queryByTestId } = render(PromptOverridesSettings);
    expect(getByTestId('settings-prompt-token-claude-MEMORY_DIR')).toBeTruthy();
    expect(getByTestId('settings-prompt-token-codex-WORKDIR')).toBeTruthy();
    expect(queryByTestId('settings-prompt-token-codex-MEMORY_DIR')).toBeNull();
  });
});
