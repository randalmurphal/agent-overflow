// Key ownership on the import surface. Every null below is a key the
// surface deliberately gives back to a focused control — those are the
// cases a component test cannot see failing (the native default just stops
// happening), so they are pinned here explicitly.

import { describe, expect, it } from 'vitest';
import { resolveImportKeyAction, type ImportKeyEvent } from './sessionImportKeyboard';

function el(tag: string, props: Record<string, unknown> = {}): HTMLElement {
  const node = document.createElement(tag);
  Object.assign(node, props);
  return node;
}

function press(key: string, target: EventTarget | null, mods: Partial<ImportKeyEvent> = {}) {
  return resolveImportKeyAction({ key, metaKey: false, ctrlKey: false, target, ...mods });
}

const search = () => el('input', { type: 'search', value: '' });

describe('resolveImportKeyAction', () => {
  it('moves the roving cursor from anywhere but the project select', () => {
    expect(press('ArrowDown', search())).toBe('cursor-down');
    expect(press('ArrowUp', el('div'))).toBe('cursor-up');
    expect(press('ArrowDown', el('select'))).toBeNull();
    expect(press('ArrowUp', el('select'))).toBeNull();
  });

  it('leaves Space to any control that has its own meaning for it', () => {
    expect(press(' ', el('div'))).toBe('toggle-active');
    expect(press(' ', search())).toBeNull();
    expect(press(' ', el('button'))).toBeNull();
    expect(press(' ', el('select'))).toBeNull();
  });

  it('runs the import on Enter except where Enter already means something', () => {
    expect(press('Enter', search())).toBe('run-import');
    expect(press('Enter', el('div'))).toBe('run-import');
    expect(press('Enter', el('button'))).toBeNull();
    expect(press('Enter', el('select'))).toBeNull();
  });

  it('takes mod+a only where text select-all would be a no-op', () => {
    expect(press('a', search(), { metaKey: true })).toBe('select-all');
    expect(press('A', el('div'), { ctrlKey: true })).toBe('select-all');
    expect(press('a', el('input', { type: 'search', value: 'typed' }), { metaKey: true })).toBeNull();
    // A plain "a" is typing, whatever has focus.
    expect(press('a', search())).toBeNull();
  });

  it('ignores keys the surface has no meaning for', () => {
    expect(press('Escape', el('div'))).toBeNull();
    expect(press('Tab', el('div'))).toBeNull();
    expect(press('ArrowDown', null)).toBe('cursor-down');
  });
});
