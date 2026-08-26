import { describe, expect, it } from 'vitest';
import { mergeTheme, theme as vendorTheme } from 'svelte-streamdown';
import { MD_BLOCK_MARKER, chatMarkdownTheme } from './streamdownTheme';

// Tailwind palette scales and the raw black/white utilities the vendor base
// theme reaches for. None of these may survive the merge on any key the
// component can mount — they are hardcoded light-mode values with no dark
// counterpart.
//
// Written as a pattern rather than a list of literal classes on purpose:
// Tailwind scans comments and string literals alike, so quoting a vendor class
// here would compile that dead rule into the production bundle.
const PALETTE_CLASS =
  /(?:^|[\s:])(?:[a-z-]+-)?(?:(?:gray|slate|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3}|black|white)(?:\/\d+)?(?![\w-])/;

// The merged theme is what actually reaches the DOM: streamdown runs
// `cn()` (clsx + tailwind-merge) per sub-key, so an override only
// cancels a vendor color when it collides with it in the SAME utility
// category AND under the SAME modifier. Asserting on the merge result
// rather than on our own strings is the only assertion that can catch
// a near-miss like a bare text color failing to cancel the vendor's
// title color under its `[&>[data-alert-title]]:` modifier — or a
// `border-0` that cancels a width and leaves the color standing.
const merged = mergeTheme(
  chatMarkdownTheme,
  // Matches ChatMarkdown.svelte's `baseTheme="tailwind"`.
  'tailwind',
) as unknown as Record<string, unknown>;

/**
 * Keys we deliberately do not theme, each because the class never mounts from
 * chat markdown. This is the ONLY reason an entry belongs here: "the vendor
 * happens not to leak on it today" is not one, because the sweep below would
 * pass on it anyway.
 *
 * Entries are checked against the vendor theme, so a rename upstream fails
 * rather than silently widening the exemption.
 */
const UNREACHABLE_KEYS: Record<string, string> = {
  // The library's code chrome has no consumer: `StreamdownCodeHost` renders
  // the pre/code DOM itself from backend highlight spans, and the vendored
  // shiki `Code.svelte` is replaced wholesale. Chat code blocks are
  // zero-chrome — the host's hover-revealed copy button is the only
  // affordance. See docs/specs/theme-system.md §4.3.
  'code.header': 'replaced by StreamdownCodeHost; the vendor header never renders',
  'code.skeleton': 'replaced by StreamdownCodeHost; there is no vendor loading skeleton',
  'code.buttons': 'replaced by StreamdownCodeHost; the host owns its own button row',
  'code.language': 'replaced by StreamdownCodeHost; no language chip is rendered',
  'code.line': 'replaced by StreamdownCodeHost; lines come from backend spans',
  // Inline citations are a shadcn-token group (a design vocabulary this app
  // does not define at all, so nothing here resolves) behind a snippet
  // ChatMarkdown overrides. Nested two levels deep, unlike every other group.
  inlineCitation: 'snippet overridden in ChatMarkdown; the group names tokens this app has none of',
};

/** Every `group.key` in the vendor theme whose value is a class string. */
function vendorClassKeys(): Array<readonly [string, string]> {
  const keys: Array<readonly [string, string]> = [];
  for (const [group, entry] of Object.entries(vendorTheme as Record<string, unknown>)) {
    if (group in UNREACHABLE_KEYS) continue;
    if (typeof entry !== 'object' || entry === null) continue;
    for (const [key, value] of Object.entries(entry as Record<string, unknown>)) {
      if (typeof value !== 'string') continue;
      if (`${group}.${key}` in UNREACHABLE_KEYS) continue;
      keys.push([group, key]);
    }
  }
  return keys;
}

describe('chatMarkdownTheme', () => {
  it('keeps fenced code wrapable instead of horizontally scrollable', () => {
    const codePreBase = chatMarkdownTheme.code.pre;
    const preClasses = codePreBase.split(/\s+/);

    expect(preClasses).toContain('whitespace-pre-wrap');
    expect(preClasses).toContain('wrap-anywhere');
    expect(preClasses).toContain('overflow-x-visible');
    expect(codePreBase).not.toMatch(/\boverflow-x-auto\b/);
    expect(codePreBase).not.toMatch(/\bwhitespace-nowrap\b/);
  });

  it('keeps inline code wrapable instead of horizontally scrollable', () => {
    const codespanBase = chatMarkdownTheme.codespan.base;
    const classes = codespanBase.split(/\s+/);

    expect(classes).toContain('inline');
    expect(classes).toContain('whitespace-pre-wrap');
    expect(classes).toContain('wrap-anywhere');
    expect(codespanBase).not.toMatch(/\boverflow-x-auto\b/);
    expect(codespanBase).not.toMatch(/\bwhitespace-nowrap\b/);
    expect(codespanBase).not.toMatch(/\binline-block\b/);
    expect(codespanBase).not.toMatch(/\bmax-w-full\b/);
  });

  it('grounds fenced code on the code-block token, not the raw surface tier', () => {
    // The code-block ground travels with a code theme (it is one of the
    // values a syntax bundle owns), so it is named apart from the
    // elevation tier it currently aliases.
    expect(chatMarkdownTheme.code.base).toMatch(/\bbg-code-block\b/);
    expect(chatMarkdownTheme.code.base).not.toMatch(/\bbg-surface-1\b/);
    // Mermaid deliberately stays on the elevation tier — a diagram is
    // not code and must not move when a code theme changes.
    expect(chatMarkdownTheme.mermaid.base).toMatch(/\bbg-surface-1\b/);
  });

  describe('vendor palette leaks', () => {
    // Driven off the VENDOR theme's own keys, not a list maintained here.
    //
    // The hand-maintained list this replaced could only assert about keys
    // somebody had already thought about, which is precisely the set that
    // does not need asserting: the one real leak in this area — a table
    // border whose width was cancelled and whose COLOR was not — sat on a
    // key the list happened to name and a sub-key it happened to miss. A
    // sweep cannot have that blind spot, and it inherits new keys from a
    // vendor bump for free.
    const keys = vendorClassKeys();

    it('sweeps a vendor theme it could actually read', () => {
      expect(keys.length).toBeGreaterThan(30);
      for (const key of Object.keys(UNREACHABLE_KEYS)) {
        const [group, sub] = key.split('.');
        const entry = (vendorTheme as Record<string, Record<string, unknown>>)[group!];
        expect(entry, `${key} is allowlisted but the vendor theme has no such group`).toBeTypeOf(
          'object',
        );
        if (sub !== undefined) {
          expect(entry?.[sub], `${key} is allowlisted but the vendor theme has no such key`).toBeTypeOf(
            'string',
          );
        }
      }
    });

    for (const [group, key] of keys) {
      it(`leaves no palette class on ${group}.${key} after the merge`, () => {
        const value = (merged[group] as Record<string, string> | undefined)?.[key];
        expect(value).toBeTypeOf('string');
        expect(value).not.toMatch(PALETTE_CLASS);
      });
    }

    it('cancels the alert title color under the vendor modifier', () => {
      // The vendor writes the title color as a palette text class under an
      // `[&>[data-alert-title]]:` modifier; tailwind-merge keys on
      // (modifier, category), so only a same-modifier override collides.
      for (const variant of ['note', 'tip', 'warning', 'caution', 'important']) {
        expect(chatMarkdownTheme.alert[variant]).toMatch(
          /\[&>\[data-alert-title\]\]:text-[a-z-]+\b/,
        );
      }
    });

    it('recognises the leak it is looking for', () => {
      // The sweep asserts a negative on every key, so a pattern that matched
      // nothing would read as a clean theme forever. These are the exact
      // shapes the vendor base theme uses, assembled at runtime so no
      // complete class literal exists for Tailwind's scanner to compile.
      expect('border-gray-' + '200').toMatch(PALETTE_CLASS);
      expect('bg-' + 'white').toMatch(PALETTE_CLASS);
      expect('[&>[data-alert-title]]:text-blue-' + '600').toMatch(PALETTE_CLASS);
      expect('hover:bg-gray-' + '100/50').toMatch(PALETTE_CLASS);
      // Our own vocabulary must not read as a leak.
      expect('border-border-subtle bg-surface-1 text-fg-muted').not.toMatch(PALETTE_CLASS);
    });
  });

  describe('md-blk block marker', () => {
    // Every element that can render as a DIRECT child of the streamdown
    // root (the .md-committed / .md-volatile wrapper) carries md-blk, and
    // nothing else does. The app.css edge-margin resets key on the marker
    // (`.markdown-body > … > .md-blk:first-child`) precisely so that
    // position in the selector is never a bare structural pseudo — see the
    // MD_BLOCK_MARKER doc in streamdownTheme.ts and
    // src/lib/styleInvalidation.test.ts for the invalidation-set mechanism.
    //
    // A block-level key missing the marker keeps its own edge margin when
    // it lands first/last in a message; an inline key carrying it would
    // hand the resets an element that is never a wrapper child. Both
    // directions are asserted, over the MERGED theme (what reaches the
    // DOM), so a tailwind-merge behavior change that dropped the unknown
    // class would fail here too.
    const BLOCK_KEYS = [
      'alert.base',
      'blockquote.base',
      'code.base',
      'descriptionList.base',
      'h1.base',
      'h2.base',
      'h3.base',
      'h4.base',
      'h5.base',
      'h6.base',
      'hr.base',
      'math.block',
      'mermaid.base',
      'ol.base',
      'paragraph.base',
      'table.base',
      'ul.base',
    ];

    const markerRe = new RegExp(`(?:^|\\s)${MD_BLOCK_MARKER}(?:\\s|$)`);

    for (const key of BLOCK_KEYS) {
      it(`stamps ${key} with the marker after the merge`, () => {
        const [group, sub] = key.split('.') as [string, string];
        const value = (merged[group] as Record<string, string> | undefined)?.[sub];
        expect(value).toBeTypeOf('string');
        expect(value).toMatch(markerRe);
      });
    }

    it('keeps the marker off every non-block key', () => {
      const offenders: string[] = [];
      for (const [group, entry] of Object.entries(merged)) {
        if (typeof entry !== 'object' || entry === null) continue;
        for (const [sub, value] of Object.entries(entry as Record<string, unknown>)) {
          if (typeof value !== 'string') continue;
          if (BLOCK_KEYS.includes(`${group}.${sub}`)) continue;
          if (markerRe.test(value)) offenders.push(`${group}.${sub}`);
        }
      }
      expect(offenders).toEqual([]);
    });
  });
});
