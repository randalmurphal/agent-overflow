import { describe, expect, it } from 'vitest';
import { parsePRReference } from './prReference';

describe('parsePRReference', () => {
  it('parses https://github.com/OWNER/REPO/pull/N', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ owner: 'foo', repo: 'bar', number: 42 });
  });

  it('parses without a scheme', () => {
    const r = parsePRReference('github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ owner: 'foo', repo: 'bar', number: 42 });
  });

  it('parses http:// (not just https)', () => {
    const r = parsePRReference('http://github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(42);
  });

  it('parses short-form OWNER/REPO#N', () => {
    const r = parsePRReference('foo/bar#321');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ owner: 'foo', repo: 'bar', number: 321 });
  });

  it('tolerates trailing path segments after the number', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/15/files');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(15);
  });

  it('tolerates trailing query strings', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/15?diff=split');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(15);
  });

  it('tolerates trailing anchors', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/15#issuecomment-9');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(15);
  });

  it('trims surrounding whitespace', () => {
    const r = parsePRReference('   https://github.com/foo/bar/pull/7   ');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(7);
  });

  it('rejects empty input', () => {
    const r = parsePRReference('');
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toMatch(/empty/i);
  });

  it('rejects whitespace-only input', () => {
    const r = parsePRReference('   ');
    expect(r.ok).toBe(false);
  });

  it('rejects non-github URLs', () => {
    const r = parsePRReference('https://gitlab.com/foo/bar/pull/1');
    expect(r.ok).toBe(false);
  });

  it('rejects issues URLs', () => {
    const r = parsePRReference('https://github.com/foo/bar/issues/1');
    expect(r.ok).toBe(false);
  });

  it('rejects references missing a number', () => {
    const r = parsePRReference('foo/bar#');
    expect(r.ok).toBe(false);
  });

  it('rejects non-integer PR numbers', () => {
    const r = parsePRReference('foo/bar#abc');
    expect(r.ok).toBe(false);
  });

  it('rejects zero and negative PR numbers', () => {
    expect(parsePRReference('foo/bar#0').ok).toBe(false);
    expect(parsePRReference('foo/bar#-3').ok).toBe(false);
    expect(parsePRReference('https://github.com/foo/bar/pull/0').ok).toBe(false);
  });

  it('rejects owner/repo with extra path segments', () => {
    const r = parsePRReference('foo/bar/baz#1');
    expect(r.ok).toBe(false);
  });

  it('rejects plain text', () => {
    const r = parsePRReference('not a url');
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toMatch(/Unrecognised/);
  });
});
