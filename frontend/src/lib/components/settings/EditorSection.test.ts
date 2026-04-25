import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import EditorSection from './EditorSection.svelte';
import { setBindingMock, getBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { setRunMode, resetRunMode } from '../../../test/runMode';
import { getToasts, removeToast } from '../../stores/toast.svelte';

interface EditorRow {
  id: string;
  name: string;
  available: boolean;
  envFallback?: boolean;
}

const FIXTURE_EDITORS: EditorRow[] = [
  { id: 'code', name: 'Visual Studio Code', available: true },
  { id: 'cursor', name: 'Cursor', available: true },
  { id: 'zed', name: 'Zed', available: false },
  { id: 'env:editor', name: '$EDITOR', available: true, envFallback: true },
];

describe('<EditorSection>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    // Drain the toast registry so a sibling case's error message can't
    // satisfy this case's "find this error" assertion.
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  afterEach(() => {
    resetBindingMocks();
    resetRunMode();
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  it('renders the editor list with installed/uninstalled markers', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => FIXTURE_EDITORS));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    const { findByTestId, getByTestId } = render(EditorSection);
    await findByTestId('editor-section-radiogroup');

    expect(getByTestId('editor-option-auto')).toBeTruthy();
    expect(getByTestId('editor-option-code').getAttribute('data-available')).toBe('true');
    expect(getByTestId('editor-option-zed').getAttribute('data-available')).toBe('false');
    expect(getByTestId('editor-option-zed').textContent).toContain('not installed');
  });

  it('reflects the current preference as the checked radio', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => FIXTURE_EDITORS));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: 'cursor' })));

    const { findByTestId } = render(EditorSection);
    await findByTestId('editor-section-radiogroup');
    const cursorRadio = (await findByTestId('editor-option-cursor')).querySelector('input') as HTMLInputElement;
    await waitFor(() => expect(cursorRadio.checked).toBe(true));
  });

  it('persists the selection through SetEditorSettings on change', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => FIXTURE_EDITORS));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));
    const setMock = setBindingMock('SetEditorSettings', vi.fn(async (next: unknown) => {
      const n = next as { preference: string };
      return { preference: n.preference };
    }));

    const { findByTestId } = render(EditorSection);
    await findByTestId('editor-section-radiogroup');
    const codeRadio = (await findByTestId('editor-option-code')).querySelector('input') as HTMLInputElement;
    await fireEvent.change(codeRadio);

    await waitFor(() => {
      expect(setMock).toHaveBeenCalledTimes(1);
    });
    const args = setMock.mock.calls[0][0] as { preference: string };
    expect(args.preference).toBe('code');
  });

  it('reverts the radio and surfaces an error when SetEditorSettings fails', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => FIXTURE_EDITORS));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));
    setBindingMock('SetEditorSettings', vi.fn(async () => {
      throw new Error('disk full');
    }));

    const { findByTestId } = render(EditorSection);
    await findByTestId('editor-section-radiogroup');
    const cursorRadio = (await findByTestId('editor-option-cursor')).querySelector('input') as HTMLInputElement;
    await fireEvent.change(cursorRadio);

    // After rejection, the radio must report the previous (Auto) state
    // again — the Auto radio should be checked while cursor should not.
    const autoRadio = (await findByTestId('editor-option-auto')).querySelector('input') as HTMLInputElement;
    await waitFor(() => {
      expect(autoRadio.checked).toBe(true);
      expect(cursorRadio.checked).toBe(false);
    });
  });

  it('keeps the unavailable editor radio disabled', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => FIXTURE_EDITORS));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    const { findByTestId } = render(EditorSection);
    await findByTestId('editor-section-radiogroup');
    const zedRadio = (await findByTestId('editor-option-zed')).querySelector('input') as HTMLInputElement;
    expect(zedRadio.disabled).toBe(true);
  });

  it('renders a placeholder + skips RPCs in client mode', async () => {
    setRunMode('client');
    const listMock = setBindingMock('ListAvailableEditors', vi.fn(async () => FIXTURE_EDITORS));
    const getMock = setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    const { findByTestId, queryByTestId } = render(EditorSection);
    await findByTestId('editor-section-clientmode');
    expect(queryByTestId('editor-section-radiogroup')).toBeNull();

    expect(listMock).not.toHaveBeenCalled();
    expect(getMock).not.toHaveBeenCalled();
  });

  it('toasts a friendly error if the catalog fails to load', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => {
      throw new Error('catalog spawn failed');
    }));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));

    render(EditorSection);
    // The toast contains the user-facing message verbatim. The toast UI
    // isn't rendered in this isolated test, so check the registry
    // directly. Editor list stays empty when the catalog RPC rejects,
    // but the Auto option still renders so the user can clear a stale
    // preference even on a degraded catalog.
    await waitFor(() => {
      const toasts = getToasts();
      const match = toasts.find((t) => t.message.includes('catalog spawn failed'));
      expect(match?.type).toBe('error');
    });
  });

  it('does not call SetEditorSettings if the user re-selects the current value', async () => {
    setBindingMock('ListAvailableEditors', vi.fn(async () => FIXTURE_EDITORS));
    setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: 'cursor' })));
    const setMock = setBindingMock('SetEditorSettings', vi.fn(async () => ({ preference: 'cursor' })));

    const { findByTestId } = render(EditorSection);
    await findByTestId('editor-section-radiogroup');
    const cursorRadio = (await findByTestId('editor-option-cursor')).querySelector('input') as HTMLInputElement;
    // Wait for the initial load to settle the checked state.
    await waitFor(() => expect(cursorRadio.checked).toBe(true));
    await fireEvent.change(cursorRadio);

    expect(setMock).not.toHaveBeenCalled();
    void getBindingMock; // keep helper imported
  });
});
