import { describe, expect, it } from 'vitest';
import { importUnavailableLabel } from './importUnavailable';

// Terminal period: this lands in the same slot as the sibling empty
// messages ("No stored payload for this tool result."), which all have one.
const LABEL = 'Not available from import.';

function withMeta(meta: unknown): { meta?: string } {
  return { meta: typeof meta === 'string' ? meta : JSON.stringify(meta) };
}

describe('importUnavailableLabel', () => {
  it('returns null when the item is not an import casualty', () => {
    expect(importUnavailableLabel(undefined)).toBeNull();
    expect(importUnavailableLabel(null)).toBeNull();
    expect(importUnavailableLabel({})).toBeNull();
    expect(importUnavailableLabel(withMeta({ pathRefs: [] }))).toBeNull();
  });

  it.each([
    ['labels every reason the importer stamps today', ['tool-output-gc', 'exec-detail']],
    ['labels an unknown reason rather than falling back to the caller default', ['some-future-reason']],
    ['accepts any truthy value, not just the string reasons', [true, 1]],
  ])('%s', (_name, reasons) => {
    for (const reason of reasons) {
      expect(importUnavailableLabel(withMeta({ import_unavailable: reason })), String(reason)).toBe(LABEL);
    }
  });

  it('treats a falsy or empty reason as no reason', () => {
    expect(importUnavailableLabel(withMeta({ import_unavailable: '' }))).toBeNull();
    expect(importUnavailableLabel(withMeta({ import_unavailable: false }))).toBeNull();
    expect(importUnavailableLabel(withMeta({ import_unavailable: null }))).toBeNull();
  });

  it('survives meta that is not a JSON object', () => {
    expect(importUnavailableLabel({ meta: 'not json at all' })).toBeNull();
    expect(importUnavailableLabel({ meta: '[1,2,3]' })).toBeNull();
    expect(importUnavailableLabel({ meta: 'null' })).toBeNull();
    expect(importUnavailableLabel({ meta: '' })).toBeNull();
  });
});
