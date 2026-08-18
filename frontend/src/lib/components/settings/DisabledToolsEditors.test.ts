import { describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ClaudeDisabledToolsEditor from './ClaudeDisabledToolsEditor.svelte';
import CodexDisabledToolsEditor from './CodexDisabledToolsEditor.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { getProviderDefinition } from '../../providers/catalog';
import type { Settings } from '../../types/settings';

const CLAUDE = getProviderDefinition('claude');
const CODEX = getProviderDefinition('codex');

async function seed(overrides: Partial<Settings> = {}): Promise<void> {
  const merged: Settings = { ...makeSettings(), ...overrides };
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => ({
    ...merged,
    ...((patch as Record<string, unknown>) ?? {}),
  }));
  await loadSettings();
}

function lastPatch(): Record<string, unknown> {
  const mock = getBindingMock('UpdateSettings');
  expect(mock).toBeDefined();
  expect(mock!.mock.calls.length).toBeGreaterThan(0);
  return mock!.mock.calls.at(-1)![0] as Record<string, unknown>;
}

describe('<ClaudeDisabledToolsEditor>', () => {
  it('renders the empty state and the free-form field', async () => {
    await seed();
    const { getByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    expect(getByTestId('settings-claude-tools-empty')).toBeTruthy();
    expect(getByTestId('settings-claude-tool-input')).toBeTruthy();
  });

  it('says the names go to the CLI verbatim and that unknown ones are harmless', async () => {
    await seed();
    const { getByText } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    expect(
      getByText(/passed to the Claude CLI verbatim, so a name it doesn't recognise is harmless/),
    ).toBeTruthy();
  });

  it('adds a trimmed free-form name and clears the draft', async () => {
    await seed();
    const { getByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    const input = getByTestId('settings-claude-tool-input') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: '  MyCustomTool ' } });
    await fireEvent.click(getByTestId('settings-claude-tool-add'));

    await waitFor(() =>
      expect(lastPatch()).toEqual({ claudeDisabledTools: ['MyCustomTool'] }),
    );
    await waitFor(() => expect(input.value).toBe(''));
  });

  it('adds on Enter and refuses a duplicate', async () => {
    await seed({ claudeDisabledTools: ['Workflow'] });
    const { getByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    const input = getByTestId('settings-claude-tool-input') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: 'Workflow' } });
    await waitFor(() =>
      expect(getByTestId('settings-claude-tool-error').textContent).toContain('already disabled'),
    );
    expect((getByTestId('settings-claude-tool-add') as HTMLButtonElement).disabled).toBe(true);

    await fireEvent.input(input, { target: { value: 'Agent' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() =>
      expect(lastPatch()).toEqual({ claudeDisabledTools: ['Workflow', 'Agent'] }),
    );
  });

  it('adds a suggested built-in and drops it from the suggestion row', async () => {
    await seed();
    const { getByTestId, queryByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });

    await fireEvent.click(getByTestId('settings-claude-tool-suggest-Workflow'));
    await waitFor(() => expect(lastPatch()).toEqual({ claudeDisabledTools: ['Workflow'] }));
    await waitFor(() => expect(queryByTestId('settings-claude-tool-suggest-Workflow')).toBeNull());
    expect(getByTestId('settings-claude-tool-Workflow')).toBeTruthy();
  });

  it('refuses a name with whitespace instead of letting the save fail', async () => {
    await seed();
    const { getByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    const input = getByTestId('settings-claude-tool-input') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: 'Web Search' } });
    await waitFor(() =>
      expect(getByTestId('settings-claude-tool-error').textContent).toContain('whitespace'),
    );
    expect((getByTestId('settings-claude-tool-add') as HTMLButtonElement).disabled).toBe(true);

    // Enter must not smuggle it past the disabled button either.
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(getBindingMock('UpdateSettings')?.mock.calls.length ?? 0).toBe(0);
  });

  it('refuses a leading dash the CLI would parse as a flag', async () => {
    await seed();
    const { getByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    const input = getByTestId('settings-claude-tool-input') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: '-Workflow' } });
    await waitFor(() =>
      expect(getByTestId('settings-claude-tool-error').textContent).toContain('start with'),
    );
    await fireEvent.click(getByTestId('settings-claude-tool-add'));
    expect(getBindingMock('UpdateSettings')?.mock.calls.length ?? 0).toBe(0);
  });

  it('refuses a name past the byte cap', async () => {
    await seed();
    const { getByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    const input = getByTestId('settings-claude-tool-input') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: 'a'.repeat(129) } });
    await waitFor(() =>
      expect(getByTestId('settings-claude-tool-error').textContent).toContain('limited to'),
    );
    expect((getByTestId('settings-claude-tool-add') as HTMLButtonElement).disabled).toBe(true);
  });

  it('clears the refusal once the name is valid', async () => {
    await seed();
    const { getByTestId, queryByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    const input = getByTestId('settings-claude-tool-input') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: 'Web Search' } });
    await waitFor(() => expect(queryByTestId('settings-claude-tool-error')).not.toBeNull());

    await fireEvent.input(input, { target: { value: 'WebSearch' } });
    await waitFor(() => expect(queryByTestId('settings-claude-tool-error')).toBeNull());
    await fireEvent.click(getByTestId('settings-claude-tool-add'));
    await waitFor(() => expect(lastPatch()).toEqual({ claudeDisabledTools: ['WebSearch'] }));
  });

  it('removes a configured name', async () => {
    await seed({ claudeDisabledTools: ['Workflow', 'WebSearch'] });
    const { getByTestId } = render(ClaudeDisabledToolsEditor, {
      props: { provider: CLAUDE },
    });
    await fireEvent.click(getByTestId('settings-claude-tool-remove-Workflow'));
    await waitFor(() => expect(lastPatch()).toEqual({ claudeDisabledTools: ['WebSearch'] }));
  });
});

describe('<CodexDisabledToolsEditor>', () => {
  it('renders the seven curated toggles, on by default', async () => {
    await seed();
    const { getByTestId } = render(CodexDisabledToolsEditor, {
      props: { provider: CODEX },
    });
    for (const id of [
      'web_search',
      'update_plan',
      'view_image',
      'request_user_input',
      'collab_agents',
      'image_generation',
      'tool_suggest',
    ]) {
      expect(getByTestId(`settings-codex-tool-${id}`).dataset.available).toBe('true');
    }
    expect(getByTestId('settings-codex-tool-collab_agents').textContent).toContain(
      'Collab / multi-agent tools',
    );
  });

  it('offers no free-form field', async () => {
    await seed();
    const { container } = render(CodexDisabledToolsEditor, { props: { provider: CODEX } });
    expect(container.querySelector('input[type="text"]')).toBeNull();
  });

  it('writes the toggle id when a tool is turned off, and drops it when turned back on', async () => {
    await seed();
    const { getByLabelText } = render(CodexDisabledToolsEditor, {
      props: { provider: CODEX },
    });

    await fireEvent.click(getByLabelText('Web search available to the model'));
    await waitFor(() => expect(lastPatch()).toEqual({ codexDisabledTools: ['web_search'] }));

    await fireEvent.click(getByLabelText('Web search available to the model'));
    await waitFor(() => expect(lastPatch()).toEqual({ codexDisabledTools: [] }));
  });

  it('renders a stored id as off without disturbing the others', async () => {
    await seed({ codexDisabledTools: ['collab_agents'] });
    const { getByTestId, getByLabelText } = render(CodexDisabledToolsEditor, {
      props: { provider: CODEX },
    });
    expect(getByTestId('settings-codex-tool-collab_agents').dataset.available).toBe('false');
    expect(getByTestId('settings-codex-tool-web_search').dataset.available).toBe('true');

    await fireEvent.click(getByLabelText('Plan updates available to the model'));
    await waitFor(() =>
      expect(lastPatch()).toEqual({ codexDisabledTools: ['collab_agents', 'update_plan'] }),
    );
  });

  it('shows a stored id it does not recognise, and lets it be removed', async () => {
    await seed({ codexDisabledTools: ['web_search', 'zzz_from_a_newer_build'] });
    const { getByTestId, queryByTestId } = render(CodexDisabledToolsEditor, {
      props: { provider: CODEX },
    });

    const row = getByTestId('settings-codex-tool-unknown-zzz_from_a_newer_build');
    expect(row.textContent).toContain('zzz_from_a_newer_build');
    expect(row.textContent).toContain('has no effect in this version');
    // A curated id keeps its switch and never doubles as an unknown row.
    expect(getByTestId('settings-codex-tool-web_search').dataset.available).toBe('false');
    expect(queryByTestId('settings-codex-tool-unknown-web_search')).toBeNull();

    await fireEvent.click(getByTestId('settings-codex-tool-forget-zzz_from_a_newer_build'));
    await waitFor(() => expect(lastPatch()).toEqual({ codexDisabledTools: ['web_search'] }));
  });

  it('renders no unknown rows when every stored id is curated', async () => {
    await seed({ codexDisabledTools: ['web_search'] });
    const { container } = render(CodexDisabledToolsEditor, { props: { provider: CODEX } });
    expect(container.querySelectorAll('[data-testid^="settings-codex-tool-unknown-"]')).toHaveLength(
      0,
    );
  });
});
