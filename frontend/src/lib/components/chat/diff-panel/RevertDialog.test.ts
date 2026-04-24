import { beforeAll, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import RevertDialog from './RevertDialog.svelte';
import { installAnimateShim } from '../../../../test/integration/_helpers';

beforeAll(installAnimateShim);

describe('<RevertDialog>', () => {
  it('does not render anything when closed', () => {
    const { queryByTestId } = render(RevertDialog, {
      open: false,
      checkpointTurnCount: 3,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    expect(queryByTestId('revert-dialog')).toBeNull();
  });

  it('renders with the checkpoint turn count in the title when open', () => {
    const { getByText } = render(RevertDialog, {
      open: true,
      checkpointTurnCount: 7,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    expect(getByText(/Revert to checkpoint 7/)).toBeInTheDocument();
  });

  it('defaults to reverting conversation and files', () => {
    const { getByTestId } = render(RevertDialog, {
      open: true,
      checkpointTurnCount: 1,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    expect((getByTestId('revert-mode-conversation-and-files') as HTMLInputElement).checked).toBe(true);
  });

  it('dispatches onRevert with the selected mode', async () => {
    const onRevert = vi.fn();
    const { getByTestId } = render(RevertDialog, {
      open: true,
      checkpointTurnCount: 2,
      provider: 'codex',
      onRevert,
      onCancel: vi.fn(),
    });

    await fireEvent.click(getByTestId('revert-apply'));
    expect(onRevert).toHaveBeenCalledWith('conversation-and-files');

    onRevert.mockClear();
    await fireEvent.click(getByTestId('revert-mode-conversation-only'));
    await fireEvent.click(getByTestId('revert-apply'));
    expect(onRevert).toHaveBeenCalledWith('conversation-only');
  });

  it('dispatches onCancel when the Cancel button is clicked', async () => {
    const onCancel = vi.fn();
    const { getByTestId } = render(RevertDialog, {
      open: true,
      checkpointTurnCount: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel,
    });
    await fireEvent.click(getByTestId('revert-cancel'));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('surfaces the Claude-specific note under conversation-only for Claude threads', () => {
    const { getByTestId } = render(RevertDialog, {
      open: true,
      checkpointTurnCount: 0,
      provider: 'claude',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    const conversationLabel = getByTestId('revert-mode-conversation-only').closest('label')!;
    expect(conversationLabel.textContent).toMatch(/Claude/i);
    expect(conversationLabel.textContent).toMatch(/fresh/i);
  });

  it('omits the Claude-specific note for Codex threads', () => {
    const { getByTestId } = render(RevertDialog, {
      open: true,
      checkpointTurnCount: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    const conversationLabel = getByTestId('revert-mode-conversation-only').closest('label')!;
    expect(conversationLabel.textContent).not.toMatch(/Claude/i);
  });

  it('resets the selection each time the dialog reopens', async () => {
    const { getByTestId, rerender } = render(RevertDialog, {
      open: true,
      checkpointTurnCount: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    await fireEvent.click(getByTestId('revert-mode-conversation-only'));
    expect((getByTestId('revert-mode-conversation-only') as HTMLInputElement).checked).toBe(true);

    await rerender({
      open: false,
      checkpointTurnCount: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    await rerender({
      open: true,
      checkpointTurnCount: 0,
      provider: 'codex',
      onRevert: vi.fn(),
      onCancel: vi.fn(),
    });
    expect((getByTestId('revert-mode-conversation-and-files') as HTMLInputElement).checked).toBe(true);
  });
});
