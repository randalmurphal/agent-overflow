import { describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import MemorySettings from './MemorySettings.svelte';
import { applySettingsSnapshot, loadSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { settingsAsV0019 } from '../../../test/helpers/olderComputer';
import { settingsFields } from './fields';
import { tick } from 'svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import type { OwnMemoryOff, Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { TransportError } from '../../transport/wsClient';

async function seed(overrides: Partial<Settings>, native: { claude: boolean; codex: boolean } | Error): Promise<void> {
  let saved: Settings = makeSettings(overrides);
  setBindingMock('GetSettings', async () => saved);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    saved = { ...saved, ...(patch as Partial<Settings>) };
    return saved;
  });
  setBindingMock('GetNativeMemoryStatus', async () => {
    if (native instanceof Error) throw native;
    return native;
  });
  await loadSettings();
}

function switchNamed(container: HTMLElement, label: string): HTMLButtonElement {
  const toggle = container.querySelector<HTMLButtonElement>(`[role="switch"][aria-label="${label}"]`);
  if (!toggle) throw new Error(`no switch ${label}`);
  return toggle;
}

function choice(container: HTMLElement, key: 'claudeOwnMemoryOff' | 'codexOwnMemoryOff', value: OwnMemoryOff): HTMLInputElement {
  const input = container.querySelector<HTMLInputElement>(`[data-testid="own-memory-${key}-${value}"] input[type="radio"]`);
  if (!input) throw new Error(`no choice ${key} ${value}`);
  return input;
}

const VALUES: OwnMemoryOff[] = ['agentOverflowMemory', 'always', 'never'];

// Shared memory is off by default; each provider's own memory is off by
// default where Agent Overflow memory is used, or in every conversation, or
// never; shared memory beside a provider's own memory, which only "never"
// allows, warns, naming the choice that turns it off.
describe('<MemorySettings>', () => {
  // The sentences about assistants' memory, in the page and in the settings
  // search, are there only while assistants are on.
  it('says nothing of assistants while they are off', async () => {
    await seed({ assistantsEnabled: false }, { claude: false, codex: false });
    const view = render(MemorySettings);
    await view.findByTestId('settings-memory');
    expect(view.container.textContent?.toLowerCase()).not.toContain('assistant');
    expect(settingsFields().find((field) => field.id === 'memory.shared')?.hint).not.toContain('Assistants');
    applySettingsSnapshot(makeSettings({ assistantsEnabled: true }));
    await tick();
    expect(view.container.textContent).toContain('Assistants always have memory.');
    expect(view.container.textContent).toContain('Off for assistants, the threads they start, and every conversation while shared memory is on.');
    expect(settingsFields().find((field) => field.id === 'memory.shared')?.hint).toContain('Assistants always have memory.');
  });

  it('chooses where Agent Overflow memory is used by default, with a line under each choice, and no warning', async () => {
    await seed({ sharedMemoryEnabled: true }, { claude: true, codex: true });
    const { container, findByTestId, queryAllByTestId } = render(MemorySettings);
    await findByTestId('settings-memory');
    await expect.poll(() => getBindingMock('GetNativeMemoryStatus')!.mock.calls.length).toBeGreaterThan(0);
    for (const [key, name] of [['claudeOwnMemoryOff', 'Claude'], ['codexOwnMemoryOff', 'Codex']] as const) {
      expect(VALUES.filter((value) => choice(container, key, value).checked)).toEqual(['agentOverflowMemory']);
      const lines = VALUES.map((value) => container.querySelector(`[data-testid="own-memory-${key}-${value}"]`)?.textContent?.replace(/\s+/g, ' ').trim());
      expect(lines).toEqual([
        `Where Agent Overflow memory is used Off for assistants, the threads they start, and every conversation while shared memory is on. Other conversations keep ${name}'s own memory as you set it up.`,
        'In every conversation Off in every conversation this app starts.',
        `Never ${name}'s own memory stays as you set it up in every conversation, so assistants keep it beside Agent Overflow memory.`,
      ]);
    }
    expect(queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
  });

  it('sends each choice as its own key', async () => {
    await seed({}, { claude: false, codex: false });
    const { container, findByTestId } = render(MemorySettings);
    await findByTestId('settings-memory');
    const update = getBindingMock('UpdateSettings')!;
    await fireEvent.click(switchNamed(container, 'Toggle shared memory'));
    expect(update.mock.calls.at(-1)![0]).toEqual({ sharedMemoryEnabled: true });
    for (const key of ['claudeOwnMemoryOff', 'codexOwnMemoryOff'] as const) {
      for (const value of ['never', 'always', 'agentOverflowMemory'] as const) {
        await fireEvent.click(choice(container, key, value));
        expect(update.mock.calls.at(-1)![0]).toEqual({ [key]: value });
        await expect.poll(() => choice(container, key, value).checked).toBe(true);
      }
    }
  });

  it('warns for each provider whose own memory runs beside shared memory, until its choice turns it off', async () => {
    await seed({ sharedMemoryEnabled: true, claudeOwnMemoryOff: 'never', codexOwnMemoryOff: 'never' }, { claude: true, codex: true });
    const { container, findAllByTestId, queryAllByTestId } = render(MemorySettings);
    const warnings = await findAllByTestId('memory-overlap-warning');
    expect(warnings.map((w) => w.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      `Claude's own memory is on too, so its sessions keep two memories that can disagree. Turn it off with "Turn off Claude's own memory" below.`,
      `Codex's own memory is on too, so its sessions keep two memories that can disagree. Turn it off with "Turn off Codex's own memory" below.`,
    ]);
    await fireEvent.click(choice(container, 'claudeOwnMemoryOff', 'agentOverflowMemory'));
    await expect.poll(() => queryAllByTestId('memory-overlap-warning').map((w) => w.textContent ?? '')).toEqual([
      expect.stringContaining("Codex's own memory is on too"),
    ]);
    await fireEvent.click(choice(container, 'codexOwnMemoryOff', 'always'));
    await expect.poll(() => queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
    // Never again warns again, and turning shared memory off ends it.
    await fireEvent.click(choice(container, 'codexOwnMemoryOff', 'never'));
    await expect.poll(() => queryAllByTestId('memory-overlap-warning')).toHaveLength(1);
    await fireEvent.click(switchNamed(container, 'Toggle shared memory'));
    await expect.poll(() => queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
  });

  it('does not warn for a provider whose own memory the user turned off', async () => {
    await seed({ sharedMemoryEnabled: true, claudeOwnMemoryOff: 'never', codexOwnMemoryOff: 'never' }, { claude: false, codex: false });
    const { findByTestId, queryAllByTestId } = render(MemorySettings);
    await findByTestId('settings-memory');
    await expect.poll(() => getBindingMock('GetNativeMemoryStatus')!.mock.calls.length).toBeGreaterThan(0);
    expect(queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
  });

  it('keeps the newest read of the providers when an older one answers after it', async () => {
    await seed({ claudeOwnMemoryOff: 'never' }, { claude: false, codex: false });
    const answers: Array<(status: { claude: boolean; codex: boolean }) => void> = [];
    setBindingMock('GetNativeMemoryStatus', () => new Promise((resolve) => { answers.push(resolve); }));
    const { container, findByTestId, queryAllByTestId } = render(MemorySettings);
    await findByTestId('settings-memory');
    await expect.poll(() => answers.length).toBe(1);
    // Turning shared memory on reads again; that newer read answers first.
    await fireEvent.click(switchNamed(container, 'Toggle shared memory'));
    await expect.poll(() => answers.length).toBe(2);
    answers[1]({ claude: true, codex: false });
    await expect.poll(() => queryAllByTestId('memory-overlap-warning')).toHaveLength(1);
    // The read the page opened with answers last, from before the change.
    answers[0]({ claude: false, codex: false });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(queryAllByTestId('memory-overlap-warning').map((w) => w.textContent ?? '')).toEqual([
      expect.stringContaining("Claude's own memory is on too"),
    ]);
  });

  it('shows why the providers could not be read', async () => {
    await seed({ sharedMemoryEnabled: true }, new Error('read Codex config: bad toml'));
    const { findByTestId, queryAllByTestId } = render(MemorySettings);
    expect((await findByTestId('memory-native-error')).textContent).toContain('read Codex config: bad toml');
    expect(queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
  });

  // v0.0.19's settings name none of the memory keys: the page says the
  // computer runs an older version, offers no editor and reads nothing.
  it('offers no memory settings on a computer whose release has none', async () => {
    await seed({}, new TransportError('method_not_found', 'unknown method'));
    stageBackend();
    try {
      applySettingsSnapshot(settingsAsV0019(), 'laptop');
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: MemorySettings, needsComputer: false } });
      expect((await view.findByTestId('settings-older-computer')).textContent).toBe('Laptop runs an older version: it has no memory settings.');
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(view.queryAllByRole('switch'), 'an editor for a key the computer lacks').toHaveLength(0);
    expect(view.queryAllByRole('radio'), 'a choice for a key the computer lacks').toHaveLength(0);
      expect(getBindingMock('GetNativeMemoryStatus')).not.toHaveBeenCalled();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      expect(view.queryAllByTestId('memory-native-error')).toHaveLength(0);
    } finally {
      resetStagedBackends();
      resetSettingsForTest();
    }
  });
});
