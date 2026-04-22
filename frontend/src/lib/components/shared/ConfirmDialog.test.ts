// ConfirmDialog contract (post-consolidation onto Modal primitive):
//   - renders nothing when open=false
//   - shows title + description when open=true
//   - clicking Cancel fires onCancel, not onConfirm
//   - clicking Confirm fires onConfirm, not onCancel
//   - Escape closes via onCancel (never onConfirm — dismissing a
//     destructive prompt must not be interpreted as confirmation)
//   - backdrop click closes via onCancel
//   - destructive=true swaps the confirm button to the error palette
//   - confirm button carries the data-autofocus marker so focusTrap
//     lands focus there on open

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ConfirmDialog from './ConfirmDialog.svelte';

async function flushFocus(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe('<ConfirmDialog>', () => {
  it('renders nothing when open=false', () => {
    const { queryByRole } = render(ConfirmDialog, {
      props: {
        open: false,
        title: 'Delete?',
        description: 'Really?',
        onConfirm: () => {},
        onCancel: () => {},
      },
    });
    expect(queryByRole('dialog')).toBeNull();
  });

  it('shows title + description when open', async () => {
    const { getByRole, getByText } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 'Archive thread',
        description: 'This removes the thread from the sidebar.',
        onConfirm: () => {},
        onCancel: () => {},
      },
    });
    await flushFocus();
    expect(getByRole('dialog')).toBeInTheDocument();
    expect(getByText('Archive thread')).toBeInTheDocument();
    expect(getByText('This removes the thread from the sidebar.')).toBeInTheDocument();
  });

  it('renders the requested confirm/cancel labels', async () => {
    const { getByText } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        confirmLabel: 'Yes, archive',
        cancelLabel: 'Nope',
        onConfirm: () => {},
        onCancel: () => {},
      },
    });
    await flushFocus();
    expect(getByText('Yes, archive')).toBeInTheDocument();
    expect(getByText('Nope')).toBeInTheDocument();
  });

  it('clicking Cancel fires onCancel, not onConfirm', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const { getByText } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm,
        onCancel,
      },
    });
    await flushFocus();
    await fireEvent.click(getByText('Cancel'));
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('clicking Confirm fires onConfirm, not onCancel', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const { getByText } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm,
        onCancel,
      },
    });
    await flushFocus();
    await fireEvent.click(getByText('Confirm'));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('Escape on the backdrop fires onCancel', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const { container } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm,
        onCancel,
      },
    });
    await flushFocus();
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('backdrop click fires onCancel', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const { container } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm,
        onCancel,
      },
    });
    await flushFocus();
    const backdrop = container.querySelector('[data-modal-backdrop]') as HTMLElement;
    await fireEvent.click(backdrop);
    expect(onCancel).toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('destructive=true paints the confirm button with the error palette', async () => {
    const { getByText } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        destructive: true,
        onConfirm: () => {},
        onCancel: () => {},
      },
    });
    await flushFocus();
    const confirmBtn = getByText('Confirm');
    expect(confirmBtn.className).toContain('bg-error');
    expect(confirmBtn.className).not.toContain('bg-accent');
  });

  it('default (non-destructive) uses the accent palette for confirm', async () => {
    const { getByText } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm: () => {},
        onCancel: () => {},
      },
    });
    await flushFocus();
    const confirmBtn = getByText('Confirm');
    expect(confirmBtn.className).toContain('bg-accent');
    expect(confirmBtn.className).not.toContain('bg-error');
  });

  it('confirm button carries data-autofocus so focusTrap lands focus there', async () => {
    const { getByText } = render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm: () => {},
        onCancel: () => {},
      },
    });
    await flushFocus();
    const confirmBtn = getByText('Confirm');
    expect(confirmBtn.hasAttribute('data-autofocus')).toBe(true);
  });
});
