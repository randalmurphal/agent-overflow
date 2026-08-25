import { afterEach, beforeEach, describe, expect, it } from 'vitest';
// The real stylesheet: these tests exist to prove app.css's ambient
// keyframes still render the waveforms ambientTicker.ts specifies.
import '../../app.css';
import {
  AMBIENT_SLOT_MS,
  ledOpacitiesAt,
  pulseOpacityAt,
  spinAngleAt,
} from './ambientTicker';
import { startAmbientPhase } from './ambientPhase';
import { BUILTIN_SPRITES } from '../spinners/catalog';

// The indicator animations moved from a JS ticker into CSS so the browser
// can run them off the main thread (measured 2026-08-23: 163.0ms of
// main-thread work per 5s became 0.0ms). That trade is only safe if the
// CSS renders the SAME waveform the ticker did, so these sample the real
// rendered animation slot by slot against the pure functions that remain
// the specification. Drift in either direction fails.
//
// happy-dom has no animation timeline and no computed transforms, so this
// needs the `browser` vitest project (real Chromium). Run with
// `pnpm test:browser`.

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

describe('ambient pulse root vars', () => {
  // The pulse is NOT a CSS animation: a running opacity animation
  // promotes its element to its own composited layer, and one dot per
  // busy thread put 18 layers on 6px dots (2026-08-25; the @theme
  // `--animate-pulse` comment carries the history). The utility reads
  // root-level custom properties the ticker writes; these tests prove
  // the CSS wiring renders the ticker's waveform and that no Animation
  // object exists to trigger the promotion.
  const setPulseVars = (t: number): void => {
    const root = document.documentElement.style;
    root.setProperty('--ambient-pulse-o', String(pulseOpacityAt(t)));
    root.setProperty('--ambient-pulse-o2', String(pulseOpacityAt(t + 250)));
    root.setProperty('--ambient-pulse-o4', String(pulseOpacityAt(t + 500)));
  };
  afterEach(() => {
    const root = document.documentElement.style;
    for (const name of ['--ambient-pulse-o', '--ambient-pulse-o2', '--ambient-pulse-o4']) {
      root.removeProperty(name);
    }
  });

  it('renders the base var across the 2s cycle, with no Animation object', () => {
    const dot = mount('<span class="animate-pulse"></span>');
    expect(dot.getAnimations(), 'pulse must not create an Animation (layer promotion)').toHaveLength(0);
    for (let t = 0; t < 2000; t += AMBIENT_SLOT_MS) {
      setPulseVars(t);
      expect(opacityOf(dot), `t=${t}ms`).toBeCloseTo(pulseOpacityAt(t), 4);
    }
  });

  it('rests at full opacity when the ticker has written nothing', () => {
    const dot = mount('<span class="animate-pulse"></span>');
    expect(opacityOf(dot)).toBe(1);
  });

  it('staggers the backgrounded dots one and two slots apart', () => {
    // Indicator.svelte's three-dot `backgrounded` variant: each stagger
    // class reads its own root property, written one and two 250ms slots
    // AHEAD of the base waveform.
    const base = mount('<span class="animate-pulse"></span>');
    const s2 = mount('<span class="animate-pulse ambient-pulse-s2"></span>');
    const s4 = mount('<span class="animate-pulse ambient-pulse-s4"></span>');
    const dots: [HTMLElement, number][] = [
      [base, 0],
      [s2, 250],
      [s4, 500],
    ];

    for (let t = 0; t < 2000; t += AMBIENT_SLOT_MS) {
      setPulseVars(t);
      for (const [el, lead] of dots) {
        expect(opacityOf(el), `lead ${lead}ms at t=${t}ms`).toBeCloseTo(pulseOpacityAt(t + lead), 4);
      }
      // The whole point of the stagger, and it holds at every slot: the
      // waveform is symmetric about slot 7.5, so two dots an even number
      // of slots apart can never land on the same value.
      const values = new Set(dots.map(([el]) => opacityOf(el).toFixed(4)));
      expect(values.size, `dots share an opacity at t=${t}ms`).toBe(3);
    }
  });
});

describe('working-LED chase keyframes', () => {
  it('renders ledOpacitiesAt() one-hot across the 750ms cycle', () => {
    const chase = mount(
      '<span class="working-leds"><i class="working-led"></i><i class="working-led"></i><i class="working-led"></i></span>',
    );
    const leds = [...chase.children];
    const animations = leds.map((led) => animationOf(led, 'ambient-led'));
    for (let t = 0; t < 750; t += AMBIENT_SLOT_MS) {
      for (const a of animations) sampleAt(a, t);
      const expected = ledOpacitiesAt(t);
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
  it('renders spinAngleAt() one 30deg spoke per slot', () => {
    const spinner = mount('<svg class="stepped-spin" viewBox="0 0 24 24" width="11" height="11"></svg>');
    const animation = animationOf(spinner, 'ambient-spin');
    for (let t = 0; t < 1500; t += AMBIENT_SLOT_MS) {
      sampleAt(animation, t);
      expect(rotationOf(spinner), `t=${t}ms`).toBeCloseTo(spinAngleAt(t) % 360, 3);
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
    // Long enough to land in a different 125ms slot without alignment.
    await new Promise((r) => setTimeout(r, 320));
    const second = mount('<svg class="stepped-spin"></svg>');
    await new Promise((r) => setTimeout(r, 30));

    const a = animationOf(first, 'ambient-spin');
    const b = animationOf(second, 'ambient-spin');
    const slot = (animation: Animation): number =>
      Math.floor(((((animation.currentTime as number) % 1500) + 1500) % 1500) / AMBIENT_SLOT_MS);
    expect(slot(b), 'later spinner is on a different beat from the first').toBe(slot(a));
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
