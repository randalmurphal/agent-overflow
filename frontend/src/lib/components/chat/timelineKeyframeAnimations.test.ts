// Structural enforcement for the timeline's "no animation objects inside the
// scroller" contract (docs/architecture/frontend-scroll.md § The Print
// Doctrine, incident 2026-08-17).
//
// The sibling guard, timelineAnimationDirectives.test.ts, covers Svelte's
// `transition:`/`in:`/`out:`/`animate:` directives. This one covers the other
// door: a CSS KEYFRAME animation, reached either through a class that app.css
// or Tailwind arms, or through an `animation:` shorthand written straight into
// a component. Neither the app.css transition kill rule nor the directive
// walk can see those, and a keyframe animation flips the present policy
// exactly as a WAAPI one does.
//
// Why the policy matters: Blink presents a frame whose tiles have finished
// rastering — unless an animation is active, which switches the scheduler to
// smoothness-priority and licenses presenting with tiles missing. The
// timeline's core moves are compensated viewport-space moves (head splices,
// prune shifts, bottom-held toggles): rows that stay screen-stationary while
// every tile invalidates at once. Under the default policy that is invisible;
// with an animation active in the same commit it is a checkerboard where text
// used to be.
//
// The armed-class set is DERIVED from app.css rather than listed here, so
// re-arming a utility (`--animate-pulse: none` → a real animation) turns every
// chat component that uses it into a failure without anyone remembering to
// update this file. That regression shipped once, in 5c860a15.
//
// The allowlist is keyed by FILE AND ANIMATION, never by file alone: a file
// exempted wholesale would silently cover the next animation added to it, and
// several of these files carry more than one. It is SHRINK-ONLY in both
// directions — a new animation fails, and so does an entry that no longer
// applies. An entry earns its place by mounting OUTSIDE
// `[data-testid="message-timeline-scroll"]` — verify that and state where.
//
// Scope caveat, identical to the directive guard: the walk covers this
// DIRECTORY, but the hazard is the scroller's DOM SUBTREE. Components rendered
// into rows from elsewhere — `primitives/Button`'s `animate-spin` loading ring
// on load-older/newer, the vendored streamdown popovers — carry animations
// this test cannot see. Both are recorded as known exceptions in the Print
// Doctrine; a new external dependency rendered inside rows needs the same
// check by hand.

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const CHAT_DIR = dirname(fileURLToPath(import.meta.url));
const APP_CSS = resolve(CHAT_DIR, '../../../app.css');

/** Tailwind ships these `animate-*` utilities without an app.css
 * declaration. Each is armed unless @theme disarms it. */
const TAILWIND_ANIMATE_UTILITIES = ['spin', 'ping', 'bounce', 'pulse'];

const OUTSIDE_SCROLLER_ALLOWLIST: Record<string, Record<string, string>> = {
  'ThreadTitleRegenerateButton.svelte': {
    'animate-spin':
      'the regenerate affordance mounts in ChatHeader, a sibling of MessageTimeline in ChatView, outside the scroller',
  },
  'MessageTimeline.svelte': {
    'animation: nav-jump-flash-fade':
      'the explicit-jump landing flash is an overlay on the NON-SCROLLING wrapper, a sibling after the scroller in source order, placed there deliberately so no row gains an animation; a jump is an instant teleport, not a compensated move',
  },
};

function readCss(): string {
  return readFileSync(APP_CSS, 'utf8');
}

/** Class names app.css or Tailwind arms with a running keyframe animation. */
function armedClasses(css: string): Set<string> {
  const armed = new Set<string>();

  // Tailwind utilities, minus any the @theme block disarms.
  const disarmed = new Set<string>();
  for (const [, name, value] of css.matchAll(/--animate-([a-z0-9-]+)\s*:\s*([^;]+);/g)) {
    (value.trim().startsWith('none') ? disarmed : armed).add(`animate-${name}`);
  }
  for (const name of TAILWIND_ANIMATE_UTILITIES) {
    if (!disarmed.has(`animate-${name}`)) armed.add(`animate-${name}`);
  }

  // Authored rules that run an animation, e.g. `.stepped-spin { animation: … }`.
  for (const [, selector, body] of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const declaration = /(?<![-\w])animation(?:-name)?\s*:\s*([^;]+)/.exec(body);
    if (!declaration || declaration[1].trim().startsWith('none')) continue;
    for (const [, cls] of selector.matchAll(/\.([a-zA-Z0-9_-]+)/g)) armed.add(cls);
  }

  if (armed.size === 0) throw new Error('no armed animation classes parsed; the rule would pass vacuously');
  return armed;
}

function* walkSvelte(dir: string): Generator<string> {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) yield* walkSvelte(full);
    else if (entry.name.endsWith('.svelte')) yield full;
  }
}

function animationUsers(): Map<string, string[]> {
  const armed = [...armedClasses(readCss())];
  const users = new Map<string, string[]>();
  let scanned = 0;

  for (const file of walkSvelte(CHAT_DIR)) {
    scanned += 1;
    // HTML comments can quote a class while documenting why it is absent.
    const text = readFileSync(file, 'utf8').replace(/<!--[\s\S]*?-->/g, '');
    const hits: string[] = [];

    for (const cls of armed) {
      if (new RegExp(`(?<![\\w-])${cls}(?![\\w-])`).test(text)) hits.push(cls);
    }
    // An `animation:` shorthand written straight into the component reaches
    // the same place without going through a class at all.
    for (const [, value] of text.matchAll(/(?<![-\w])animation(?:-name)?\s*:\s*([^;'"`\n]+)/g)) {
      if (!value.trim().startsWith('none')) hits.push(`animation: ${value.trim()}`);
    }

    if (hits.length > 0) users.set(relative(CHAT_DIR, file).split(sep).join('/'), hits);
  }

  if (scanned === 0) throw new Error('no .svelte files found; the rule would pass vacuously');
  return users;
}

describe('timeline keyframe animations', () => {
  it('animate-pulse stays disarmed, because chat rows render it', () => {
    const value = /--animate-pulse\s*:\s*([^;]+);/.exec(readCss())?.[1].trim();

    expect(
      value,
      'components/chat/Indicator.svelte renders `animate-pulse` from fourteen row ' +
        'components, so arming this utility puts a live Animation object inside the ' +
        'timeline scroller on every running tool call. That flips Blink to ' +
        'smoothness-priority and licenses presenting with un-rastered tiles during ' +
        'the timeline\'s compensated moves — the 2026-08-17 checkerboard. The pulse ' +
        'is driven by utils/ambientTicker.ts inline writes, which create no ' +
        'animation object. Keep this `none`.',
    ).toBe('none');
  });

  it('no chat component inside the scroller runs a keyframe animation', () => {
    const offenders: string[] = [];
    for (const [file, hits] of animationUsers()) {
      const allowed = OUTSIDE_SCROLLER_ALLOWLIST[file] ?? {};
      for (const hit of hits) {
        if (!(hit in allowed)) offenders.push(`${file}: ${hit}`);
      }
    }

    expect(
      offenders,
      'A running CSS animation inside the timeline scroller licenses the compositor ' +
        'to present before re-raster finishes (the 2026-08-17 expand flicker). Drive ' +
        'the value with utils/ambientTicker.ts inline writes instead, or — only for a ' +
        'component verified to mount outside the scroller — add it to the allowlist ' +
        'with where it mounts.',
    ).toEqual([]);
  });

  it('the allowlist stays shrink-only', () => {
    const users = animationUsers();
    const stale: string[] = [];
    for (const [file, allowed] of Object.entries(OUTSIDE_SCROLLER_ALLOWLIST)) {
      const hits = users.get(file) ?? [];
      for (const hit of Object.keys(allowed)) {
        if (!hits.includes(hit)) stale.push(`${file}: ${hit}`);
      }
    }

    expect(
      stale,
      'allowlist entries whose animation is gone must be removed, or the list stops ' +
        'describing the tree and starts pre-approving',
    ).toEqual([]);
  });
});
