// The SPA shell's half of the shipped Content-Security-Policy.
//
// `internal/transport.CSPProduction` says `script-src 'self'` with no
// 'unsafe-inline', no hash and no nonce, and that is only true while the
// document this build serves has no inline script in it. A reintroduced
// inline block does not fail the build, does not fail type-checking, and does
// not fail a render test: the browser simply refuses to run it and the page
// loses whatever it was doing — which, for the one inline script this document
// used to carry, would be an unstyled first frame nobody notices in review.
//
// Inline STYLE is checked here too, for the opposite reason. style-src does
// carry 'unsafe-inline' (Svelte, KaTeX and mermaid write style attributes
// constantly, and attribute-level style cannot be nonced), so an inline style
// block would keep working — the point is that the empty `<style
// id="user-theme">` element is the only one, and a second one with CONTENT
// would be a first-paint palette nobody pinned against the serializer.
//
// Residual, stated rather than papered over: this reads the SOURCE document.
// A bundler that started inlining something during `vite build` would not trip
// it. The live check for that is the e2e suite, which loads the real built
// bundle through the real server and fails on any CSP violation the page
// reports.

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const html = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), '../index.html'),
  'utf8',
);

/** Every `<script …>` open tag in the document, with its attributes. */
function scriptTags(): string[] {
  return [...html.matchAll(/<script\b([^>]*)>/gi)].map((match) => match[1]);
}

describe('index.html carries no inline script', () => {
  it('gives every script tag a src', () => {
    const withoutSrc = scriptTags().filter((attrs) => !/\bsrc\s*=/i.test(attrs));
    expect(withoutSrc).toEqual([]);
  });

  it('leaves every script element empty', () => {
    // A `<script src=…>` is allowed to have a body, and that body would be
    // inline script the CSP refuses just the same.
    const bodies = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi)].map((m) =>
      m[1].trim(),
    );
    expect(bodies).toEqual(bodies.map(() => ''));
  });

  it('leaves every style element empty', () => {
    const bodies = [...html.matchAll(/<style\b[^>]*>([\s\S]*?)<\/style>/gi)].map((m) => m[1].trim());
    expect(bodies).toEqual(bodies.map(() => ''));
  });

  it('carries no inline event handler attribute', () => {
    // `onclick="…"` is script under script-src, and 'unsafe-inline' is the
    // only thing that would admit it — 'unsafe-hashes' aside, which this
    // policy does not carry either.
    expect(html).not.toMatch(/\son[a-z]+\s*=\s*["']/i);
  });
});
