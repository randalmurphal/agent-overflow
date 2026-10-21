import { describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import MemorySettings from './MemorySettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';

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

// Shared memory is off by default; each provider switch turns that
// provider's own memory off; turning shared memory on beside a provider's
// own memory warns, naming the switch that turns it off.
describe('<MemorySettings>', () => {
  it('shows every switch off by default and no warning', async () => {
    await seed({}, { claude: true, codex: true });
    const { container, findByTestId, queryAllByTestId } = render(MemorySettings);
    await findByTestId('settings-memory');
    for (const label of ['Toggle shared memory', "Toggle Claude's own memory off", "Toggle Codex's own memory off"]) {
      expect(switchNamed(container, label).getAttribute('aria-checked')).toBe('false');
    }
    expect(queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
  });

  it('sends each switch as its own key', async () => {
    await seed({}, { claude: false, codex: false });
    const { container, findByTestId } = render(MemorySettings);
    await findByTestId('settings-memory');
    const update = getBindingMock('UpdateSettings')!;
    await fireEvent.click(switchNamed(container, 'Toggle shared memory'));
    expect(update.mock.calls.at(-1)![0]).toEqual({ sharedMemoryEnabled: true });
    await fireEvent.click(switchNamed(container, "Toggle Claude's own memory off"));
    expect(update.mock.calls.at(-1)![0]).toEqual({ claudeNativeMemoryDisabled: true });
    await fireEvent.click(switchNamed(container, "Toggle Codex's own memory off"));
    expect(update.mock.calls.at(-1)![0]).toEqual({ codexNativeMemoryDisabled: true });
  });

  it('warns for each provider whose own memory runs beside shared memory, until its switch turns it off', async () => {
    await seed({ sharedMemoryEnabled: true }, { claude: true, codex: true });
    const { container, findAllByTestId, queryAllByTestId } = render(MemorySettings);
    const warnings = await findAllByTestId('memory-overlap-warning');
    expect(warnings.map((w) => w.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      `Claude's own memory is on too, so its sessions keep two memories that can disagree. Turn it off with "Turn off Claude's own memory" below.`,
      `Codex's own memory is on too, so its sessions keep two memories that can disagree. Turn it off with "Turn off Codex's own memory" below.`,
    ]);
    await fireEvent.click(switchNamed(container, "Toggle Claude's own memory off"));
    await expect.poll(() => queryAllByTestId('memory-overlap-warning').map((w) => w.textContent ?? '')).toEqual([
      expect.stringContaining("Codex's own memory is on too"),
    ]);
    await fireEvent.click(switchNamed(container, 'Toggle shared memory'));
    await expect.poll(() => queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
  });

  it('does not warn for a provider whose own memory the user turned off', async () => {
    await seed({ sharedMemoryEnabled: true }, { claude: false, codex: false });
    const { findByTestId, queryAllByTestId } = render(MemorySettings);
    await findByTestId('settings-memory');
    await expect.poll(() => getBindingMock('GetNativeMemoryStatus')!.mock.calls.length).toBeGreaterThan(0);
    expect(queryAllByTestId('memory-overlap-warning')).toHaveLength(0);
  });

  it('keeps the newest read of the providers when an older one answers after it', async () => {
    await seed({}, { claude: false, codex: false });
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
});
