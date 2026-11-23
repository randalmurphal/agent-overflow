// A layer that grows in place (a toast added to the stack) starts covering a
// native view without registering again: the one shared resize observer
// bumps the revision the view re-judges its obscurity on.
import { afterEach, expect, it, vi } from 'vitest';
import { airspaceIntersects, airspaceRevision, airspaceSurface } from './paneAirspace.svelte';

afterEach(() => {
  document.body.replaceChildren();
});

it('a layer resized in place changes what it covers, and the revision says so', async () => {
  const toast = document.createElement('div');
  toast.style.cssText = 'position:fixed;right:0;bottom:0;width:100px;height:40px';
  document.body.append(toast);
  const action = airspaceSurface(toast, { overlay: false });
  const view = { left: 0, top: 0, right: window.innerWidth - 150, bottom: window.innerHeight };
  expect(airspaceIntersects(view)).toBe(false);
  const before = airspaceRevision();

  toast.style.width = '400px';
  await vi.waitFor(() => expect(airspaceRevision()).toBeGreaterThan(before));
  expect(airspaceIntersects(view)).toBe(true);
  action.destroy();
  expect(airspaceIntersects(view)).toBe(false);
});
