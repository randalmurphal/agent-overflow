import { beforeEach, describe, expect, it } from 'vitest';
import { importUnavailableLabel } from './importUnavailable';
import { __resetParseJsonObjectCacheForTest } from './parseJsonObject';

// Terminal period: this lands in the same slot as the sibling empty
// messages ("No stored payload for this tool result."), which all have one.
const LABEL = 'Not available from import.';

function withMeta(meta: unknown): { meta?: string } {
  return { meta: typeof meta === 'string' ? meta : JSON.stringify(meta) };
}

beforeEach(() => {
  __resetParseJsonObjectCacheForTest();
});

describe('importUnavailableLabel', () => {
  it('returns null when the item is not an import casualty', () => {
    expect(importUnavailableLabel(undefined)).toBeNull();
    expect(importUnavailableLabel(null)).toBeNull();
    expect(importUnavailableLabel({})).toBeNull();
    expect(importUnavailableLabel(withMeta({ pathRefs: [] }))).toBeNull();
  });

  it('labels every reason the importer stamps today', () => {
    expect(importUnavailableLabel(withMeta({ import_unavailable: 'tool-output-gc' }))).toBe(LABEL);
    expect(importUnavailableLabel(withMeta({ import_unavailable: 'exec-detail' }))).toBe(LABEL);
  });

  it('labels an unknown reason rather than falling back to the caller default', () => {
    expect(importUnavailableLabel(withMeta({ import_unavailable: 'some-future-reason' }))).toBe(LABEL);
  });

  it('accepts any truthy value, not just the string reasons', () => {
    expect(importUnavailableLabel(withMeta({ import_unavailable: true }))).toBe(LABEL);
    expect(importUnavailableLabel(withMeta({ import_unavailable: 1 }))).toBe(LABEL);
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
