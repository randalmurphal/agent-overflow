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

describe('committed sprite strips', () => {
  it('every catalog entry matches its PNG geometry', async () => {
    for (const sprite of BUILTIN_SPRITES) {
      const { width, height } = await decode(sprite.src);
      expect(height, sprite.id).toBe(sprite.frameHeight);
      expect(width, sprite.id).toBe(sprite.frames * sprite.frameWidth);
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
