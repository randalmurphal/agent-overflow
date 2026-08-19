// Pins the first-paint boot script in `index.html` against the module that
// writes what it reads.
//
// The two sides can only agree by convention: the script runs before any
// bundle exists, so it cannot import a constant, and every disagreement is
// SILENT — the app looks correct from the first `applyTheme` onward, and only
// the frame nobody is looking at when they run the tests is wrong. A grep is
// the cheapest thing that fails when someone renames one side.

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { THEME_BOOT_MAX_CSS, THEME_BOOT_STORAGE_KEY, USER_THEME_STYLE_ID } from './themeApply.svelte';
import { serializeThemeCss, type ResolvedDeclaration } from './themeResolve';

const html = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), '../../../index.html'),
  'utf8',
);

/**
 * The boot script's CSS validator, lifted out of the HTML and executed.
 *
 * Grepping for the guard would prove it is spelled somewhere; this proves it
 * ACCEPTS what the serializer writes and REFUSES everything else. Both halves
 * matter: a validator that rejects real output silently costs every user their
 * first-paint palette, and one that accepts junk is the injection primitive it
 * was added to close.
 */
function bootCssValidator(): (css: string) => boolean {
  const start = html.indexOf('/* boot-css-validator:start */');
  const end = html.indexOf('/* boot-css-validator:end */');
  expect(start).toBeGreaterThan(-1);
  expect(end).toBeGreaterThan(start);
  const source = html.slice(start, end);
  // eslint-disable-next-line @typescript-eslint/no-implied-eval
  return new Function(`${source}\nreturn okCss;`)() as (css: string) => boolean;
}

function declaration(
  key: string,
  value: string,
  variant: 'dark' | 'light' = 'dark',
): ResolvedDeclaration {
  return { key, cssVar: `--${key}`, value, section: 'colors', variant, themeId: 't' };
}

describe('index.html boot stamp', () => {
  it('reads the key the applier writes', () => {
    expect(html).toContain(`localStorage.getItem('${THEME_BOOT_STORAGE_KEY}')`);
  });

  it('fills the same style element the applier rewrites', () => {
    expect(html).toContain(`<style id="${USER_THEME_STYLE_ID}"></style>`);
    expect(html).toContain(`document.getElementById('${USER_THEME_STYLE_ID}')`);
  });

  it('declares that element in the body, after nothing that could outrank it', () => {
    // Vite appends the app's stylesheet links to the end of <head>, so a
    // user-theme style in the head loses the source-order tie to app.css.
    const headEnd = html.indexOf('</head>');
    const styleAt = html.indexOf(`<style id="${USER_THEME_STYLE_ID}">`);
    expect(styleAt).toBeGreaterThan(headEnd);
  });

  it('applies the same CSS size cap the writer applies', () => {
    expect(html).toContain(`stamp.s.length <= ${THEME_BOOT_MAX_CSS}`);
  });

  it('reads the three fields the writer emits, and validates the ground', () => {
    expect(html).toContain('stamp.c');
    expect(html).toContain('stamp.b');
    expect(html).toContain('stamp.s');
    // An origin store is user-writable; the inline style would take an
    // arbitrary string otherwise.
    expect(html).toContain('/^#[0-9a-fA-F]{6}$/.test(stamp.b)');
  });

  it('writes the cached CSS only after validating it', () => {
    // The size cap alone was the entire gate; localStorage is same-origin
    // writable, so any script execution became a persistent CSS-injection
    // primitive that outlives the page that planted it.
    const write = html.indexOf(`document.getElementById('${USER_THEME_STYLE_ID}').textContent`);
    expect(write).toBeGreaterThan(-1);
    const guard = html.lastIndexOf('okCss(stamp.s)', write);
    expect(guard).toBeGreaterThan(-1);
  });

  it('cannot throw the boot script', () => {
    // Everything the script DOES is optional decoration; a parse failure of a
    // hand-edited stamp must not stop `main.ts` from loading. The validator is
    // a function declaration above the try and cannot run on its own.
    const scriptStart = html.indexOf('(function () {');
    expect(scriptStart).toBeGreaterThan(-1);
    const tryAt = html.indexOf('try {', scriptStart);
    const readAt = html.indexOf(`localStorage.getItem('${THEME_BOOT_STORAGE_KEY}')`, scriptStart);
    expect(tryAt).toBeGreaterThan(scriptStart);
    expect(readAt).toBeGreaterThan(tryAt);
    expect(html.indexOf('} catch (err)', tryAt)).toBeGreaterThan(readAt);
  });
});

describe('boot stamp CSS validator', () => {
  const okCss = bootCssValidator();

  it('accepts exactly what the serializer writes, both blocks', () => {
    const css = serializeThemeCss({
      root: [
        declaration('surface-1', '#111111'),
        declaration('accent', 'oklch(0.7 0.15 250)'),
        declaration('accent-fg', 'rgb(255, 255, 255)'),
        declaration('ansi-fg-31', 'color-mix(in oklab, #f00 50%, #00f)'),
      ],
      light: [declaration('surface-1', '#eeeeee', 'light')],
    });
    expect(css).toContain(':root {');
    expect(css).toContain('html.light {');
    expect(okCss(css)).toBe(true);
  });

  it('accepts a single-block stylesheet', () => {
    expect(okCss(serializeThemeCss({ root: [declaration('surface-0', '#090807')], light: [] }))).toBe(
      true,
    );
  });

  it('refuses anything that is not that grammar', () => {
    // Every one of these is reachable by hand-editing the origin store, and
    // every one of them is a rule the serializer could never have produced.
    expect(okCss('')).toBe(false);
    expect(okCss('body { background: url(http://evil/x) }')).toBe(false);
    expect(okCss(':root {\n  --a: red;\n}\n* { display: none }')).toBe(false);
    expect(okCss(':root {\n  --a: red;\n')).toBe(false);
    expect(okCss(':root {\n  --a: red;\n}\n@import url(http://evil/x);')).toBe(false);
    // A value that can end the declaration, open a block, or close the element.
    expect(okCss(':root {\n  --a: red; } body { color: red;\n}')).toBe(false);
    expect(okCss(':root {\n  --a: </style><script>x()</script>;\n}')).toBe(false);
    expect(okCss(':root {\n  --a: "x";\n}')).toBe(false);
    // An unclosed function consumes to EOF in CSS's tokenizer.
    expect(okCss(':root {\n  --a: rgb(1, 2, 3;\n}')).toBe(false);
    // A property name that is not a custom property, and a bad token shape.
    expect(okCss(':root {\n  color: red;\n}')).toBe(false);
    expect(okCss(':root {\n  --A: red;\n}')).toBe(false);
    // Indentation is part of the grammar the serializer emits.
    expect(okCss(':root {\n--a: red;\n}')).toBe(false);
    expect(okCss('html.dark {\n  --a: red;\n}')).toBe(false);
  });

  it('refuses the fetch-capable functions the serializer refuses, inside the grammar', () => {
    // These fit the line grammar perfectly — the danger is the VALUE. The
    // boot CSS paints before any app code runs and with no CSP, and app.css
    // paints `background: var(--surface-0)` (a shorthand, which accepts an
    // image), so a url() in a hostile localStorage stamp is a network beacon
    // on the first frame of every launch. The validator mirrors
    // themeResolve's REFUSED_FUNCTIONS; schemeless URLs keep the charset
    // happy (no colon needed: `//host/x` fetches).
    expect(okCss(':root {\n  --surface-0: url(//evil.test/x);\n}')).toBe(false);
    expect(okCss(':root {\n  --surface-0: URL (//evil.test/x);\n}')).toBe(false);
    expect(okCss(':root {\n  --surface-0: image-set(//evil.test/x 1x);\n}')).toBe(false);
    expect(okCss(':root {\n  --surface-0: src(//evil.test/x);\n}')).toBe(false);
    expect(okCss(':root {\n  --surface-0: var(--other);\n}')).toBe(false);
    expect(okCss(':root {\n  --surface-0: attr(data-x);\n}')).toBe(false);
    expect(okCss(':root {\n  --surface-0: env(safe-area-inset-top);\n}')).toBe(false);
  });
});
