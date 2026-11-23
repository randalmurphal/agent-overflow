import { describe, expect, it } from 'vitest';
import {
  airspaceIntersects,
  airspaceRevision,
  airspaceSurface,
  airspaceSurfaces,
  registerAirspaceSurface,
} from './paneAirspace.svelte';

function surfaceAt(left: number, top: number, width: number, height: number): HTMLElement {
  const el = document.createElement('div');
  el.getBoundingClientRect = () =>
    ({
      left,
      top,
      right: left + width,
      bottom: top + height,
      width,
      height,
      x: left,
      y: top,
      toJSON: () => ({}),
    }) as DOMRect;
  return el;
}

const paneRect = { left: 100, top: 100, right: 500, bottom: 400 };

describe('paneAirspace', () => {

  it('registers and releases surfaces, surviving a second engagement', () => {
    const el = surfaceAt(0, 0, 50, 50);
    const release = registerAirspaceSurface(el);
    expect(airspaceSurfaces()).toContain(el);
    release();
    expect(airspaceSurfaces()).not.toContain(el);
    // Second lap: a re-register after release must not duplicate or leak.
    const again = registerAirspaceSurface(el);
    expect(airspaceSurfaces()).toEqual([el]);
    again();
    expect(airspaceSurfaces()).toEqual([]);
  });

  it('releasing one surface keeps the others registered', () => {
    const a = surfaceAt(0, 0, 10, 10);
    const b = surfaceAt(20, 20, 10, 10);
    const releaseA = registerAirspaceSurface(a);
    registerAirspaceSurface(b);
    releaseA();
    expect(airspaceSurfaces()).toEqual([b]);
  });

  it('answers intersection against live geometry, not registration order', () => {
    registerAirspaceSurface(surfaceAt(600, 600, 100, 100)); // far away
    expect(airspaceIntersects(paneRect)).toBe(false);
    registerAirspaceSurface(surfaceAt(450, 350, 200, 200)); // overlaps corner
    expect(airspaceIntersects(paneRect)).toBe(true);
  });

  it('a merely touching edge does not intersect', () => {
    registerAirspaceSurface(surfaceAt(500, 100, 100, 100)); // shares the right edge
    expect(airspaceIntersects(paneRect)).toBe(false);
  });

  it('ignores zero-size surfaces even when their point lies inside the rect', () => {
    registerAirspaceSurface(surfaceAt(200, 200, 0, 0));
    expect(airspaceIntersects(paneRect)).toBe(false);
  });

  it('a surface inside which the subject sits does not occlude it', () => {
    const overlay = surfaceAt(0, 0, 1000, 1000);
    const inside = document.createElement('div');
    overlay.append(inside);
    registerAirspaceSurface(overlay);
    expect(airspaceIntersects(paneRect, inside)).toBe(false);
    expect(airspaceIntersects(paneRect, document.createElement('div'))).toBe(true);
    expect(airspaceIntersects(paneRect)).toBe(true);
  });

  it('a surface that supplies its own rect occludes by that rect, not its box', () => {
    let painted: { left: number; top: number; right: number; bottom: number } | null = {
      left: 600, top: 100, right: 700, bottom: 200,
    };
    registerAirspaceSurface(surfaceAt(0, 0, 1000, 1000), { rect: () => painted });
    expect(airspaceIntersects(paneRect)).toBe(false);
    painted = { left: 450, top: 100, right: 700, bottom: 200 };
    expect(airspaceIntersects(paneRect)).toBe(true);
    painted = null;
    expect(airspaceIntersects(paneRect)).toBe(false);
  });

  it('a layer occludes but is not an overlay, and releases cleanly', () => {
    const layer = surfaceAt(0, 0, 1000, 1000);
    const release = registerAirspaceSurface(layer, { overlay: false });
    expect(airspaceSurfaces()).toEqual([]);
    expect(airspaceIntersects(paneRect)).toBe(true);
    release();
    release();
    expect(airspaceIntersects(paneRect)).toBe(false);
  });

  it('the action registers again when its kind changes, both ways, and only then', () => {
    const el = surfaceAt(0, 0, 1000, 1000);
    const action = airspaceSurface(el, { overlay: true });
    expect(airspaceSurfaces()).toEqual([el]);
    action.update({ overlay: false });
    expect(airspaceSurfaces()).toEqual([]);
    expect(airspaceIntersects(paneRect)).toBe(true);
    const revision = airspaceRevision();
    action.update({ overlay: false });
    expect(airspaceRevision()).toBe(revision);
    action.update({ overlay: true });
    expect(airspaceSurfaces()).toEqual([el]);
    action.destroy();
    expect(airspaceSurfaces()).toEqual([]);
    expect(airspaceIntersects(paneRect)).toBe(false);
  });

  it('releasing twice drops only its own registration of a shared element', () => {
    const el = surfaceAt(0, 0, 10, 10);
    const first = registerAirspaceSurface(el);
    registerAirspaceSurface(el);
    first();
    first();
    expect(airspaceSurfaces()).toEqual([el]);
  });
});
