// Modal primitive contract:
//   - renders only when `open=true`.
//   - panel is role=dialog + aria-modal=true + aria-labelledby wired.
//   - Escape on the backdrop calls onClose.
//   - backdrop click calls onClose; click inside the panel does not.
//   - focus trap: Shift+Tab from the first focusable wraps to the last.
//   - width prop maps to the configured max-w-[] class.
//   - footer snippet renders when provided.

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import Harness from './ModalHarness.svelte';

async function flushFocus(): Promise<void> {
  // focusTrap uses queueMicrotask to focus the first focusable; flush it.
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe('<Modal>', () => {
  it('renders nothing when open=false', () => {
    const { queryByRole } = render(Harness, { props: { open: false } });
    expect(queryByRole('dialog')).toBeNull();
  });

  it('renders panel with ARIA contract when open=true', async () => {
    const { getByRole, getByText } = render(Harness, {
      props: { open: true, title: 'Add Project' },
    });
    await flushFocus();
    const dialog = getByRole('dialog');
    expect(dialog.getAttribute('aria-modal')).toBe('true');
    const labelId = dialog.getAttribute('aria-labelledby');
    expect(labelId).toBeTruthy();
    // aria-labelledby references the heading's id.
    expect(getByText('Add Project').id).toBe(labelId);
  });

  it('Escape on the backdrop calls onClose', async () => {
    const onClose = vi.fn();
    const { container } = render(Harness, { props: { onClose } });
    await flushFocus();
    const backdrop = container.querySelector('[data-modal-backdrop]');
    expect(backdrop).toBeTruthy();
    await fireEvent.keyDown(backdrop!, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });

  it('backdrop click calls onClose', async () => {
    const onClose = vi.fn();
    const { container } = render(Harness, { props: { onClose } });
    await flushFocus();
    const backdrop = container.querySelector('[data-modal-backdrop]') as HTMLElement;
    await fireEvent.click(backdrop);
    expect(onClose).toHaveBeenCalled();
  });

  it('click inside the panel does NOT call onClose', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(Harness, { props: { onClose } });
    await flushFocus();
    await fireEvent.click(getByTestId('modal-middle'));
    expect(onClose).not.toHaveBeenCalled();
  });

  it('focus trap wraps Shift+Tab from first focusable back to the last', async () => {
    const { getByTestId } = render(Harness, { props: {} });
    await flushFocus();
    const first = getByTestId('modal-first') as HTMLInputElement;
    const last = getByTestId('modal-last') as HTMLButtonElement;
    first.focus();
    expect(document.activeElement).toBe(first);
    const ev = new KeyboardEvent('keydown', {
      key: 'Tab',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    first.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(last);
  });

  it('focus trap wraps Tab from last focusable back to the first', async () => {
    const { getByTestId } = render(Harness, { props: {} });
    await flushFocus();
    const first = getByTestId('modal-first') as HTMLInputElement;
    const last = getByTestId('modal-last') as HTMLButtonElement;
    last.focus();
    const ev = new KeyboardEvent('keydown', {
      key: 'Tab',
      bubbles: true,
      cancelable: true,
    });
    last.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(first);
  });

  it('width="sm" maps to max-w-[380px]', async () => {
    const { container } = render(Harness, { props: { width: 'sm' } });
    await flushFocus();
    const panel = container.querySelector('[data-modal-panel]');
    expect(panel?.className).toContain('max-w-[380px]');
  });

  it('width="lg" maps to max-w-[800px]', async () => {
    const { container } = render(Harness, { props: { width: 'lg' } });
    await flushFocus();
    const panel = container.querySelector('[data-modal-panel]');
    expect(panel?.className).toContain('max-w-[800px]');
  });

  it('renders the footer snippet when provided', async () => {
    const { getByTestId } = render(Harness, { props: { withFooter: true } });
    await flushFocus();
    expect(getByTestId('modal-last')).toBeInTheDocument();
  });

  it('omits the footer element when no snippet is supplied', async () => {
    const { container } = render(Harness, { props: { withFooter: false } });
    await flushFocus();
    expect(container.querySelector('footer')).toBeNull();
  });
});
