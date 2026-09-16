import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  composerPickerSelectionLabel,
  isAnyComposerPickerOpen,
  registerComposerPicker,
  registerComposerPickerFallbackAnchor,
  resetComposerPickerRegistryForTest,
  resolvePickerAnchor,
  toggleComposerPicker,
  type ComposerPickerHandle,
} from './composerPickerRegistry.svelte';

function makeHandle(): {
  handle: ComposerPickerHandle;
  open: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  isOpen: ReturnType<typeof vi.fn>;
  setOpen: (v: boolean) => void;
} {
  let _open = false;
  const isOpen = vi.fn(() => _open);
  const open = vi.fn(() => {
    _open = true;
  });
  const close = vi.fn(() => {
    _open = false;
  });
  return {
    handle: { isOpen, open, close },
    open,
    close,
    isOpen,
    setOpen: (v) => {
      _open = v;
    },
  };
}

describe('composerPickerRegistry', () => {
  beforeEach(() => resetComposerPickerRegistryForTest());
  afterEach(() => resetComposerPickerRegistryForTest());

  it('reacts to selection, registration, and cleanup without leaking across panes', () => {
    let label = $state('High');
    let observed = '';
    const stop = $effect.root(() => {
      $effect(() => { observed = composerPickerSelectionLabel('main', 'effort'); });
    });
    try {
      flushSync();
      expect(observed).toBe('');
      const first = registerComposerPicker('main', 'effort', {
        ...makeHandle().handle, selectionLabel: () => label,
      });
      flushSync();
      expect(observed).toBe('High');
      label = 'Low';
      flushSync();
      expect(observed).toBe('Low');
      expect(composerPickerSelectionLabel('other', 'effort')).toBe('');
      const second = registerComposerPicker('main', 'effort', {
        ...makeHandle().handle, selectionLabel: () => 'Medium',
      });
      first();
      flushSync();
      expect(observed).toBe('Medium');
      second();
      flushSync();
      expect(observed).toBe('');
    } finally {
      stop();
    }
  });

  describe('toggleComposerPicker', () => {
    it('calls open() when the picker is closed', () => {
      const a = makeHandle();
      registerComposerPicker('pane-main', 'model', a.handle);
      const ok = toggleComposerPicker('pane-main', 'model');
      expect(ok).toBe(true);
      expect(a.open).toHaveBeenCalledTimes(1);
      expect(a.close).not.toHaveBeenCalled();
    });

    it('calls close() when the picker is open', () => {
      const a = makeHandle();
      a.setOpen(true);
      registerComposerPicker('pane-main', 'model', a.handle);
      const ok = toggleComposerPicker('pane-main', 'model');
      expect(ok).toBe(true);
      expect(a.close).toHaveBeenCalledTimes(1);
      expect(a.open).not.toHaveBeenCalled();
    });

    it('returns false when no handle is registered for the (pane, picker)', () => {
      const a = makeHandle();
      registerComposerPicker('pane-main', 'model', a.handle);
      // Different picker, same pane.
      expect(toggleComposerPicker('pane-main', 'effort')).toBe(false);
      // Different pane, same picker.
      expect(toggleComposerPicker('pane-other', 'model')).toBe(false);
      expect(a.open).not.toHaveBeenCalled();
    });

    it('returns false when paneId is null (composer-less surfaces)', () => {
      const a = makeHandle();
      registerComposerPicker('pane-main', 'model', a.handle);
      expect(toggleComposerPicker(null, 'model')).toBe(false);
    });

    it('routes to the per-pane handle in multi-pane mode', () => {
      const a = makeHandle();
      const b = makeHandle();
      registerComposerPicker('pane-main', 'model', a.handle);
      registerComposerPicker('pane-secondary', 'model', b.handle);
      toggleComposerPicker('pane-secondary', 'model');
      expect(b.open).toHaveBeenCalledTimes(1);
      expect(a.open).not.toHaveBeenCalled();
    });
  });

  describe('register cleanup', () => {
    it('cleanup removes the handle', () => {
      const a = makeHandle();
      const cleanup = registerComposerPicker('pane-main', 'model', a.handle);
      cleanup();
      expect(toggleComposerPicker('pane-main', 'model')).toBe(false);
    });

    it('a stale cleanup does NOT evict a fresh handle for the same key', () => {
      // Component A registers, then unmounts. Component B (newer) registers
      // under the same (pane, picker) key BEFORE A's $effect cleanup ran.
      // A's cleanup must not delete B's handle.
      const a = makeHandle();
      const b = makeHandle();
      const cleanupA = registerComposerPicker('pane-main', 'model', a.handle);
      registerComposerPicker('pane-main', 'model', b.handle);
      cleanupA();
      // B should still be the active handle.
      toggleComposerPicker('pane-main', 'model');
      expect(b.open).toHaveBeenCalledTimes(1);
      expect(a.open).not.toHaveBeenCalled();
    });

    it('re-registering the same key overwrites the previous handle', () => {
      const a = makeHandle();
      const b = makeHandle();
      registerComposerPicker('pane-main', 'model', a.handle);
      registerComposerPicker('pane-main', 'model', b.handle);
      toggleComposerPicker('pane-main', 'model');
      expect(b.open).toHaveBeenCalledTimes(1);
      expect(a.open).not.toHaveBeenCalled();
    });
  });

  describe('isAnyComposerPickerOpen', () => {
    it('returns false when no pickers are registered', () => {
      expect(isAnyComposerPickerOpen()).toBe(false);
    });

    it('returns false when registered pickers are all closed', () => {
      const a = makeHandle();
      const b = makeHandle();
      registerComposerPicker('pane-main', 'model', a.handle);
      registerComposerPicker('pane-main', 'effort', b.handle);
      expect(isAnyComposerPickerOpen()).toBe(false);
    });

    it('returns true when any picker reports open', () => {
      const a = makeHandle();
      const b = makeHandle();
      registerComposerPicker('pane-main', 'model', a.handle);
      registerComposerPicker('pane-main', 'effort', b.handle);
      b.setOpen(true);
      expect(isAnyComposerPickerOpen()).toBe(true);
    });
  });
});

// Where a picker's menu hangs. The toolbar's minimal rung hides the
// picker triggers behind one roll-up button, so a picker opened by a chord
// or a slash command (no anchor of its own) must not anchor to a hidden
// trigger, which has no geometry: it hangs from the roll-up instead.
describe('resolvePickerAnchor', () => {
  afterEach(() => {
    resetComposerPickerRegistryForTest();
    document.body.innerHTML = '';
  });

  function rendered(hidden = false): HTMLElement {
    const el = document.createElement('button');
    if (hidden) el.style.display = 'none';
    document.body.appendChild(el);
    // happy-dom reports no client rects for a display:none element; a
    // visible one still gets a (zero-sized) rect, which is what the
    // resolver keys on.
    el.getClientRects = () => (hidden ? ([] as unknown as DOMRectList) : ([new DOMRect()] as unknown as DOMRectList));
    return el;
  }

  it('an explicit anchor wins over everything', () => {
    const explicit = rendered();
    const trigger = rendered();
    expect(resolvePickerAnchor('main', explicit, trigger)).toBe(explicit);
  });

  it('a rendered trigger anchors itself', () => {
    const trigger = rendered();
    registerComposerPickerFallbackAnchor('main', rendered());
    expect(resolvePickerAnchor('main', undefined, trigger)).toBe(trigger);
  });

  it('a hidden trigger yields to the pane\'s fallback, and to itself when there is none', () => {
    const trigger = rendered(true);
    expect(resolvePickerAnchor('main', undefined, trigger)).toBe(trigger);
    const rollup = rendered();
    const dispose = registerComposerPickerFallbackAnchor('main', rollup);
    expect(resolvePickerAnchor('main', undefined, trigger)).toBe(rollup);
    expect(resolvePickerAnchor('other', undefined, trigger)).toBe(trigger);
    dispose();
    expect(resolvePickerAnchor('main', undefined, trigger)).toBe(trigger);
  });

  it('a stale disposer does not remove a newer registration', () => {
    const first = rendered();
    const second = rendered();
    const disposeFirst = registerComposerPickerFallbackAnchor('main', first);
    registerComposerPickerFallbackAnchor('main', second);
    disposeFirst();
    expect(resolvePickerAnchor('main', undefined, rendered(true))).toBe(second);
  });
});
