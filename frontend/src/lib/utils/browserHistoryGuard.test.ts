import { afterEach, describe, expect, it, vi } from 'vitest';
import { installBrowserHistoryGuard } from './browserHistoryGuard';

describe('installBrowserHistoryGuard', () => {
  let cleanup = () => {};
  const removeListeners: Array<() => void> = [];

  afterEach(() => {
    cleanup();
    cleanup = () => {};
    for (const remove of removeListeners.splice(0)) remove();
    Reflect.deleteProperty(window.navigator, 'platform');
    vi.restoreAllMocks();
  });

  function on(type: keyof DocumentEventMap, listener: EventListener): void {
    document.addEventListener(type, listener);
    removeListeners.push(() => document.removeEventListener(type, listener));
  }

  // Own-property shadow over the prototype getter; removed in afterEach.
  function stubPlatform(value: string): void {
    Object.defineProperty(window.navigator, 'platform', { value, configurable: true });
  }

  it('consumes Alt+ArrowLeft and Alt+ArrowRight before app listeners see them', () => {
    stubPlatform('Win32');
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

  it('leaves Alt+Arrow alone on macOS, where it is word navigation, not a history gesture', () => {
    stubPlatform('MacIntel');
    cleanup = installBrowserHistoryGuard();

    const textarea = document.createElement('textarea');
    document.body.append(textarea);

    try {
      for (const target of [textarea, document.body]) {
        const seen = vi.fn();
        on('keydown', seen);
        const event = new KeyboardEvent('keydown', {
          bubbles: true,
          cancelable: true,
          altKey: true,
          key: 'ArrowLeft',
        });

        expect(target.dispatchEvent(event)).toBe(true);
        expect(event.defaultPrevented).toBe(false);
        expect(seen).toHaveBeenCalledTimes(1);
      }
    } finally {
      textarea.remove();
    }
  });

  it('consumes Alt+Arrow on Windows/Linux even with a text caret focused', () => {
    stubPlatform('Win32');
    cleanup = installBrowserHistoryGuard();
    const seen = vi.fn();
    on('keydown', seen);

    const textarea = document.createElement('textarea');
    document.body.append(textarea);

    try {
      const event = new KeyboardEvent('keydown', {
        bubbles: true,
        cancelable: true,
        altKey: true,
        key: 'ArrowLeft',
      });

      expect(textarea.dispatchEvent(event)).toBe(false);
      expect(event.defaultPrevented).toBe(true);
      expect(seen).not.toHaveBeenCalled();
    } finally {
      textarea.remove();
    }
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

  describe('context menu policy', () => {
    const mounted: Element[] = [];

    afterEach(() => {
      for (const el of mounted.splice(0)) el.remove();
    });

    function mount<T extends HTMLElement>(el: T): T {
      document.body.appendChild(el);
      mounted.push(el);
      return el;
    }

    function rightClick(target: EventTarget, init?: MouseEventInit): MouseEvent {
      const event = new MouseEvent('contextmenu', {
        bubbles: true,
        cancelable: true,
        button: 2,
        ...init,
      });
      target.dispatchEvent(event);
      return event;
    }

    it('suppresses the native menu on non-editable surfaces without blocking app handlers', () => {
      cleanup = installBrowserHistoryGuard();
      const seen = vi.fn();
      on('contextmenu', seen);

      const event = rightClick(mount(document.createElement('div')));
      expect(event.defaultPrevented).toBe(true);
      expect(seen).toHaveBeenCalledTimes(1);
    });

    it('allows the native menu in writable textareas and text inputs', () => {
      cleanup = installBrowserHistoryGuard();

      expect(rightClick(mount(document.createElement('textarea'))).defaultPrevented).toBe(false);

      const input = mount(document.createElement('input'));
      input.type = 'text';
      expect(rightClick(input).defaultPrevented).toBe(false);
    });

    it('suppresses the native menu on non-text and disabled inputs', () => {
      cleanup = installBrowserHistoryGuard();

      const checkbox = mount(document.createElement('input'));
      checkbox.type = 'checkbox';
      expect(rightClick(checkbox).defaultPrevented).toBe(true);

      const disabled = mount(document.createElement('textarea'));
      disabled.disabled = true;
      expect(rightClick(disabled).defaultPrevented).toBe(true);
    });

    it('allows read-only fields only when text is selected', () => {
      cleanup = installBrowserHistoryGuard();

      const readonly = mount(document.createElement('textarea'));
      readonly.readOnly = true;
      readonly.value = 'some text';
      expect(rightClick(readonly).defaultPrevented).toBe(true);

      readonly.setSelectionRange(0, 4);
      expect(rightClick(readonly).defaultPrevented).toBe(false);
    });

    it('allows the native menu in contentEditable elements', () => {
      cleanup = installBrowserHistoryGuard();

      const editable = mount(document.createElement('div'));
      editable.contentEditable = 'true';
      expect(rightClick(editable).defaultPrevented).toBe(false);
    });

    it('allows the native menu only when the click lands on selected text', () => {
      cleanup = installBrowserHistoryGuard();
      const div = mount(document.createElement('div'));

      const rect = { left: 10, right: 110, top: 20, bottom: 40 };
      vi.spyOn(window, 'getSelection').mockReturnValue({
        isCollapsed: false,
        rangeCount: 1,
        getRangeAt: () => ({ getClientRects: () => [rect] }),
      } as unknown as Selection);

      expect(rightClick(div, { clientX: 50, clientY: 30 }).defaultPrevented).toBe(false);
      expect(rightClick(div, { clientX: 200, clientY: 30 }).defaultPrevented).toBe(true);
    });
  });
});
