import { afterEach, describe, expect, it } from 'vitest';
import {
  airspaceIntersects,
  airspaceSurfaces,
  registerAirspaceSurface,
  resetAirspaceForTest,
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
  afterEach(() => resetAirspaceForTest());

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
});
