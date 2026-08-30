// Structural enforcement for the timeline's "runs no CSS transitions" row
// contract (AGENTS.md, incident 2026-08-17): the transition kill rule in
// app.css can only reach CSS transitions. Svelte's `transition:` / `in:` /
// `out:` / `animate:` directives animate through WAAPI and inline styles, so
// a directive on a component mounted inside the scroller would reintroduce
// exactly the animation-priority present the kill rule exists to prevent —
// and pass the timelineTransitionSuppression guard while doing it.
//
// Same allowlist mechanic as lib/architecture.test.ts, and it is SHRINK-ONLY:
// a new directive user fails, and so does an allowlist entry that no longer
// uses one. An entry earns its place by mounting OUTSIDE
// `[data-testid="message-timeline-scroll"]` — verify that before adding one,
// and state where it mounts.
//
// Scope caveat: the walk covers this DIRECTORY, but the hazard is the
// scroller's DOM SUBTREE. Components rendered into rows from outside it —
// `lib/markdown/render/` and `primitives/` — carry directives this test
// cannot see; a new
// external dependency rendered inside rows needs the same check by hand.

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const CHAT_DIR = dirname(fileURLToPath(import.meta.url));

// Components under components/chat/ that legitimately use a Svelte animation
// directive because they mount outside the timeline scroller.
const OUTSIDE_SCROLLER_ALLOWLIST: Record<string, string> = {
  'ScrollToBottomButton.svelte':
    'mounts after the scroller div closes in MessageTimeline — an overlay over the surface, never inside it',
  'ProviderStatusBanner.svelte':
    'mounts in ChatView above the timeline, outside the scroller entirely',
};

// A directive is `name:expr` glued together on an element tag — whitespace
// before, an identifier immediately after the colon. Prose ("No transition:
// the…") and TS annotations (`out: string`) put a space after the colon and
// never match.
const DIRECTIVE = /[\s<](transition|in|out|animate):[a-zA-Z_$][\w$]*/g;

function* walkSvelte(dir: string): Generator<string> {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      yield* walkSvelte(full);
      continue;
    }
    if (entry.name.endsWith('.svelte')) yield full;
  }
}

function directiveUsers(): Map<string, string[]> {
  const users = new Map<string, string[]>();
  let scanned = 0;
  for (const file of walkSvelte(CHAT_DIR)) {
    scanned += 1;
    const text = readFileSync(file, 'utf8')
      // HTML comments can quote a directive while documenting its absence.
      .replace(/<!--[\s\S]*?-->/g, '')
      // <style> blocks can legitimately write `transition:opacity …` — CSS,
      // not a directive, and the app.css kill rule already governs it.
      .replace(/<style[\s\S]*?<\/style>/g, '');
    const hits = [...text.matchAll(DIRECTIVE)].map((m) => m[0].trim());
    // Forward slashes regardless of platform, so allowlist keys for a future
    // nested file are written one way.
    if (hits.length > 0) users.set(relative(CHAT_DIR, file).split(sep).join('/'), hits);
  }
  if (scanned === 0) throw new Error('no .svelte files found; the rule would pass vacuously');
  return users;
}

describe('timeline animation directives', () => {
  it('no chat component inside the scroller uses a Svelte animation directive', () => {
    const offenders = [...directiveUsers()]
      .filter(([file]) => !(file in OUTSIDE_SCROLLER_ALLOWLIST))
      .map(([file, hits]) => `${file}: ${hits.join(', ')}`);

    expect(
      offenders,
      'Svelte transition/in/out/animate directives animate via WAAPI, which the ' +
        'app.css timeline transition kill rule cannot reach. Inside the scroller ' +
        'they license the compositor to present before re-raster finishes ' +
        '(the 2026-08-17 expand flicker). Remove the directive, or — only for a ' +
        'component verified to mount outside the scroller — add it to the ' +
        'allowlist with where it mounts.',
    ).toEqual([]);
  });

  it('the allowlist stays shrink-only', () => {
    const users = directiveUsers();
    const stale = Object.keys(OUTSIDE_SCROLLER_ALLOWLIST).filter(
      (file) => !users.has(file),
    );

    expect(
      stale,
      'allowlisted files that no longer use an animation directive must be ' +
        'removed, or the list stops describing the tree and starts pre-approving',
    ).toEqual([]);
  });
});
