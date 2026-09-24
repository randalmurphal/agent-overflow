import { afterEach, expect, it, vi } from 'vitest';
import { createLockSurface } from './lockSurface';

afterEach(() => document.body.replaceChildren());

it('blocks app and portal interaction, retains prior inert state, and restores on teardown', async () => {
  const app = document.createElement('div');
  const overlay = document.createElement('div');
  const existing = document.createElement('div');
  existing.inert = true;
  document.body.append(app, overlay, existing);
  const surface = createLockSurface(overlay);
  const shortcut = vi.fn();
  window.addEventListener('keydown', shortcut);
  surface.setLocked(true);
  const portal = document.createElement('div');
  document.body.append(portal);
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(app.inert).toBe(true);
  expect(portal.inert).toBe(true);
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'n', ctrlKey: true }));
  overlay.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
  expect(shortcut).not.toHaveBeenCalled();
  const tab = new KeyboardEvent('keydown', { key: 'Tab', cancelable: true });
  window.dispatchEvent(tab);
  expect(tab.defaultPrevented).toBe(false);
  expect(shortcut).not.toHaveBeenCalled();
  surface.dispose();
  expect(app.inert).toBeFalsy();
  expect(portal.inert).toBeFalsy();
  expect(existing.inert).toBe(true);
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'n', ctrlKey: true }));
  expect(shortcut).toHaveBeenCalledTimes(1);
  window.removeEventListener('keydown', shortcut);
});

it('keeps the native-menu policy for a right-click it stops on the lock screen', () => {
  const overlay = document.createElement('div');
  const label = document.createElement('p');
  const field = document.createElement('input');
  field.type = 'text';
  overlay.append(label, field);
  document.body.append(overlay);
  const surface = createLockSurface(overlay);
  surface.setLocked(true);
  const rightClick = (target: Element) => {
    const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'isTrusted', { value: true });
    target.dispatchEvent(event);
    return event;
  };
  try {
    expect(rightClick(label).defaultPrevented).toBe(true);
    expect(rightClick(field).defaultPrevented).toBe(false);
  } finally {
    surface.dispose();
  }
});
