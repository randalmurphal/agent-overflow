// Structural enforcement for two rules that CSS keyframe animations can
// break quietly, reached either through a class app.css or Tailwind arms
// or through an `animation:` shorthand written straight into a component.
// Neither the app.css transition kill rule nor the Svelte directive walk
// (`timelineAnimationDirectives.test.ts`) can see that door.
//
// RULE 1 — a standing animation is stepped. A smoothly interpolated
// animation that repeats forever pins GPU frame production to panel
// refresh for as long as it is on screen: on a 165Hz panel one 6px
// smoothly pulsing dot was a standing 165 presents/sec client that
// stuttered OTHER APPLICATIONS (2026-07-04; re-measured 2026-08-23 at
// 63/s smooth vs 7.3/s stepped). Stepping the waveform fixes it and
// costs nothing visually — the ambient indicators all read as the same
// motion. This is checked over app.css itself, exhaustively, because the
// hazard is document-wide and has nothing to do with the timeline.
// Tailwind's own `animate-spin` is out of that scope on purpose: app.css
// does not declare it, and it is legitimate for TRANSIENT spinners,
// which this file cannot tell apart from standing ones. A standing
// spinner uses `primitives/SteppedSpinner.svelte` by convention.
//
// RULE 2 — inside the timeline scroller, an animation may move light,
// never geometry. The transcript renders like print: the scroll glide
// and `TailClampedText`'s line-slide are the only two motion owners
// (docs/architecture/frontend-scroll.md § The Print Doctrine). An
// animation on a row that changes transform, size or position is a third
// owner, it fights the scroll controller's compensated moves, and it
// invalidates fresh tiles inside the very commit those moves are already
// racing. `opacity` is the one property that does none of that, so it is
// the one property allowed.
//
// What this file does NOT claim any more, corrected 2026-08-24 against
// measurement (`scripts/perfprobe/present-policy-arms.mjs`): that the
// mere EXISTENCE of an Animation object in the scroller is the hazard.
// An active animation does drive a begin-frame every vsync, which lets
// the compositor draw before raster lands — but that exposure is BINARY
// (one animation and thirty measure the same) and DOCUMENT-wide (an
// animation outside the scroller scores the same as one inside). The
// working sprite, the LED chase and the line-slide hold it open through
// every working turn, so counting animation objects in this directory
// bought nothing. It cost a real regression in the other direction:
// `1633dcea` disarmed `--animate-pulse` on that theory and put ~28
// whole-document repaints/sec back on the main thread.
//
// The armed-class set is DERIVED from app.css, so a new animation
// utility is caught without anyone remembering to update this file.
//
// The allowlist is keyed by FILE AND ANIMATION, never by file alone: a
// file exempted wholesale would silently cover the next animation added
// to it. It is SHRINK-ONLY in both directions — a new animation fails,
// and so does an entry that no longer applies.
//
// Scope caveat, identical to the directive guard: the walk covers this
// DIRECTORY, but the hazard is the scroller's DOM SUBTREE. Components
// rendered into rows from elsewhere — `primitives/Button`'s
// `animate-spin` loading ring on load-older/newer, the vendored
// streamdown popovers — carry animations this test cannot see. Both are
// recorded as known exceptions in the Print Doctrine; a new external
// dependency rendered inside rows needs the same check by hand.

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const CHAT_DIR = dirname(fileURLToPath(import.meta.url));
const APP_CSS = resolve(CHAT_DIR, '../../../app.css');

/** Tailwind ships these `animate-*` utilities without an app.css
 * declaration. Each is armed unless @theme redefines or disarms it, and
 * its keyframes are Tailwind's, so this file cannot read what they
 * animate — they resolve as UNKNOWN and fail closed into the allowlist. */
const TAILWIND_ANIMATE_UTILITIES = ['spin', 'ping', 'bounce', 'pulse'];

/** The one property an animation inside the scroller may touch. */
const PAINT_ONLY = 'opacity';

const OUTSIDE_SCROLLER_ALLOWLIST: Record<string, Record<string, string>> = {
  'ThreadTitleRegenerateButton.svelte': {
    'animate-spin':
      'the regenerate affordance mounts in ChatHeader, a sibling of MessageTimeline in ChatView, outside the scroller; Tailwind owns the keyframes (transform), and it is transient — visible for the seconds a title takes to regenerate, never standing',
  },
  'MessageTimeline.svelte': {
    'animation: nav-jump-flash-fade':
      "the explicit-jump landing flash is an overlay on the NON-SCROLLING wrapper, a sibling after the scroller in source order, placed there deliberately so no row gains an animation; its keyframes are local to the component (opacity only, one shot, fill-mode forwards) and a jump is an instant teleport, not a compensated move",
  },
};

function readCss(): string {
  return readFileSync(APP_CSS, 'utf8');
}

/** Every `@keyframes` block in app.css, mapped to the properties it
 * animates. Brace-balanced rather than regex-matched: each keyframe
 * selector inside the block opens a nested body. */
function keyframeProperties(css: string): Map<string, Set<string>> {
  const byName = new Map<string, Set<string>>();
  const opener = /@keyframes\s+([A-Za-z0-9_-]+)\s*\{/g;

  for (let match = opener.exec(css); match !== null; match = opener.exec(css)) {
    let depth = 1;
    let end = opener.lastIndex;
    while (end < css.length && depth > 0) {
      if (css[end] === '{') depth += 1;
      else if (css[end] === '}') depth -= 1;
      end += 1;
    }
    const properties = new Set<string>();
    for (const chunk of css.slice(opener.lastIndex, end - 1).split(/[;{}]/)) {
      const declaration = /^\s*([a-z-]+)\s*:/.exec(chunk);
      if (declaration) properties.add(declaration[1]!);
    }
    byName.set(match[1]!, properties);
  }

  if (byName.size === 0) throw new Error('no @keyframes parsed from app.css; the rules would pass vacuously');
  return byName;
}

/**
 * Class names app.css or Tailwind arms with a running keyframe
 * animation, mapped to the shorthand that arms them. `null` means armed
 * but declared somewhere this file cannot read (a Tailwind builtin).
 */
function armedClasses(css: string): Map<string, string | null> {
  const armed = new Map<string, string | null>();

  // Tailwind utilities, minus any the @theme block disarms.
  const disarmed = new Set<string>();
  for (const [, name, value] of css.matchAll(/--animate-([a-z0-9-]+)\s*:\s*([^;]+);/g)) {
    if (value.trim().startsWith('none')) disarmed.add(`animate-${name}`);
    else armed.set(`animate-${name}`, value.trim());
  }
  for (const name of TAILWIND_ANIMATE_UTILITIES) {
    const cls = `animate-${name}`;
    if (!disarmed.has(cls) && !armed.has(cls)) armed.set(cls, null);
  }

  // Authored rules that run an animation, e.g. `.stepped-spin { animation: … }`.
  for (const [, selector, body] of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const declaration = /(?<![-\w])animation(?:-name)?\s*:\s*([^;]+)/.exec(body);
    if (!declaration || declaration[1]!.trim().startsWith('none')) continue;
    for (const [, cls] of selector.matchAll(/\.([a-zA-Z0-9_-]+)/g)) armed.set(cls, declaration[1]!.trim());
  }

  if (armed.size === 0) throw new Error('no armed animation classes parsed; the rules would pass vacuously');
  return armed;
}

const STEPPED = /steps\s*\(|step-start|step-end/;

/** Properties a shorthand's keyframes animate, or `null` when the
 * keyframes are not in app.css and this file cannot tell. */
function animatedProperties(
  declaration: string | null,
  keyframes: Map<string, Set<string>>,
): Set<string> | null {
  if (declaration === null) return null;
  const properties = new Set<string>();
  let matched = false;
  for (const [name, props] of keyframes) {
    if (!new RegExp(`(?<![\\w-])${name}(?![\\w-])`).test(declaration)) continue;
    matched = true;
    for (const property of props) properties.add(property);
  }
  return matched ? properties : null;
}

function* walkSvelte(dir: string): Generator<string> {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) yield* walkSvelte(full);
    else if (entry.name.endsWith('.svelte')) yield full;
  }
}

/** Every chat component's animation hits: the label the allowlist keys
 * on, and the shorthand behind it. */
function animationUsers(): Map<string, Map<string, string | null>> {
  const armed = armedClasses(readCss());
  const users = new Map<string, Map<string, string | null>>();
  let scanned = 0;

  for (const file of walkSvelte(CHAT_DIR)) {
    scanned += 1;
    // HTML comments can quote a class while documenting why it is absent.
    const text = readFileSync(file, 'utf8').replace(/<!--[\s\S]*?-->/g, '');
    const hits = new Map<string, string | null>();

    for (const [cls, declaration] of armed) {
      if (new RegExp(`(?<![\\w-])${cls}(?![\\w-])`).test(text)) hits.set(cls, declaration);
    }
    // An `animation:` shorthand written straight into the component
    // reaches the same place without going through a class at all.
    for (const [, value] of text.matchAll(/(?<![-\w])animation(?:-name)?\s*:\s*([^;'"`\n]+)/g)) {
      const declaration = value.trim();
      if (!declaration.startsWith('none')) hits.set(`animation: ${declaration}`, declaration);
    }

    if (hits.size > 0) users.set(relative(CHAT_DIR, file).split(sep).join('/'), hits);
  }

  if (scanned === 0) throw new Error('no .svelte files found; the rule would pass vacuously');
  return users;
}

describe('standing animations are stepped', () => {
  it('app.css arms no smoothly interpolated infinite animation', () => {
    const css = readCss();
    const declarations: [string, string][] = [];
    for (const [, name, value] of css.matchAll(/--animate-([a-z0-9-]+)\s*:\s*([^;]+);/g)) {
      declarations.push([`--animate-${name}`, value.trim()]);
    }
    for (const [, selector, body] of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      const declaration = /(?<![-\w])animation(?:-name)?\s*:\s*([^;]+)/.exec(body);
      if (declaration) declarations.push([selector.trim(), declaration[1]!.trim()]);
    }

    expect(declarations.length, 'no animation declarations parsed; the rule would pass vacuously').toBeGreaterThan(0);

    const smooth = declarations
      .filter(([, value]) => /(?<![-\w])infinite(?![-\w])/.test(value) && !STEPPED.test(value))
      .map(([where, value]) => `${where}: ${value}`);

    expect(
      smooth,
      'An infinite animation with a smooth timing function presents a GPU frame every vsync for as ' +
        'long as it is on screen — a standing 165/s client on a 165Hz panel, which stutters other ' +
        'applications (2026-07-04). Give it a steps()/step-end timing function on the ambient 125ms ' +
        'slot grid; the ambient indicators all read as the same motion stepped.',
    ).toEqual([]);
  });
});

describe('timeline keyframe animations', () => {
  it('no chat component animates anything but opacity', () => {
    const keyframes = keyframeProperties(readCss());
    const offenders: string[] = [];

    for (const [file, hits] of animationUsers()) {
      const allowed = OUTSIDE_SCROLLER_ALLOWLIST[file] ?? {};
      for (const [hit, declaration] of hits) {
        if (hit in allowed) continue;
        const properties = animatedProperties(declaration, keyframes);
        if (properties === null) {
          offenders.push(`${file}: ${hit} (keyframes not in app.css — cannot tell what it animates)`);
          continue;
        }
        const moving = [...properties].filter((property) => property !== PAINT_ONLY);
        if (moving.length > 0) offenders.push(`${file}: ${hit} animates ${moving.join(', ')}`);
      }
    }

    expect(
      offenders,
      'Inside the timeline scroller an animation may move light, never geometry: the scroll glide and ' +
        "TailClampedText's line-slide are the only two motion owners, and anything else fights the " +
        'compensated moves the controller makes (docs/architecture/frontend-scroll.md § The Print ' +
        'Doctrine). Animate opacity, or — only for a component verified to mount outside the scroller — ' +
        'add it to the allowlist with where it mounts and what it animates.',
    ).toEqual([]);
  });

  it('the allowlist stays shrink-only', () => {
    const users = animationUsers();
    const stale: string[] = [];
    for (const [file, allowed] of Object.entries(OUTSIDE_SCROLLER_ALLOWLIST)) {
      const hits = users.get(file);
      for (const hit of Object.keys(allowed)) {
        if (hits === undefined || !hits.has(hit)) stale.push(`${file}: ${hit}`);
      }
    }

    expect(
      stale,
      'allowlist entries whose animation is gone must be removed, or the list stops describing the tree ' +
        'and starts pre-approving',
    ).toEqual([]);
  });
});
