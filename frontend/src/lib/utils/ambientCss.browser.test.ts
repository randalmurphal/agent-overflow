import { afterEach, beforeEach, describe, expect, it } from 'vitest';
// The real stylesheet: these tests exist to prove app.css's ambient
// keyframes still render the waveforms the tables below specify.
import '../../app.css';
import { AMBIENT_SLOT_MS } from './ambientTicker';
import { startAmbientPhase } from './ambientPhase';
import { BUILTIN_SPRITES } from '../spinners/catalog';

// The indicator animations moved from a JS ticker into CSS so the browser
// can run them off the main thread (measured 2026-08-23: 163.0ms of
// main-thread work per 5s became 0.0ms). That trade is only safe if the
// CSS renders the SAME waveform the ticker did, so these sample the real
// rendered animation slot by slot against the tables below, which are the
// specification. Each slot is sampled at its first and its last
// millisecond, so a value that changes inside a slot fails too.
//
// happy-dom has no animation timeline and no computed transforms, so this
// needs the `browser` vitest project (real Chromium). Run with
// `pnpm test:browser`.

// `animate-pulse` opacity per 125ms slot over its 2s cycle: base → 0.5 →
// base, steps(8, jump-none) per 1s keyframe segment, so each half lands 8
// values with both endpoints. 0.5 holds across the join (slots 7 and 8)
// and base across the wrap (slots 15 and 0).
const PULSE_OPACITY = [14, 13, 12, 11, 10, 9, 8, 7, 7, 8, 9, 10, 11, 12, 13, 14].map((n) => n / 14);

// Working-LED opacities per 125ms slot over the 750ms chase: one LED lit
// (1) and two resting (0.2), left to right, 250ms each.
const LED_OPACITIES = [
  [1, 0.2, 0.2], [1, 0.2, 0.2],
  [0.2, 1, 0.2], [0.2, 1, 0.2],
  [0.2, 0.2, 1], [0.2, 0.2, 1],
];

// SteppedSpinner rotation in degrees per 125ms slot: one 30° spoke per
// slot, 12 spokes per 1.5s revolution.
const SPIN_DEGREES = [0, 30, 60, 90, 120, 150, 180, 210, 240, 270, 300, 330];

/** The first and the last millisecond of each slot of a `slots`-slot
 * cycle, with the slot's index. */
function slotSamples(slots: number): Array<readonly [number, number]> {
  return Array.from({ length: slots }, (_, slot) => [
    [slot * AMBIENT_SLOT_MS, slot] as const,
    [(slot + 1) * AMBIENT_SLOT_MS - 1, slot] as const,
  ]).flat();
}

const hosts: HTMLElement[] = [];

function mount(html: string): HTMLElement {
  const host = document.createElement('div');
  host.innerHTML = html;
  const el = host.firstElementChild as HTMLElement;
  document.body.appendChild(el);
  hosts.push(el);
  return el;
}

/** The one CSS animation on an element, failing loudly if the rule that
 * should arm it has gone missing. */
function animationOf(el: Element, expectedName: string): Animation {
  const found = el.getAnimations().find((a) => (a as CSSAnimation).animationName === expectedName);
  expect(found, `no '${expectedName}' animation on ${el.className || el.tagName}`).toBeDefined();
  return found as Animation;
}

/** Park the animation at a wall-clock instant, the way ambientPhase.ts
 * pins it, and flush style so computed values reflect it. */
function sampleAt(animation: Animation, tMs: number): void {
  const timing = animation.effect!.getComputedTiming();
  const duration = timing.duration as number;
  animation.currentTime = ((tMs % duration) + duration) % duration;
}

const opacityOf = (el: Element): number => Number(getComputedStyle(el).opacity);

/** Degrees from a computed 2D matrix. */
function rotationOf(el: Element): number {
  const m = new DOMMatrixReadOnly(getComputedStyle(el).transform);
  const deg = (Math.atan2(m.b, m.a) * 180) / Math.PI;
  return ((Math.round(deg * 1000) / 1000) + 360) % 360;
}

const translateXOf = (el: Element): number =>
  new DOMMatrixReadOnly(getComputedStyle(el).transform).e;

afterEach(() => {
  for (const el of hosts.splice(0)) el.remove();
});

describe('ambient pulse keyframes', () => {
  it('renders the pulse table across the 2s cycle', () => {
    const dot = mount('<span class="animate-pulse"></span>');
    const animation = animationOf(dot, 'ambient-pulse');
    for (const [t, slot] of slotSamples(PULSE_OPACITY.length)) {
      sampleAt(animation, t);
      expect(opacityOf(dot), `t=${t}ms`).toBeCloseTo(PULSE_OPACITY[slot]!, 4);
    }
  });

  // The staggers are negative animation-delays, so each marked dot shows
  // the waveform one or two 250ms slots AHEAD of the base — that, and
  // not a different waveform, is what keeps the three from breathing in
  // unison. A positive delay would hold the dot un-animated through its
  // first quarter second and fire `animationstart` late, which is the
  // event ambientPhase.ts pins wall-clock phase on.
  it.each([
    ['ambient-pulse-s2', 250],
    ['ambient-pulse-s4', 500],
  ])('runs %s a lead of %dms ahead of the base waveform', (stagger, leadMs) => {
    const base = mount('<span class="animate-pulse"></span>');
    const staggered = mount(`<span class="animate-pulse ${stagger}"></span>`);
    const baseAnimation = animationOf(base, 'ambient-pulse');
    const staggeredAnimation = animationOf(staggered, 'ambient-pulse');
    const leadSlots = leadMs / AMBIENT_SLOT_MS;
    for (const [t, slot] of slotSamples(PULSE_OPACITY.length)) {
      sampleAt(baseAnimation, t);
      sampleAt(staggeredAnimation, t);
      expect(opacityOf(staggered), `t=${t}ms`).toBeCloseTo(PULSE_OPACITY[(slot + leadSlots) % PULSE_OPACITY.length]!, 4);
    }
  });
});

describe('working-LED chase keyframes', () => {
  it('renders the LED table one-hot across the 750ms cycle', () => {
    const chase = mount(
      '<span class="working-leds"><i class="working-led"></i><i class="working-led"></i><i class="working-led"></i></span>',
    );
    const leds = [...chase.children];
    const animations = leds.map((led) => animationOf(led, 'ambient-led'));
    for (const [t, slot] of slotSamples(LED_OPACITIES.length)) {
      for (const a of animations) sampleAt(a, t);
      const expected = LED_OPACITIES[slot]!;
      for (let i = 0; i < 3; i += 1) {
        expect(opacityOf(leds[i]!), `LED ${i} at t=${t}ms`).toBeCloseTo(expected[i]!, 4);
      }
    }
  });

  it('rests all three LEDs at 0.45 without the wrapper class (low-power mode)', () => {
    const chase = mount(
      '<span><i class="working-led"></i><i class="working-led"></i><i class="working-led"></i></span>',
    );
    for (const led of chase.children) {
      expect(led.getAnimations()).toHaveLength(0);
      expect(opacityOf(led)).toBeCloseTo(0.45, 4);
    }
  });
});

describe('stepped spinner keyframes', () => {
  it('renders the spin table one 30deg spoke per slot', () => {
    const spinner = mount('<svg class="stepped-spin" viewBox="0 0 24 24" width="11" height="11"></svg>');
    const animation = animationOf(spinner, 'ambient-spin');
    for (const [t, slot] of slotSamples(SPIN_DEGREES.length)) {
      sampleAt(animation, t);
      expect(rotationOf(spinner), `t=${t}ms`).toBeCloseTo(SPIN_DEGREES[slot]!, 3);
    }
  });
});

describe('working sprite strip', () => {
  beforeEach(() => {
    // --dpr is what snaps the frame width to whole device pixels.
    startAmbientPhase()();
    document.documentElement.style.setProperty('--dpr', String(window.devicePixelRatio || 1));
  });

  it('lands every frame on a whole number of device pixels', () => {
    // A composited transform is applied AFTER rasterization, so a frame
    // offset between device pixels resamples the texture — measured at
    // 80.8% of pixels for a 25px frame at 125% scaling before the snap.
    const dpr = window.devicePixelRatio || 1;
    for (const sprite of BUILTIN_SPRITES) {
      const slot = mount(
        `<span class="working-sprite-slot"><span class="working-sprite" style="--working-sprite-aspect:${sprite.frameWidth / sprite.frameHeight};--working-sprite-frames:${sprite.frames}"><span class="working-sprite-strip"></span></span></span>`,
      );
      const fw = slot.firstElementChild!.getBoundingClientRect().width;
      const deviceFw = fw * dpr;
      expect(
        Math.abs(deviceFw - Math.round(deviceFw)),
        `${sprite.id}: frame width ${fw}px is ${deviceFw} device px`,
      ).toBeLessThan(0.001);
      expect(fw).toBeGreaterThan(0);
    }
  });

  it('steps the strip exactly one frame width per frame, and never past the end', () => {
    const sprite = BUILTIN_SPRITES.find((s) => s.frames > 3)!;
    const slot = mount(
      `<span class="working-sprite-slot"><span class="working-sprite" style="--working-sprite-aspect:${sprite.frameWidth / sprite.frameHeight};--working-sprite-frames:${sprite.frames}"><span class="working-sprite-strip" style="animation:working-sprite-run ${sprite.frames * sprite.frameMs}ms steps(${sprite.frames}) infinite"></span></span></span>`,
    );
    const frameWindow = slot.firstElementChild!;
    const strip = frameWindow.firstElementChild!;
    const fw = frameWindow.getBoundingClientRect().width;
    const animation = animationOf(strip, 'working-sprite-run');
    const cycle = sprite.frames * sprite.frameMs;
    for (let frame = 0; frame < sprite.frames; frame += 1) {
      // mid-frame, so a rounding error at the boundary cannot pass
      sampleAt(animation, frame * sprite.frameMs + sprite.frameMs / 2);
      expect(translateXOf(strip), `${sprite.id} frame ${frame}`).toBeCloseTo(-frame * fw, 3);
    }
    // steps() must not reveal the `to` value: the last frame is frames-1.
    sampleAt(animation, cycle - 1);
    expect(translateXOf(strip)).toBeCloseTo(-(sprite.frames - 1) * fw, 3);
  });

  it('rests on frame 0 with no animation (low-power mode)', () => {
    const sprite = BUILTIN_SPRITES[0]!;
    const slot = mount(
      `<span class="working-sprite-slot"><span class="working-sprite" style="--working-sprite-aspect:${sprite.frameWidth / sprite.frameHeight};--working-sprite-frames:${sprite.frames}"><span class="working-sprite-strip"></span></span></span>`,
    );
    const strip = slot.firstElementChild!.firstElementChild!;
    expect(strip.getAnimations()).toHaveLength(0);
    expect(translateXOf(strip)).toBe(0);
  });
});

describe('startAmbientPhase', () => {
  let stop: (() => void) | null = null;
  const cleanup: (() => void)[] = [];
  afterEach(() => {
    stop?.();
    stop = null;
    for (const undo of cleanup.splice(0)) undo();
  });

  it('puts spinners mounted at different times on one wall-clock beat', async () => {
    stop = startAmbientPhase();
    const first = mount('<svg class="stepped-spin"></svg>');
    // Long enough to be plainly out of phase without alignment.
    await new Promise((r) => setTimeout(r, 320));
    const second = mount('<svg class="stepped-spin"></svg>');

    const a = animationOf(first, 'ambient-spin');
    const b = animationOf(second, 'ambient-spin');
    // Compare how far apart the two phases ARE, not which 125ms slot each
    // one floors into. `alignOne` pins `startTime` from the FRAME's
    // `timeline.currentTime` minus the INSTANT's `Date.now()`, so two
    // alignments differ by wherever in their frames the `animationstart`
    // handlers ran — under a frame, invisible, and exactly what the module
    // promises. A slot index turns that into a coin flip whenever the pair
    // straddles a boundary, which is a boundary the animations do not
    // have: it failed about one browser run in thirty (2026-08-30). Two
    // unaligned spinners mounted 320ms apart sit 320ms out, so the signal
    // this test exists for still clears the bound by 2.5x.
    const phase = (animation: Animation): number =>
      ((((animation.currentTime as number) % 1500) + 1500) % 1500);
    const phaseDistance = (): number => {
      const apart = Math.abs(phase(a) - phase(b));
      return Math.min(apart, 1500 - apart);
    };

    // `animationstart` is delivered on the first animation frame. Under the
    // full browser suite Chromium can defer that frame past a fixed timer even
    // though no unaligned frame was painted. Wait for the event's observable
    // result with a wall-clock deadline, then retain the same phase assertion.
    const deadline = Date.now() + 2000;
    while (phaseDistance() >= AMBIENT_SLOT_MS && Date.now() < deadline) {
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    }
    expect(
      phaseDistance(),
      'later spinner is on a different beat from the first',
    ).toBeLessThan(AMBIENT_SLOT_MS);
  });

  it('aligns hundreds of indicators mounted in one frame in one pass', async () => {
    stop = startAmbientPhase();
    const reference = animationOf(mount('<svg class="stepped-spin"></svg>'), 'ambient-spin');
    await new Promise((r) => setTimeout(r, 320));
    const host = mount('<div></div>');
    // Small enough that every one is on screen, as tray rows are; Chromium
    // defers animation events for off-screen elements.
    host.innerHTML = '<svg class="stepped-spin" width="2" height="2"></svg>'.repeat(400);
    const animations = [...host.children].map((el) => animationOf(el, 'ambient-spin'));
    // Phase distance from the reference, as in the test above: unaligned,
    // the batch sits about 320ms off it.
    const phase = (animation: Animation): number =>
      ((((animation.currentTime as number) % 1500) + 1500) % 1500);
    const offBeat = (animation: Animation): boolean => {
      const apart = Math.abs(phase(animation) - phase(reference));
      return Math.min(apart, 1500 - apart) >= AMBIENT_SLOT_MS;
    };
    const started = performance.now();
    const deadline = started + 5000;
    while (animations.some(offBeat) && performance.now() < deadline) {
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    }
    const elapsed = performance.now() - started;
    expect(animations.filter(offBeat).length, 'spinners off the reference beat').toBe(0);
    // Aligning each indicator on its own event was quadratic: seconds at 400.
    expect(elapsed, 'aligning 400 indicators').toBeLessThan(2500);
  });

  // A long period makes the two outcomes unmistakable: an untouched
  // animation starts NOW, an aligned one is rewound by Date.now() % 60000
  // — on average half a minute earlier.
  const FOREIGN_MS = 60000;
  const untouched = (animation: Animation, startedAt: number): void => {
    expect(
      animation.startTime as number,
      'an animation the aligner does not own was rewound to wall clock',
    ).toBeGreaterThan(startedAt - 250);
  };

  it('leaves a foreign CSS animation alone', async () => {
    const style = document.createElement('style');
    style.textContent = '@keyframes not-ours { to { opacity: 0; } }';
    document.head.append(style);
    cleanup.push(() => style.remove());
    stop = startAmbientPhase();

    const startedAt = document.timeline.currentTime as number;
    const el = mount(`<span style="animation: not-ours ${FOREIGN_MS}ms linear infinite"></span>`);
    await new Promise((r) => setTimeout(r, 60));
    const animation = el.getAnimations()[0]!;
    expect((animation as CSSAnimation).animationName).toBe('not-ours');
    untouched(animation, startedAt);
  });

  it('leaves a script-driven animation alone', async () => {
    // svelte's FLIP/transitions drive sidebar rows this way. A script animation
    // has no `animationName` at all, so a name check that only rejects
    // KNOWN-foreign strings would rewind it mid-flight.
    const el = mount('<span></span>');
    const startedAt = document.timeline.currentTime as number;
    const animation = el.animate([{ opacity: 1 }, { opacity: 0 }], {
      duration: FOREIGN_MS,
      iterations: Infinity,
    });
    cleanup.push(() => animation.cancel());
    await new Promise((r) => setTimeout(r, 20));

    // Installed AFTER it starts: the install sweep is the path that sees
    // animations no `animationstart` of ours ever fired for.
    stop = startAmbientPhase();
    await new Promise((r) => setTimeout(r, 40));
    untouched(animation, startedAt);
  });

  it('publishes the device pixel ratio for the sprite frame snap', () => {
    stop = startAmbientPhase();
    expect(getComputedStyle(document.documentElement).getPropertyValue('--dpr').trim()).toBe(
      String(window.devicePixelRatio || 1),
    );
  });
});
