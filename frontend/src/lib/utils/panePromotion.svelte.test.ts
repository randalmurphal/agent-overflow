import { afterEach, describe, expect, it } from 'vitest';
import { promotedLayerClip, promotedLayerFrame, setPromotedLayer } from './panePromotion.svelte';
import { airspaceIntersects, airspaceSurfaces } from './paneAirspace.svelte';

const clip = { left: 10, top: 0, right: 110, bottom: 50 };

describe('promoted layer', () => {
  afterEach(() => {
    setPromotedLayer(null, null);
  });

  it('answers its clip only for an element inside it, and only while clipped', () => {
    const layer = document.createElement('section');
    const inside = document.createElement('div');
    layer.append(inside);
    setPromotedLayer(layer, clip);
    expect(promotedLayerClip(inside)).toEqual(clip);
    expect(promotedLayerClip(document.createElement('div'))).toBeNull();
    setPromotedLayer(layer, null);
    expect(promotedLayerClip(inside)).toBeNull();
    setPromotedLayer(null, null);
    expect(promotedLayerClip(inside)).toBeNull();
  });

  it('changes its frame signal on every write and registers as a layer, not an overlay', () => {
    const layer = document.createElement('section');
    const start = promotedLayerFrame();
    setPromotedLayer(layer, clip);
    setPromotedLayer(layer, { ...clip, left: 5 });
    expect(promotedLayerFrame()).toBe(start + 2);
    expect(airspaceSurfaces()).toEqual([]);
    // Occludes by its current clip, and never what is inside it.
    const corner = { left: 0, top: 0, right: 6, bottom: 10 };
    expect(airspaceIntersects(corner)).toBe(true);
    const inside = document.createElement('div');
    layer.append(inside);
    expect(airspaceIntersects(corner, inside)).toBe(false);
    setPromotedLayer(null, null);
    expect(promotedLayerFrame()).toBe(start + 3);
    expect(airspaceIntersects(corner)).toBe(false);
  });
});
