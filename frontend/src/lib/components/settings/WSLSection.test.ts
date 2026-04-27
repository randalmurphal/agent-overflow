import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import WSLSection, { resetWSLSectionCache } from './WSLSection.svelte';
import { setBindingMock, getBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { setRunMode, resetRunMode } from '../../../test/runMode';
import { getToasts, removeToast } from '../../stores/toast.svelte';

interface DistroRow {
  name: string;
  default: boolean;
  version: number;
  state: string;
}

const FIXTURE_DISTROS: DistroRow[] = [
  { name: 'Ubuntu-24.04', default: true, version: 2, state: 'Running' },
  { name: 'Debian', default: false, version: 2, state: 'Stopped' },
];

describe('<WSLSection>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    resetWSLSectionCache();
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  afterEach(() => {
    resetBindingMocks();
    resetRunMode();
    resetWSLSectionCache();
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  it('renders nothing when the backend is not running under WSL', async () => {
    setBindingMock('IsWSL', vi.fn(async () => false));
    // ListWSLDistros / GetWSLDistroPreference must not be called when
    // IsWSL=false; the section renders nothing and skips the live
    // queries entirely. If they were called, the bindings-app default
    // would throw "binding called without a mock", failing this test.

    const { queryByTestId } = render(WSLSection);
    await waitFor(() => {
      expect(queryByTestId('wsl-section-loading')).toBeFalsy();
    });
    expect(queryByTestId('wsl-section')).toBeFalsy();
    expect(queryByTestId('wsl-section-radiogroup')).toBeFalsy();
  });

  it('renders the distro list with default + state markers when on WSL', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => FIXTURE_DISTROS));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => 'Ubuntu-24.04'));

    const { findByTestId, getByTestId } = render(WSLSection);
    await findByTestId('wsl-section-radiogroup');

    expect(getByTestId('wsl-option-Ubuntu-24.04').textContent).toContain('Ubuntu-24.04');
    expect(getByTestId('wsl-option-Ubuntu-24.04').textContent).toContain('default');
    // Stopped distro shows the state in italic markup.
    expect(getByTestId('wsl-option-Debian').textContent).toContain('Stopped');
  });

  it('reflects the current preference as the checked radio', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => FIXTURE_DISTROS));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => 'Debian'));

    const { findByTestId } = render(WSLSection);
    await findByTestId('wsl-section-radiogroup');
    const debianRadio = (await findByTestId('wsl-option-Debian')).querySelector('input') as HTMLInputElement;
    await waitFor(() => expect(debianRadio.checked).toBe(true));
  });

  it('persists the selection through SetWSLDistroPreference on change', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => FIXTURE_DISTROS));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => 'Ubuntu-24.04'));
    const setMock = setBindingMock('SetWSLDistroPreference', vi.fn(async (name: unknown) => name as string));

    const { findByTestId } = render(WSLSection);
    await findByTestId('wsl-section-radiogroup');
    const debianRadio = (await findByTestId('wsl-option-Debian')).querySelector('input') as HTMLInputElement;
    await fireEvent.change(debianRadio);

    await waitFor(() => {
      expect(setMock).toHaveBeenCalledTimes(1);
    });
    expect(setMock.mock.calls[0][0]).toBe('Debian');
  });

  it('caches the cold-load Promise across remounts so wsl.exe is not re-spawned', async () => {
    const isWslMock = setBindingMock('IsWSL', vi.fn(async () => true));
    const listMock = setBindingMock('ListWSLDistros', vi.fn(async () => FIXTURE_DISTROS));
    const prefMock = setBindingMock('GetWSLDistroPreference', vi.fn(async () => 'Ubuntu-24.04'));

    const first = render(WSLSection);
    await first.findByTestId('wsl-section-radiogroup');
    first.unmount();

    const second = render(WSLSection);
    await second.findByTestId('wsl-section-radiogroup');

    // Each binding fired exactly once across both mounts. Without the
    // module-scoped cache, every Network-tab visit would re-shell wsl.exe.
    expect(isWslMock).toHaveBeenCalledTimes(1);
    expect(listMock).toHaveBeenCalledTimes(1);
    expect(prefMock).toHaveBeenCalledTimes(1);
  });

  it('surfaces a toast when the WSL list query rejects', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => {
      throw new Error('wsl.exe vanished');
    }));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => ''));

    const { queryByTestId } = render(WSLSection);
    await waitFor(() => {
      expect(queryByTestId('wsl-section-loading')).toBeFalsy();
    });

    // Section never renders supported state on a rejected list query.
    expect(queryByTestId('wsl-section')).toBeFalsy();
    expect(queryByTestId('wsl-section-radiogroup')).toBeFalsy();

    const toasts = getToasts();
    expect(toasts.some((t) => t.type === 'error' && /wsl/i.test(t.message))).toBe(true);
  });

  it('still renders the radio list when only GetWSLDistroPreference fails', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => FIXTURE_DISTROS));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => {
      throw new Error('decode wsl.json');
    }));

    const { findByTestId, queryByTestId } = render(WSLSection);
    // The list is independent of the preference — Promise.allSettled
    // means a rejected preference read renders the radio group with
    // no current selection rather than blanking the panel.
    await findByTestId('wsl-section-radiogroup');
    expect(queryByTestId('wsl-section-no-distros')).toBeFalsy();
  });

  it('surfaces a toast when SetWSLDistroPreference fails', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => FIXTURE_DISTROS));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => 'Ubuntu-24.04'));
    setBindingMock('SetWSLDistroPreference', vi.fn(async () => {
      throw new Error('write failed');
    }));

    const { findByTestId } = render(WSLSection);
    await findByTestId('wsl-section-radiogroup');
    const debianRadio = (await findByTestId('wsl-option-Debian')).querySelector('input') as HTMLInputElement;
    await fireEvent.change(debianRadio);

    await waitFor(() => {
      const toasts = getToasts();
      expect(toasts.some((t) => t.type === 'error' && /wsl/i.test(t.message))).toBe(true);
    });
  });

  it('reverts the radio and surfaces an error when SetWSLDistroPreference fails', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => FIXTURE_DISTROS));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => 'Ubuntu-24.04'));
    setBindingMock('SetWSLDistroPreference', vi.fn(async () => {
      throw new Error('write failed');
    }));

    const { findByTestId } = render(WSLSection);
    await findByTestId('wsl-section-radiogroup');
    const debianRadio = (await findByTestId('wsl-option-Debian')).querySelector('input') as HTMLInputElement;
    const ubuntuRadio = (await findByTestId('wsl-option-Ubuntu-24.04')).querySelector('input') as HTMLInputElement;
    await fireEvent.change(debianRadio);

    // After rejection: the previously-saved value (Ubuntu-24.04) wins
    // and Debian's radio is back to unchecked.
    await waitFor(() => {
      expect(ubuntuRadio.checked).toBe(true);
      expect(debianRadio.checked).toBe(false);
    });
  });

  it('renders the install hint when wsl.exe reports no distros', async () => {
    setBindingMock('IsWSL', vi.fn(async () => true));
    setBindingMock('ListWSLDistros', vi.fn(async () => []));
    setBindingMock('GetWSLDistroPreference', vi.fn(async () => ''));

    const { findByTestId } = render(WSLSection);
    const empty = await findByTestId('wsl-section-no-distros');
    expect(empty.textContent).toContain('wsl --install');
  });

  it('hides the section entirely in client mode (no IsWSL probe)', async () => {
    setRunMode('client');

    const { queryByTestId } = render(WSLSection);
    // No bindings set — if the component called IsWSL it would throw.
    // We just verify nothing renders.
    await waitFor(() => {
      expect(queryByTestId('wsl-section')).toBeFalsy();
      expect(queryByTestId('wsl-section-loading')).toBeFalsy();
      expect(queryByTestId('wsl-section-radiogroup')).toBeFalsy();
    });
    expect(getBindingMock('IsWSL')).toBeUndefined();
  });
});
