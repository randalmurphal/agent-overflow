// Covers submenu behaviour:
//   - trigger exposes aria-haspopup=menu + aria-expanded state.
//   - ArrowRight/Enter on the trigger opens the submenu immediately.
//   - ArrowLeft on the trigger with submenu open closes it.
//   - Escape inside the submenu closes it but does NOT close the parent.
//   - Selecting a nested MenuItem closes the submenu.

import { describe, expect, it, vi, beforeAll } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import Harness from './MenuSubmenuItemHarness.svelte';

class StubResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

beforeAll(() => {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
    StubResizeObserver as unknown as typeof ResizeObserver;
});

async function flushMicrotasks(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

function submenuTrigger(): HTMLElement {
  const el = document.querySelector<HTMLElement>('[data-submenu-trigger]');
  if (!el) throw new Error('submenu trigger not found');
  return el;
}

describe('<MenuSubmenuItem>', () => {
  it('trigger exposes aria-haspopup=menu and aria-expanded=false when closed', async () => {
    render(Harness);
    await flushMicrotasks();
    const trigger = submenuTrigger();
    expect(trigger.getAttribute('aria-haspopup')).toBe('menu');
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });

  it('ArrowRight on the trigger opens the submenu immediately', async () => {
    render(Harness);
    await flushMicrotasks();
    const trigger = submenuTrigger();
    trigger.focus();
    await fireEvent.keyDown(trigger, { key: 'ArrowRight' });
    await flushMicrotasks();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    // Nested items render inside a menu role.
    const menus = document.querySelectorAll('[role="menu"]');
    expect(menus.length).toBe(2);
  });

  it('Enter on the trigger opens the submenu immediately', async () => {
    render(Harness);
    await flushMicrotasks();
    const trigger = submenuTrigger();
    trigger.focus();
    await fireEvent.keyDown(trigger, { key: 'Enter' });
    await flushMicrotasks();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
  });

  it('ArrowLeft on the trigger closes an open submenu', async () => {
    render(Harness);
    await flushMicrotasks();
    const trigger = submenuTrigger();
    trigger.focus();
    await fireEvent.keyDown(trigger, { key: 'ArrowRight' });
    await flushMicrotasks();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    await fireEvent.keyDown(trigger, { key: 'ArrowLeft' });
    await flushMicrotasks();
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });

  it('selecting a nested item closes the submenu', async () => {
    const onLeafSelect = vi.fn();
    render(Harness, { props: { onLeafSelect } });
    await flushMicrotasks();

    const trigger = submenuTrigger();
    trigger.focus();
    await fireEvent.keyDown(trigger, { key: 'ArrowRight' });
    await flushMicrotasks();

    const nested = Array.from(document.querySelectorAll<HTMLElement>('[role="menuitem"]'))
      .find((el) => el.textContent?.trim() === 'Nested-One');
    expect(nested).toBeTruthy();
    await fireEvent.click(nested!);
    await flushMicrotasks();

    expect(onLeafSelect).toHaveBeenCalledWith('Nested-One');
    // After selection the submenu closes.
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });

  it('Escape inside the submenu closes the inner menu but not the parent', async () => {
    const onParentClose = vi.fn();
    render(Harness, { props: { onParentClose } });
    await flushMicrotasks();

    const trigger = submenuTrigger();
    trigger.focus();
    await fireEvent.keyDown(trigger, { key: 'ArrowRight' });
    await flushMicrotasks();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');

    await fireEvent.keyDown(trigger, { key: 'Escape' });
    await flushMicrotasks();

    expect(trigger.getAttribute('aria-expanded')).toBe('false');
    // Parent Menu's onClose should NOT have been called — inner Escape
    // stopPropagation prevents the event from bubbling up.
    expect(onParentClose).not.toHaveBeenCalled();
  });
});
