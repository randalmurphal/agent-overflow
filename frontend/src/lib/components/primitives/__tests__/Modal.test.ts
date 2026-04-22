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
import PaletteHarness from './ModalPaletteHarness.svelte';

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

  // --- New in Stage 1 redesign: xl width, padding variants, headerActions ---

  it('width="xl" maps to max-w-[960px]', async () => {
    const { container } = render(Harness, { props: { width: 'xl' } });
    await flushFocus();
    const panel = container.querySelector('[data-modal-panel]');
    expect(panel?.className).toContain('max-w-[960px]');
  });

  it('panel uses the redesigned radius + shadow tokens', async () => {
    const { container } = render(Harness);
    await flushFocus();
    const panel = container.querySelector('[data-modal-panel]')!;
    expect(panel.className).toContain('rounded-[var(--radius-card)]');
    expect(panel.className).toContain('shadow-modal');
    expect(panel.className).toContain('border-border-subtle');
  });

  it('backdrop uses the dimmer + heavier blur tokens', async () => {
    const { container } = render(Harness);
    await flushFocus();
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    expect(backdrop.className).toContain('bg-black/45');
    expect(backdrop.className).toContain('backdrop-blur-md');
  });

  it('padding="tight" reduces the body padding', async () => {
    const { container } = render(Harness, { props: { padding: 'tight' } });
    await flushFocus();
    // Body is the div containing the children snippet — between header
    // and footer. Find by class signature rather than testid.
    const body = container.querySelector('[data-modal-panel] > div:nth-of-type(1)')!;
    expect(body.className).toContain('px-4');
    expect(body.className).toContain('py-3');
  });

  it('padding="loose" enlarges the body padding', async () => {
    const { container } = render(Harness, { props: { padding: 'loose' } });
    await flushFocus();
    const body = container.querySelector('[data-modal-panel] > div:nth-of-type(1)')!;
    expect(body.className).toContain('px-6');
    expect(body.className).toContain('py-5');
  });

  it('padding defaults to "comfortable"', async () => {
    const { container } = render(Harness);
    await flushFocus();
    const body = container.querySelector('[data-modal-panel] > div:nth-of-type(1)')!;
    expect(body.className).toContain('px-5');
    expect(body.className).toContain('py-4');
  });

  it('renders headerActions snippet when supplied', async () => {
    const { getByTestId } = render(Harness, {
      props: { withFooter: false, withHeaderActions: true },
    });
    await flushFocus();
    expect(getByTestId('modal-header-action')).toBeInTheDocument();
  });

  it('does not render headerActions container when snippet is omitted', async () => {
    const { queryByTestId } = render(Harness);
    await flushFocus();
    expect(queryByTestId('modal-header-action')).toBeNull();
  });

  it('title is truncated + flex-1 so long titles do not push header actions off-screen', async () => {
    const { getByText } = render(Harness, {
      props: { title: 'An extremely long title that would overflow' },
    });
    await flushFocus();
    const heading = getByText('An extremely long title that would overflow');
    expect(heading.className).toContain('truncate');
    expect(heading.className).toContain('flex-1');
  });

  // Stage 4 refactor: Modal gained an `align` prop so command-palette
  // style surfaces (UnifiedThreadPicker, MessageSearch, KeybindingsCheatSheet)
  // can mount near the top of the viewport without reinventing their own
  // backdrop chrome.
  it('defaults align="center" which centers the panel vertically', async () => {
    const { container } = render(Harness);
    await flushFocus();
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    expect(backdrop.getAttribute('data-modal-align')).toBe('center');
    expect(backdrop.className).toContain('items-center');
    expect(backdrop.className).not.toContain('items-start');
  });

  it('align="top" anchors the panel 10vh below the top edge', async () => {
    const { container } = render(Harness, { props: { align: 'top' } });
    await flushFocus();
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    expect(backdrop.getAttribute('data-modal-align')).toBe('top');
    expect(backdrop.className).toContain('items-start');
    expect(backdrop.className).toContain('pt-[10vh]');
  });

  // Palette-shape extensions: Modal must support consumers that don't
  // have a title header (command palette, message search). When `title`
  // is omitted, aria-labelledby must drop and aria-label takes over.
  describe('title-less "palette" shape', () => {
    it('falls back to aria-label when title is omitted', async () => {
      const { getByRole } = render(PaletteHarness, {
        props: { ariaLabel: 'Command palette' },
      });
      await flushFocus();
      const dialog = getByRole('dialog');
      expect(dialog.hasAttribute('aria-labelledby')).toBe(false);
      expect(dialog.getAttribute('aria-label')).toBe('Command palette');
    });

    it('renders the custom header snippet in place of the default chrome', async () => {
      const { getByTestId, queryByRole } = render(PaletteHarness, {
        props: { withHeader: true },
      });
      await flushFocus();
      // Custom header renders.
      expect(getByTestId('modal-custom-header')).toBeInTheDocument();
      // Default <header> with an <h2> title is NOT rendered — the caller
      // replaced it entirely.
      expect(queryByRole('heading', { level: 2 })).toBeNull();
    });

    it('renders NEITHER default header NOR custom header when both title and header are absent', async () => {
      const { container, queryByRole } = render(PaletteHarness, {
        props: { withHeader: false },
      });
      await flushFocus();
      expect(queryByRole('heading', { level: 2 })).toBeNull();
      // No <header> element was rendered inside the panel.
      const panel = container.querySelector('[data-modal-panel]')!;
      expect(panel.querySelector('header')).toBeNull();
    });

    it('padding="none" produces an un-padded body', async () => {
      const { container } = render(PaletteHarness, {
        props: { padding: 'none' },
      });
      await flushFocus();
      const panel = container.querySelector('[data-modal-panel]')!;
      // The direct child wrapping the body snippet is the scroller
      // with overflow-y-auto. Its class string should contain none of
      // the default padding tokens.
      const body = panel.querySelector('.overflow-y-auto')!;
      const cls = body.className;
      expect(cls).not.toMatch(/\bpx-(4|5|6)\b/);
      expect(cls).not.toMatch(/\bpy-(3|4|5)\b/);
    });

    it('padding="tight" still produces the tight padding token set', async () => {
      const { container } = render(PaletteHarness, {
        props: { padding: 'tight', withHeader: true },
      });
      await flushFocus();
      const body = container.querySelector('[data-modal-panel] .overflow-y-auto')!;
      expect(body.className).toContain('px-4');
      expect(body.className).toContain('py-3');
    });
  });
});
