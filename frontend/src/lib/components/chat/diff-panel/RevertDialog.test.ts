import { beforeAll, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import RevertDialog from './RevertDialog.svelte';
import { installAnimateShim } from '../../../../test/integration/_helpers';

beforeAll(installAnimateShim);

describe('<RevertDialog>', () => {
  it('does not render anything when closed', () => {
    const { queryByTestId } = render(RevertDialog, {
      open: false,
      turnIndex: 3,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    expect(queryByTestId('revert-dialog')).toBeNull();
  });

  it('renders with the turn index in the title when open', () => {
    const { getByText } = render(RevertDialog, {
      open: true,
      turnIndex: 7,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    expect(getByText(/Revert to turn 7/)).toBeInTheDocument();
  });

  it('defaults to the combined (revert-both) mode on open', () => {
    const { getByTestId } = render(RevertDialog, {
      open: true,
      turnIndex: 1,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    const both = getByTestId('revert-mode-both') as HTMLInputElement;
    expect(both.checked).toBe(true);
  });

  it('dispatches onRevert with "revert-both" when Apply is clicked in the default state', async () => {
    const onRevert = vi.fn();
    const { getByTestId } = render(RevertDialog, {
      open: true,
      turnIndex: 2,
      provider: 'codex',
      onRevert,
      onCancel: vi.fn(),
    });
    await fireEvent.click(getByTestId('revert-apply'));
    expect(onRevert).toHaveBeenCalledTimes(1);
    expect(onRevert).toHaveBeenCalledWith('revert-both');
  });

  it('dispatches onRevert with the selected mode after changing the radio', async () => {
    const onRevert = vi.fn();
    const { getByTestId } = render(RevertDialog, {
      open: true,
      turnIndex: 2,
      provider: 'codex',
      onRevert,
      onCancel: vi.fn(),
    });
    await fireEvent.click(getByTestId('revert-mode-conversation'));
    await fireEvent.click(getByTestId('revert-apply'));
    expect(onRevert).toHaveBeenCalledWith('revert-conversation');

    onRevert.mockClear();
    await fireEvent.click(getByTestId('revert-mode-code'));
    await fireEvent.click(getByTestId('revert-apply'));
    expect(onRevert).toHaveBeenCalledWith('revert-code');
  });

  it('dispatches onRevert with "fork" when the fork shortcut is used', async () => {
    const onRevert = vi.fn();
    const { getByTestId } = render(RevertDialog, {
      open: true,
      turnIndex: 0,
      provider: 'claude',
      onRevert,
      onCancel: vi.fn(),
    });
    await fireEvent.click(getByTestId('revert-fork'));
    expect(onRevert).toHaveBeenCalledWith('fork');
  });

  it('dispatches onCancel when the Cancel button is clicked', async () => {
    const onCancel = vi.fn();
    const { getByTestId } = render(RevertDialog, {
      open: true,
      turnIndex: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel,
    });
    await fireEvent.click(getByTestId('revert-cancel'));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('surfaces Claude-specific note under revert-conversation when provider=claude', () => {
    const { getByTestId } = render(RevertDialog, {
      open: true,
      turnIndex: 0,
      provider: 'claude',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    // The conversation row contains the Claude-specific sentence.
    const conversationLabel = getByTestId('revert-mode-conversation').closest('label')!;
    expect(conversationLabel.textContent).toMatch(/Claude/i);
    expect(conversationLabel.textContent).toMatch(/not remember/i);
  });

  it('omits the Claude-specific note for Codex threads', () => {
    const { getByTestId } = render(RevertDialog, {
      open: true,
      turnIndex: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    const conversationLabel = getByTestId('revert-mode-conversation').closest('label')!;
    expect(conversationLabel.textContent).not.toMatch(/Claude/i);
    expect(conversationLabel.textContent).not.toMatch(/not remember/i);
  });

  it('resets the selection to "revert-both" each time the dialog reopens', async () => {
    const { component, getByTestId, rerender } = render(RevertDialog, {
      open: true,
      turnIndex: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    await fireEvent.click(getByTestId('revert-mode-code'));
    expect((getByTestId('revert-mode-code') as HTMLInputElement).checked).toBe(true);

    // Close then reopen — the selection should snap back to the default.
    await rerender({
      open: false,
      turnIndex: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    await rerender({
      open: true,
      turnIndex: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    expect((getByTestId('revert-mode-both') as HTMLInputElement).checked).toBe(true);
    // Silence unused binding warning from the harness.
    void component;
  });
});
