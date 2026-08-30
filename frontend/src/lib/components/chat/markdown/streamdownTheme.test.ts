import { readFileSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { describe, expect, it } from 'vitest';
import { SRC_ROOT, walkSources } from '../../../../test/sourceScan';
import { MD_BLOCK_MARKER, chatMarkdownTheme } from './streamdownTheme';

// Completeness tripwire for the ONE first-party markdown theme table.
//
// The table used to be an override layer merged over a vendor base at
// runtime (`mergeTheme`, i.e. tailwind-merge per sub-key). Both halves are
// gone: `chatMarkdownTheme` is now the whole theme, handed to `<Streamdown>`
// as-is, and a slot the render path reads but the table omits is a
// `class={undefined}` — an element that silently loses all of its styling.
//
// So the roster is derived from the CODE, not maintained here: every
// `streamdown.theme.<group>.<slot>` read across the render path (the vendored
// tree plus the app's own hosts) must exist in the table, and every entry in
// the table must have a reader. Both directions matter — the first catches a
// deleted entry, the second stops dead class strings from surviving in a
// table nobody merges any more (Tailwind compiles what it scans, so a dead
// entry is a dead rule in the bundle).

// ---------------------------------------------------------------------------
// The read set, scanned out of the render path
// ---------------------------------------------------------------------------

/** Every file that can read `streamdown.theme.*`. */
const RENDER_PATH_ROOTS = [
  // The vendored renderer (Streamdown, Block, CompactBlocks, Element,
  // Elements/*, static-html).
  resolve(SRC_ROOT, '..', 'vendor', 'svelte-streamdown', 'dist'),
  // The app's own hosts, which render code-block DOM themselves and read the
  // same table (`StreamdownCodeHost.svelte`, `staticCodeBlock.ts`).
  join(SRC_ROOT, 'lib', 'components', 'chat', 'markdown'),
];

function renderPathSources(): Array<{ path: string; text: string }> {
  const files: Array<{ path: string; text: string }> = [];
  for (const root of RENDER_PATH_ROOTS) {
    for (const file of walkSources(root, /\.(ts|js|svelte)$/)) {
      if (/\.(test|spec)\.[a-z]+$|\.manual\.ts$/.test(file)) continue;
      files.push({ path: file.split(sep).join('/'), text: readFileSync(file, 'utf8') });
    }
  }
  return files;
}

const SOURCES = renderPathSources();

/** `streamdown.theme.<group>.<slot>` — the literal reads. */
const STATIC_READ = /streamdown\.theme\.([A-Za-z0-9_]+)\.([A-Za-z0-9_]+)/g;

/**
 * The reads that index the table with a computed key. A regex cannot resolve
 * these, so each one names the slots its branch can produce and carries the
 * exact source expression as a probe: if the site is renamed or deleted, the
 * probe stops matching and this list fails rather than quietly widening.
 */
const DYNAMIC_READS: Array<{ what: string; probe: string; slots: string[] }> = [
  {
    what: "Element.svelte's heading branch",
    probe: 'streamdown.theme[`h${token.depth}`].base',
    slots: ['h1.base', 'h2.base', 'h3.base', 'h4.base', 'h5.base', 'h6.base'],
  },
  {
    what: "static-html.ts's heading branch",
    probe: 'streamdown.theme[tag as keyof typeof streamdown.theme].base',
    slots: ['h1.base', 'h2.base', 'h3.base', 'h4.base', 'h5.base', 'h6.base'],
  },
  {
    what: "static-html.ts's list branch",
    probe: 'streamdown.theme[tag].base',
    slots: ['ul.base', 'ol.base'],
  },
  {
    what: "static-html.ts's table-section, cell, inline and description branches",
    probe: 'streamdown.theme[token.type].base',
    slots: [
      'thead.base',
      'tbody.base',
      'tfoot.base',
      'td.base',
      'th.base',
      'sub.base',
      'sup.base',
      'strong.base',
      'em.base',
      'del.base',
      'descriptionTerm.base',
      'descriptionDetail.base',
    ],
  },
  {
    what: "Alert.svelte's variant class",
    probe: 'streamdown.theme.alert[token.variant]',
    slots: ['alert.note', 'alert.tip', 'alert.warning', 'alert.caution', 'alert.important'],
  },
];

function readSet(): Set<string> {
  const slots = new Set<string>();
  for (const { text } of SOURCES) {
    for (const match of text.matchAll(STATIC_READ)) {
      slots.add(`${match[1]}.${match[2]}`);
    }
  }
  for (const entry of DYNAMIC_READS) {
    for (const slot of entry.slots) slots.add(slot);
  }
  return slots;
}

const READ = readSet();

/**
 * The table as a plain string map. `Theme` names its groups explicitly (that
 * is what makes a missing slot a compile error at the table), so the sweeps
 * below — which walk it generically — need the erased shape.
 */
const TABLE = chatMarkdownTheme as unknown as Record<string, Record<string, string>>;

/** `group.slot` for every class string in the table. */
function tableSlots(): string[] {
  const slots: string[] = [];
  for (const [group, entry] of Object.entries(TABLE)) {
    for (const [slot, value] of Object.entries(entry)) {
      expect(value, `${group}.${slot} must be a class string`).toBeTypeOf('string');
      slots.push(`${group}.${slot}`);
    }
  }
  return slots;
}

describe('chatMarkdownTheme completeness', () => {
  it('scans a render path it could actually read', () => {
    // A scan that found nothing would pass every assertion below forever.
    expect(SOURCES.length).toBeGreaterThan(20);
    expect(SOURCES.some((f) => f.path.endsWith('/static-html.ts'))).toBe(true);
    expect(SOURCES.some((f) => f.path.endsWith('/Elements/Element.svelte'))).toBe(true);
    expect(SOURCES.some((f) => f.path.endsWith('/StreamdownCodeHost.svelte'))).toBe(true);
    expect(READ.size).toBeGreaterThan(35);
  });

  for (const entry of DYNAMIC_READS) {
    it(`still finds the computed-key read in ${entry.what}`, () => {
      const holders = SOURCES.filter((f) => f.text.includes(entry.probe));
      expect(
        holders.map((f) => f.path),
        `no render-path file contains ${entry.probe}; the slot list beside it is stale`,
      ).not.toEqual([]);
    });
  }

  it('carries every slot the render path reads', () => {
    const table = new Set(tableSlots());
    const missing = [...READ].filter((slot) => !table.has(slot)).sort();
    expect(
      missing,
      'the render path reads these slots and the table has no entry: each renders class={undefined}',
    ).toEqual([]);
  });

  it('carries no slot the render path never reads', () => {
    const dead = tableSlots()
      .filter((slot) => !READ.has(slot))
      .sort();
    expect(
      dead,
      'nothing reads these entries; delete them rather than shipping dead classes Tailwind compiles',
    ).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Class-string hygiene
// ---------------------------------------------------------------------------

// Tailwind palette scales and the raw black/white utilities the deleted vendor
// base theme reached for. None may appear in the table: they are hardcoded
// light-mode values with no dark counterpart.
//
// Written as a pattern rather than a list of literal classes on purpose:
// Tailwind scans comments and string literals alike, so quoting one here would
// compile that dead rule into the production bundle.
const PALETTE_CLASS =
  /(?:^|[\s:])(?:[a-z-]+-)?(?:(?:gray|slate|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3}|black|white)(?:\/\d+)?(?![\w-])/;

describe('chatMarkdownTheme class strings', () => {
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

  it('carries the alert title color under the modifier the markup needs', () => {
    // The title color lands on the alert's `[data-alert-title]` row, which is
    // a CHILD of the element carrying the class — so the variant entry must
    // keep the descendant modifier, not a bare text color.
    for (const variant of ['note', 'tip', 'warning', 'caution', 'important'] as const) {
      expect(chatMarkdownTheme.alert[variant]).toMatch(/\[&>\[data-alert-title\]\]:text-[a-z-]+\b/);
    }
  });

  it('leaves no raw palette class anywhere in the table', () => {
    const offenders: string[] = [];
    for (const [group, entry] of Object.entries(TABLE)) {
      for (const [slot, value] of Object.entries(entry)) {
        if (PALETTE_CLASS.test(value)) offenders.push(`${group}.${slot}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it('recognises the leak it is looking for', () => {
    // The sweep asserts a negative over every entry, so a pattern that matched
    // nothing would read as a clean theme forever. These are the exact shapes
    // the deleted vendor base theme used, assembled at runtime so no complete
    // class literal exists for Tailwind's scanner to compile.
    expect('border-gray-' + '200').toMatch(PALETTE_CLASS);
    expect('bg-' + 'white').toMatch(PALETTE_CLASS);
    expect('[&>[data-alert-title]]:text-blue-' + '600').toMatch(PALETTE_CLASS);
    expect('hover:bg-gray-' + '100/50').toMatch(PALETTE_CLASS);
    // Our own vocabulary must not read as a leak.
    expect('border-border-subtle bg-surface-1 text-fg-muted').not.toMatch(PALETTE_CLASS);
  });
});

// ---------------------------------------------------------------------------
// The block marker
// ---------------------------------------------------------------------------

describe('md-blk block marker', () => {
  // Every element that can render as a DIRECT child of the streamdown root
  // (the .md-committed / .md-volatile wrapper) carries md-blk, and nothing
  // else does. Streamdown uses it to find the two direct edge blocks and
  // publishes explicit `sd-trim-*` classes. app.css therefore needs no
  // structural pseudo whose invalidation set reacts to nested syntax-span
  // changes. See the MD_BLOCK_MARKER doc in streamdownTheme.ts.
  //
  // A block-level key missing the marker keeps its own edge margin when it
  // lands first/last in a message; an inline key carrying it would hand the
  // resets an element that is never a wrapper child. Both directions are
  // asserted.
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

  function valueOf(key: string): string | undefined {
    const [group, slot] = key.split('.') as [string, string];
    return TABLE[group]?.[slot];
  }

  for (const key of BLOCK_KEYS) {
    it(`stamps ${key} with the marker`, () => {
      expect(valueOf(key)).toMatch(markerRe);
    });
  }

  it('keeps the marker off every non-block key', () => {
    const offenders: string[] = [];
    for (const [group, entry] of Object.entries(TABLE)) {
      for (const [slot, value] of Object.entries(entry)) {
        if (BLOCK_KEYS.includes(`${group}.${slot}`)) continue;
        if (markerRe.test(value)) offenders.push(`${group}.${slot}`);
      }
    }
    expect(offenders).toEqual([]);
  });
});
