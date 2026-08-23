import { afterEach, describe, expect, it } from 'vitest';
// The real stylesheet: the height contract under test is app.css's
// .working-sprite-slot-rail negative margin-block against the activity
// rail's rem-derived single-row height. Delete that rule and the
// invariant test below fails.
import '../../app.css';
import { mount, unmount } from 'svelte';
import WorkingSprite from '../components/composer/WorkingSprite.svelte';
import { activityRailChipClasses, activityRailRowClasses } from '../components/composer/activityRailClasses';
import { BUILTIN_SPRITES } from './catalog';

// happy-dom reports zero geometry, so the rail's height contract — the
// oversized sprite must not change the row's measured height, for every
// committed sprite — needs a real layout engine. Runs in the `browser`
// vitest project (real Chromium; see vitest.config.ts).

const mounted: Array<{ host: HTMLElement; instance?: Record<string, unknown> }> = [];

afterEach(() => {
  for (const entry of mounted.splice(0)) {
    if (entry.instance) void unmount(entry.instance as never);
    entry.host.remove();
  }
});

/** One activity-rail row with a working chip, as ActivityRail renders it. */
function mountRow(): { row: HTMLDivElement; chip: HTMLSpanElement } {
  const host = document.createElement('div');
  host.style.cssText = 'width: 480px;';
  const row = document.createElement('div');
  row.className = activityRailRowClasses;
  const chip = document.createElement('span');
  chip.className = `${activityRailChipClasses} shrink-0`;
  const label = document.createElement('span');
  label.textContent = 'Working 4m32s';
  chip.appendChild(label);
  row.appendChild(chip);
  host.appendChild(row);
  document.body.appendChild(host);
  mounted.push({ host });
  return { row, chip };
}

function decode(src: string): Promise<{ width: number; height: number }> {
  return new Promise((resolve, reject) => {
    const image = new Image();
    image.onload = () => resolve({ width: image.naturalWidth, height: image.naturalHeight });
    image.onerror = () => reject(new Error(`failed to decode ${src}`));
    image.src = src;
  });
}

/** Alpha channel of one strip, row-major, via a canvas readback. */
function alphaPlane(src: string): Promise<{ width: number; height: number; alpha: Uint8ClampedArray }> {
  return new Promise((resolve, reject) => {
    const image = new Image();
    image.onload = () => {
      const canvas = document.createElement('canvas');
      canvas.width = image.naturalWidth;
      canvas.height = image.naturalHeight;
      const ctx = canvas.getContext('2d')!;
      ctx.drawImage(image, 0, 0);
      const { data } = ctx.getImageData(0, 0, canvas.width, canvas.height);
      const alpha = new Uint8ClampedArray(canvas.width * canvas.height);
      for (let i = 0; i < alpha.length; i += 1) alpha[i] = data[i * 4 + 3]!;
      resolve({ width: canvas.width, height: canvas.height, alpha });
    };
    image.onerror = () => reject(new Error(`failed to decode ${src}`));
    image.src = src;
  });
}

// Strips whose art runs to the cell edge BY DESIGN: a looping rainbow
// trail (nyan) or a body framed flush against its cell (the parrot GIFs).
// Every other strip is a pose centred in its cell with empty columns on
// both sides, so content on BOTH sides of an internal boundary means one
// frame carries a piece of its neighbour.
const EDGE_ART_BY_DESIGN = new Set(['nyan-cat', 'nyan-parrot', 'party-parrot', 'party-parrot-classic']);

describe('committed sprite strips', () => {
  it('every catalog entry matches its PNG geometry', async () => {
    for (const sprite of BUILTIN_SPRITES) {
      const { width, height } = await decode(sprite.src);
      expect(height, sprite.id).toBe(sprite.frameHeight);
      expect(width, sprite.id).toBe(sprite.frames * sprite.frameWidth);
    }
  });

  it('no frame carries a piece of its neighbour', async () => {
    // Field bug 2026-08-22: the activity sheet these robots were cut from
    // is not a uniform grid (its last row holds seven poses), and filing
    // components by grid cell put two robots in one cell — the frame then
    // showed a sliver of the second at its edge. Debris clipped by a frame
    // edge (a paper sheet, a gear) renders as the same kind of dark bar.
    // The extractor now files by pose and drops edge-clipped debris
    // whole; this pins the result on the committed bytes.
    for (const sprite of BUILTIN_SPRITES) {
      if (EDGE_ART_BY_DESIGN.has(sprite.id)) continue;
      const { width, height, alpha } = await alphaPlane(sprite.src);
      const opaqueRows = (x: number): number => {
        let n = 0;
        for (let y = 0; y < height; y += 1) if (alpha[y * width + x]! > 40) n += 1;
        return n;
      };
      for (let frame = 0; frame + 1 < sprite.frames; frame += 1) {
        const right = opaqueRows(frame * sprite.frameWidth + sprite.frameWidth - 1);
        const left = opaqueRows((frame + 1) * sprite.frameWidth);
        expect(
          right > 0 && left > 0,
          `${sprite.id}: content on both sides of the ${frame}|${frame + 1} boundary (${right} and ${left} opaque rows)`,
        ).toBe(false);
      }
      expect(opaqueRows(width - 1), `${sprite.id}: last frame clipped at the strip edge`).toBe(0);
    }
  });
});

describe('activity rail height contract', () => {
  it('holds the row height for every built-in sprite', async () => {
    const { row: baseline } = mountRow();
    const baselineHeight = baseline.getBoundingClientRect().height;
    expect(baselineHeight).toBeGreaterThan(0);

    for (const sprite of BUILTIN_SPRITES) {
      const { row, chip } = mountRow();
      const instance = mount(WorkingSprite, {
        target: chip,
        anchor: chip.firstChild as Node,
        props: { sprite, animate: false },
      });
      mounted.at(-1)!.instance = instance as Record<string, unknown>;

      const rect = row.getBoundingClientRect();
      expect(rect.height, `${sprite.id} row height`).toBeCloseTo(baselineHeight, 1);

      const slot = row.querySelector('[data-testid="activity-rail-sprite"]')!;
      const spriteRect = slot.querySelector('.working-sprite')!.getBoundingClientRect();
      expect(spriteRect.height, `${sprite.id} sprite height`).toBeCloseTo(24, 1);
      // The sprite may overhang the chip vertically (that is the negative
      // margin trick) but must stay inside the ROW's box.
      expect(spriteRect.top, `${sprite.id} top clearance`).toBeGreaterThanOrEqual(rect.top - 0.5);
      expect(spriteRect.bottom, `${sprite.id} bottom clearance`).toBeLessThanOrEqual(
        rect.bottom + 0.5,
      );
      // And the row must never scroll horizontally.
      expect(row.scrollWidth - row.clientWidth, `${sprite.id} overflow`).toBe(0);
    }
  });

  it('settings preview (inRail=false) takes the full sprite height', () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    mounted.push({ host });
    const instance = mount(WorkingSprite, {
      target: host,
      props: { sprite: BUILTIN_SPRITES[0]!, animate: false, inRail: false },
    });
    mounted.at(-1)!.instance = instance as Record<string, unknown>;
    const slot = host.querySelector('[data-testid="activity-rail-sprite"]')!;
    expect(slot.getBoundingClientRect().height).toBeCloseTo(24, 1);
  });
});
