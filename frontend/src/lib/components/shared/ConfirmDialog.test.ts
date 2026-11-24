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

  it.each([
    ['Cancel', 'onCancel', 'onConfirm'],
    ['Confirm', 'onConfirm', 'onCancel'],
  ] as const)('clicking %s fires %s, not %s', async (label, fired, other) => {
    const handlers = { onConfirm: vi.fn(), onCancel: vi.fn() };
    const { getByText } = render(ConfirmDialog, {
      props: { open: true, title: 't', description: 'd', ...handlers },
    });
    await flushFocus();
    await fireEvent.click(getByText(label));
    expect(handlers[fired]).toHaveBeenCalledTimes(1);
    expect(handlers[other]).not.toHaveBeenCalled();
  });

  it('Escape on the backdrop fires onCancel', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm,
        onCancel,
      },
    });
    await flushFocus();
    const backdrop = document.querySelector('[data-modal-backdrop]')!;
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('backdrop click fires onCancel', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        description: 'd',
        onConfirm,
        onCancel,
      },
    });
    await flushFocus();
    const backdrop = document.querySelector('[data-modal-backdrop]') as HTMLElement;
    await fireEvent.click(backdrop);
    expect(onCancel).toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  // A destructive prompt puts focus on Cancel so a stray Enter does not
  // confirm; otherwise focusTrap lands focus on Confirm.
  it.each([
    ['destructive=true paints confirm with the error palette and autofocuses cancel', { destructive: true }, 'bg-error', 'bg-accent', 'Cancel', 'Confirm'],
    ['default (non-destructive) paints confirm with the accent palette and autofocuses it', {}, 'bg-accent', 'bg-error', 'Confirm', 'Cancel'],
  ])('%s', async (_name, extra, palette, otherPalette, focused, unfocused) => {
    const { getByText } = render(ConfirmDialog, {
      props: { open: true, title: 't', description: 'd', ...extra, onConfirm: () => {}, onCancel: () => {} },
    });
    await flushFocus();
    const confirmBtn = getByText('Confirm');
    expect(confirmBtn.className).toContain(palette);
    expect(confirmBtn.className).not.toContain(otherPalette);
    expect(getByText(focused).hasAttribute('data-autofocus')).toBe(true);
    expect(getByText(unfocused).hasAttribute('data-autofocus')).toBe(false);
  });
});
