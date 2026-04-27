import { describe, it, expect } from 'vitest';
import { languageFromPath } from './diffLanguage';

describe('languageFromPath', () => {
  it('maps common extensions to Shiki language ids', () => {
    expect(languageFromPath('src/foo.ts')).toBe('typescript');
    expect(languageFromPath('src/Component.tsx')).toBe('tsx');
    expect(languageFromPath('main.go')).toBe('go');
    expect(languageFromPath('script.py')).toBe('python');
    expect(languageFromPath('App.svelte')).toBe('svelte');
    expect(languageFromPath('config.yaml')).toBe('yaml');
    expect(languageFromPath('config.yml')).toBe('yaml');
    expect(languageFromPath('install.sh')).toBe('bash');
    expect(languageFromPath('lib.rs')).toBe('rust');
  });

  it('falls back to plaintext for unknown extensions', () => {
    expect(languageFromPath('binary.bin')).toBe('plaintext');
    expect(languageFromPath('archive.tar.gz')).toBe('plaintext');
    expect(languageFromPath('LICENSE')).toBe('plaintext');
  });

  it('handles paths with directories correctly', () => {
    expect(languageFromPath('frontend/src/lib/components/Foo.svelte')).toBe('svelte');
    expect(languageFromPath('a/b/c/d.ts')).toBe('typescript');
  });

  it('strips dirname when matching extension', () => {
    // Don't get fooled by directories whose names contain dots.
    expect(languageFromPath('node_modules/.bin/something')).toBe('plaintext');
    expect(languageFromPath('a.b.c/file.go')).toBe('go');
  });

  it('is case-insensitive on extension', () => {
    expect(languageFromPath('Foo.TS')).toBe('typescript');
    expect(languageFromPath('Bar.Py')).toBe('python');
  });
});
