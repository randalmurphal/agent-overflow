import { describe, expect, it } from 'vitest';
import {
  MAX_NAME_LENGTH,
  MAX_VALUE_LENGTH,
  parseTheme,
  parseThemeDoc,
  type ThemeWarning,
} from './themeParse';

const json = (value: unknown): string => JSON.stringify(value);

function codes(warnings: readonly ThemeWarning[]): string[] {
  return warnings.map((w) => w.code);
}

describe('parseThemeDoc', () => {
  it('is parseTheme without the JSON round trip', () => {
    // The built-ins are authored as object literals and used to reach the
    // parser by stringifying ~31KB and re-parsing it at module init, on the
    // eager startup path. Same validation, same warnings — asserted against
    // the file entry point so the two can never diverge.
    for (const doc of [
      {
        name: 'Midnight',
        dark: { colors: { 'surface-0': 'black', nope: 'red' }, syntax: { 'syntax-tag': 'lime' } },
        light: { colors: { 'surface-0': '' } },
        bogus: 1,
      },
      { name: 'Empty' },
      { dark: 'not an object' },
    ]) {
      expect(parseThemeDoc('t', doc)).toEqual(parseTheme('t', json(doc)));
    }
  });

  it('reports a non-object document the same way the file path does', () => {
    expect(codes(parseThemeDoc('t', ['array']).warnings)).toEqual(['not-an-object']);
    expect(codes(parseThemeDoc('t', null).warnings)).toEqual(['not-an-object']);
  });
});

describe('parseTheme', () => {
  it('reads a two-variant file into sparse sections', () => {
    const theme = parseTheme(
      'midnight',
      json({
        $schema: './theme.schema.json',
        name: 'Midnight',
        dark: { colors: { 'surface-0': 'oklch(0.1 0 0)' }, syntax: { 'syntax-keyword': 'red' } },
        light: { colors: { 'surface-0': 'white' } },
      }),
    );

    expect(theme.warnings).toEqual([]);
    expect(theme.id).toBe('midnight');
    expect(theme.name).toBe('Midnight');
    expect(theme.variants.dark?.colors).toEqual({ 'surface-0': 'oklch(0.1 0 0)' });
    expect(theme.variants.dark?.syntax).toEqual({ 'syntax-keyword': 'red' });
    expect(theme.variants.light?.colors).toEqual({ 'surface-0': 'white' });
    expect(theme.axes).toEqual({ ui: true, code: true });
    expect(theme.builtin).toBe(false);
  });

  it('names itself after the file when name is absent or unusable', () => {
    expect(parseTheme('my-theme', json({ dark: { colors: { accent: 'red' } } })).name).toBe('my-theme');

    const blank = parseTheme('my-theme', json({ name: '   ', dark: { colors: { accent: 'red' } } }));
    expect(blank.name).toBe('my-theme');
    expect(codes(blank.warnings)).toEqual(['invalid-name']);

    const long = parseTheme('my-theme', json({ name: 'x'.repeat(MAX_NAME_LENGTH + 1) }));
    expect(long.name).toBe('my-theme');
    expect(codes(long.warnings)).toContain('invalid-name');
  });

  it('reports the axes a file is selectable on', () => {
    const uiOnly = parseTheme('a', json({ dark: { colors: { accent: 'red' } } }));
    expect(uiOnly.axes).toEqual({ ui: true, code: false });

    const codeOnly = parseTheme('b', json({ light: { ansi: { 'ansi-fg-31': 'red' } } }));
    expect(codeOnly.axes).toEqual({ ui: false, code: true });

    const codeGrounds = parseTheme('c', json({ dark: { code: { 'terminal-bg': 'black' } } }));
    expect(codeGrounds.axes).toEqual({ ui: false, code: true });
  });

  // -------------------------------------------------------------------------
  // Warning taxonomy — every reachable malformation is a warning plus a skip
  // -------------------------------------------------------------------------

  it('survives a file that is not JSON', () => {
    const theme = parseTheme('broken', '{ "dark": ');
    expect(codes(theme.warnings)).toEqual(['invalid-json']);
    expect(theme.variants).toEqual({});
    expect(theme.axes).toEqual({ ui: false, code: false });
    expect(theme.name).toBe('broken');
  });

  it('survives a file that is JSON but not an object', () => {
    for (const raw of ['[]', '"a string"', '42', 'null']) {
      const theme = parseTheme('odd', raw);
      expect(codes(theme.warnings), raw).toEqual(['not-an-object']);
    }
  });

  it('warns per unknown root key, section and token', () => {
    const theme = parseTheme(
      't',
      json({
        id: 'ignored',
        extends: 'dark',
        dark: {
          colours: { accent: 'red' },
          colors: { accent: 'red', 'surface-9': 'red', 'syntax-keyword': 'red' },
        },
      }),
    );

    expect(codes(theme.warnings).sort()).toEqual([
      'unknown-key',
      'unknown-key',
      'unknown-root-key',
      'unknown-root-key',
      'unknown-section',
    ]);
    // A key from another section is still an unknown key HERE — sections are
    // separate namespaces, not decoration.
    expect(theme.variants.dark?.colors).toEqual({ accent: 'red' });
    expect(theme.warnings.find((w) => w.code === 'unknown-key')?.path).toBe('dark.colors.surface-9');
  });

  it('warns and skips per bad value, keeping the rest of the section', () => {
    const theme = parseTheme(
      't',
      json({
        dark: {
          colors: {
            accent: 'red',
            'surface-0': 42,
            'surface-1': '   ',
            'surface-2': 'x'.repeat(MAX_VALUE_LENGTH + 1),
            'surface-3': ['red'],
          },
        },
      }),
    );

    expect(theme.variants.dark?.colors).toEqual({ accent: 'red' });
    expect(codes(theme.warnings).sort()).toEqual([
      'empty-value',
      'non-string-value',
      'non-string-value',
      'value-too-long',
    ]);
    expect(theme.axes.ui).toBe(true);
  });

  it('trims values and accepts one exactly at the cap', () => {
    const atCap = 'a'.repeat(MAX_VALUE_LENGTH);
    const theme = parseTheme('t', json({ dark: { colors: { accent: `  ${atCap}  ` } } }));
    expect(theme.warnings).toEqual([]);
    expect(theme.variants.dark?.colors).toEqual({ accent: atCap });
  });

  it('warns when a variant or section is not an object', () => {
    const badVariant = parseTheme('t', json({ dark: 'nope' }));
    expect(codes(badVariant.warnings)).toEqual(['not-an-object', 'empty-theme']);
    expect(badVariant.warnings[0]!.path).toBe('dark');

    const badSection = parseTheme('t', json({ dark: { colors: 'nope', syntax: { 'syntax-tag': 'red' } } }));
    expect(codes(badSection.warnings)).toEqual(['not-an-object']);
    expect(badSection.warnings[0]!.path).toBe('dark.colors');
    expect(badSection.variants.dark?.syntax).toEqual({ 'syntax-tag': 'red' });
  });

  it('warns when the file ends up defining nothing', () => {
    const theme = parseTheme('empty', json({ name: 'Empty', dark: {}, light: {} }));
    expect(codes(theme.warnings)).toEqual(['empty-theme']);
    expect(theme.variants).toEqual({});
  });

  it('stamps every warning with the theme id and a path', () => {
    const theme = parseTheme('stamped', json({ dark: { colors: { nope: 'red' } } }));
    for (const warning of theme.warnings) {
      expect(warning.themeId).toBe('stamped');
      expect(typeof warning.path).toBe('string');
      expect(warning.message.length).toBeGreaterThan(0);
    }
  });

  it('does not judge whether a value is a color', () => {
    // Colour validity needs CSS.supports, which needs a browser. Anything
    // string-shaped and short enough passes here and is rejected later, per
    // token, with the theme still applying around it.
    const theme = parseTheme('t', json({ dark: { colors: { accent: 'not-a-color-at-all' } } }));
    expect(theme.warnings).toEqual([]);
    expect(theme.variants.dark?.colors).toEqual({ accent: 'not-a-color-at-all' });
  });

  it('drops a section that had only unknown keys rather than keeping it empty', () => {
    const theme = parseTheme('t', json({ dark: { colors: { nope: 'red' } } }));
    expect(theme.variants).toEqual({});
    expect(theme.axes).toEqual({ ui: false, code: false });
    expect(codes(theme.warnings)).toEqual(['unknown-key', 'empty-theme']);
  });
});
