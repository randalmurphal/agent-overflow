import { afterEach, describe, expect, it, vi } from 'vitest';
import { installBrowserHistoryGuard } from './browserHistoryGuard';

describe('installBrowserHistoryGuard', () => {
  let cleanup = () => {};
  const removeListeners: Array<() => void> = [];

  afterEach(() => {
    cleanup();
    cleanup = () => {};
    for (const remove of removeListeners.splice(0)) remove();
    vi.restoreAllMocks();
  });

  function on(type: keyof DocumentEventMap, listener: EventListener): void {
    document.addEventListener(type, listener);
    removeListeners.push(() => document.removeEventListener(type, listener));
  }

  it('consumes Alt+ArrowLeft and Alt+ArrowRight before app listeners see them', () => {
    cleanup = installBrowserHistoryGuard();
    const seen = vi.fn();
    on('keydown', seen);

    for (const key of ['ArrowLeft', 'ArrowRight']) {
      const event = new KeyboardEvent('keydown', {
        bubbles: true,
        cancelable: true,
        altKey: true,
        key,
      });

      expect(document.dispatchEvent(event)).toBe(false);
      expect(event.defaultPrevented).toBe(true);
    }
    expect(seen).not.toHaveBeenCalled();
  });

  it('leaves ordinary keys alone', () => {
    cleanup = installBrowserHistoryGuard();
    const seen = vi.fn();
    on('keydown', seen);

    const event = new KeyboardEvent('keydown', {
      bubbles: true,
      cancelable: true,
      key: 'ArrowLeft',
    });

    expect(document.dispatchEvent(event)).toBe(true);
    expect(event.defaultPrevented).toBe(false);
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it('consumes browser back and forward mouse buttons', () => {
    cleanup = installBrowserHistoryGuard();
    const seen = vi.fn();
    on('mouseup', seen);

    for (const button of [3, 4]) {
      const event = new MouseEvent('mouseup', {
        bubbles: true,
        cancelable: true,
        button,
      });

      expect(document.dispatchEvent(event)).toBe(false);
      expect(event.defaultPrevented).toBe(true);
    }
    expect(seen).not.toHaveBeenCalled();
  });

  it('leaves normal mouse buttons alone', () => {
    cleanup = installBrowserHistoryGuard();
    const seen = vi.fn();
    on('mouseup', seen);

    const event = new MouseEvent('mouseup', {
      bubbles: true,
      cancelable: true,
      button: 0,
    });

    expect(document.dispatchEvent(event)).toBe(true);
    expect(event.defaultPrevented).toBe(false);
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it('prevents the native browser context menu without blocking app handlers', () => {
    cleanup = installBrowserHistoryGuard();
    const seen = vi.fn();
    on('contextmenu', seen);

    const event = new MouseEvent('contextmenu', {
      bubbles: true,
      cancelable: true,
      button: 2,
    });

    expect(document.dispatchEvent(event)).toBe(false);
    expect(event.defaultPrevented).toBe(true);
    expect(seen).toHaveBeenCalledTimes(1);
  });
});
