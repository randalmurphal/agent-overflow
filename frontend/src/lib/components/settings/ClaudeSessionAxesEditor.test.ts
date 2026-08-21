import { describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ClaudeSessionAxesEditor from './ClaudeSessionAxesEditor.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import type { Settings } from '../../types/settings';

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

function patchCount(): number {
  return getBindingMock('UpdateSettings')?.mock.calls.length ?? 0;
}

describe('<ClaudeSessionAxesEditor>', () => {
  it('defaults every axis to the Claude Code default and says so', async () => {
    await seed();
    const { getByTestId } = render(ClaudeSessionAxesEditor);

    expect((getByTestId('settings-claude-output-style') as HTMLSelectElement).value).toBe('');
    expect((getByTestId('settings-claude-subagent-depth') as HTMLInputElement).value).toBe('0');
    expect((getByTestId('settings-claude-tool-memory-limit') as HTMLInputElement).value).toBe('');
  });

  it('writes the selected output style and follows it with that style\'s description', async () => {
    await seed();
    const { getByTestId } = render(ClaudeSessionAxesEditor);

    await fireEvent.change(getByTestId('settings-claude-output-style'), {
      target: { value: 'Explanatory' },
    });

    await waitFor(() => expect(lastPatch()).toEqual({ claudeOutputStyle: 'Explanatory' }));
    await waitFor(() =>
      expect(getByTestId('settings-claude-output-style-hint').textContent).toContain(
        'Explains its implementation choices',
      ),
    );
  });

  it('writes both subagent axes together so editing one cannot drop the other', async () => {
    await seed({ claudeSubagentLimits: { maxSpawnDepth: 3, maxConcurrent: 4 } });
    const { getByTestId } = render(ClaudeSessionAxesEditor);

    await fireEvent.change(getByTestId('settings-claude-subagent-concurrent'), {
      target: { value: '7' },
    });

    await waitFor(() =>
      expect(lastPatch()).toEqual({
        claudeSubagentLimits: { maxSpawnDepth: 3, maxConcurrent: 7 },
      }),
    );
  });

  it('clamps an over-range limit and repaints the field with what was stored', async () => {
    await seed();
    const { getByTestId } = render(ClaudeSessionAxesEditor);
    const input = getByTestId('settings-claude-subagent-depth') as HTMLInputElement;

    await fireEvent.change(input, { target: { value: '9999' } });

    await waitFor(() =>
      expect(lastPatch()).toEqual({
        claudeSubagentLimits: { maxSpawnDepth: 512, maxConcurrent: 0 },
      }),
    );
    expect(input.value).toBe('512');
  });

  it('accepts a size for the tool memory limit', async () => {
    await seed();
    const { getByTestId } = render(ClaudeSessionAxesEditor);
    const input = getByTestId('settings-claude-tool-memory-limit') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: ' 4G ' } });
    await fireEvent.change(input);

    await waitFor(() => expect(lastPatch()).toEqual({ claudeToolMemoryLimit: '4G' }));
  });

  it('accepts "none" as the lift-the-limit word', async () => {
    await seed();
    const { getByTestId } = render(ClaudeSessionAxesEditor);
    const input = getByTestId('settings-claude-tool-memory-limit') as HTMLInputElement;

    await fireEvent.input(input, { target: { value: 'none' } });
    await fireEvent.change(input);

    await waitFor(() => expect(lastPatch()).toEqual({ claudeToolMemoryLimit: 'none' }));
  });

  it('explains a malformed memory limit and never sends it', async () => {
    await seed();
    const { getByTestId, queryByTestId } = render(ClaudeSessionAxesEditor);
    const input = getByTestId('settings-claude-tool-memory-limit') as HTMLInputElement;
    const before = patchCount();

    await fireEvent.input(input, { target: { value: '4 gigs' } });

    await waitFor(() =>
      expect(getByTestId('settings-claude-tool-memory-limit-error').textContent).toContain(
        'Use a size like 4G',
      ),
    );

    // The backend refuses the whole patch over one bad value, so sending it
    // would cost the user every other field in flight and tell them nothing.
    await fireEvent.change(input);
    expect(patchCount()).toBe(before);
    await waitFor(() =>
      expect(queryByTestId('settings-claude-tool-memory-limit-error')).toBeNull(),
    );
  });

  // Thinking is the one axis here that reaches a RUNNING session, and the
  // budget field only exists in the mode that uses it.
  it('reveals the budget field only in budget mode and writes the whole key', async () => {
    await seed();
    const { getByTestId, queryByTestId } = render(ClaudeSessionAxesEditor);

    expect((getByTestId('settings-claude-thinking-mode') as HTMLSelectElement).value).toBe('');
    expect(queryByTestId('settings-claude-thinking-budget')).toBeNull();

    await fireEvent.change(getByTestId('settings-claude-thinking-mode'), {
      target: { value: 'budget' },
    });

    // One settings key, written whole: a patch carrying only the mode would
    // drop the display the user already chose.
    await waitFor(() =>
      expect(lastPatch()).toEqual({
        claudeThinking: { mode: 'budget', display: '', budgetTokens: 8000 },
      }),
    );
    await waitFor(() => expect(getByTestId('settings-claude-thinking-budget')).toBeTruthy());
  });

  // Zero means DISABLED to the CLI, so an unfinished entry must land on the
  // accepted floor rather than silently turning thinking off.
  it('clamps a below-floor budget instead of letting it read as off', async () => {
    await seed({ claudeThinking: { mode: 'budget', budgetTokens: 2048 } });
    const { getByTestId } = render(ClaudeSessionAxesEditor);

    const budget = getByTestId('settings-claude-thinking-budget') as HTMLInputElement;
    await fireEvent.change(budget, { target: { value: '5' } });

    await waitFor(() =>
      expect(lastPatch()).toEqual({
        claudeThinking: { mode: 'budget', display: '', budgetTokens: 1024 },
      }),
    );
    expect(budget.value).toBe('1024');
  });

  it('writes the display axis on its own and follows it with its description', async () => {
    await seed();
    const { getByTestId } = render(ClaudeSessionAxesEditor);

    await fireEvent.change(getByTestId('settings-claude-thinking-display'), {
      target: { value: 'omitted' },
    });

    await waitFor(() =>
      expect(lastPatch()).toEqual({ claudeThinking: { mode: '', display: 'omitted' } }),
    );
    await waitFor(() =>
      expect(getByTestId('settings-claude-thinking-display-hint').textContent).toContain(
        'never reaches the thread',
      ),
    );
  });

  // The return to Claude's default is the one thinking change with no wire
  // form, so it is also the one the user has to be told will wait.
  it('warns about the deferred restart only when returning to the default', async () => {
    await seed({ claudeThinking: { mode: 'budget', budgetTokens: 2048 } });
    const { getByTestId, queryByTestId } = render(ClaudeSessionAxesEditor);

    expect(queryByTestId('settings-claude-thinking-deferred')).toBeNull();
    expect(getByTestId('settings-claude-thinking-mode-hint').textContent).toContain(
      'Applies to running sessions too',
    );

    await fireEvent.change(getByTestId('settings-claude-thinking-mode'), {
      target: { value: 'off' },
    });
    await waitFor(() => expect(lastPatch()).toEqual({ claudeThinking: { mode: 'off', display: '' } }));
    expect(queryByTestId('settings-claude-thinking-deferred')).toBeNull();

    await fireEvent.change(getByTestId('settings-claude-thinking-mode'), {
      target: { value: '' },
    });
    await waitFor(() => expect(getByTestId('settings-claude-thinking-deferred')).toBeTruthy());
  });

  // The notice is a claim about the last SAVE, so it has to stop being made
  // when the save did not happen. updateSettingsPatch never rejects: it
  // restores the keys it patched and toasts, so a latch armed on the way in
  // left the user reading a restart warning for a setting that was never
  // stored.
  it('drops the deferred-restart notice when the save fails', async () => {
    await seed({ claudeThinking: { mode: 'budget', budgetTokens: 2048 } });
    const { getByTestId, queryByTestId } = render(ClaudeSessionAxesEditor);
    setBindingMock('UpdateSettings', async () => {
      throw new Error('settings write refused');
    });

    await fireEvent.change(getByTestId('settings-claude-thinking-mode'), {
      target: { value: '' },
    });

    await waitFor(() =>
      expect((getByTestId('settings-claude-thinking-mode') as HTMLSelectElement).value).toBe(
        'budget',
      ),
    );
    expect(queryByTestId('settings-claude-thinking-deferred')).toBeNull();
    expect(getByTestId('settings-claude-thinking-mode-hint').textContent).toContain(
      'Applies to running sessions too',
    );
  });

  // And when the axis moves again somewhere else — another window's save
  // lands on the same store — the notice describes a value nobody is running
  // toward any more.
  it('drops the deferred-restart notice when the mode changes elsewhere', async () => {
    await seed({ claudeThinking: { mode: 'budget', budgetTokens: 2048 } });
    const { getByTestId, queryByTestId } = render(ClaudeSessionAxesEditor);

    await fireEvent.change(getByTestId('settings-claude-thinking-mode'), {
      target: { value: '' },
    });
    await waitFor(() => expect(getByTestId('settings-claude-thinking-deferred')).toBeTruthy());

    await seed({ claudeThinking: { mode: 'budget', budgetTokens: 4096 } });

    await waitFor(() =>
      expect(queryByTestId('settings-claude-thinking-deferred')).toBeNull(),
    );
  });

});
