import { describe, expect, it } from 'vitest';
import {
  STREAMDOWN_ALLOWED_IMAGE_PREFIXES,
  STREAMDOWN_CONTROLS,
  streamdownComponentsFor,
} from './streamdownConfig';

describe('streamdownConfig', () => {
  it('reuses stable config objects across render updates', () => {
    expect(STREAMDOWN_ALLOWED_IMAGE_PREFIXES).toBe(STREAMDOWN_ALLOWED_IMAGE_PREFIXES);
    expect(STREAMDOWN_CONTROLS).toBe(STREAMDOWN_CONTROLS);
    expect(streamdownComponentsFor(false)).toBe(streamdownComponentsFor(false));
    expect(streamdownComponentsFor(true)).toBe(streamdownComponentsFor(true));
  });

  it('keeps complete and incomplete host maps distinct', () => {
    const complete = streamdownComponentsFor(false);
    const incomplete = streamdownComponentsFor(true);

    expect(complete).not.toBe(incomplete);
    expect(complete.code).toBe(incomplete.code);
    expect(complete.mermaid).not.toBe(incomplete.mermaid);
    expect(complete.math).not.toBe(incomplete.math);
  });

  it('freezes shared objects so callers cannot mutate the hot-path config', () => {
    expect(Object.isFrozen(STREAMDOWN_ALLOWED_IMAGE_PREFIXES)).toBe(true);
    expect(Object.isFrozen(STREAMDOWN_CONTROLS)).toBe(true);
    expect(Object.isFrozen(streamdownComponentsFor(false))).toBe(true);
    expect(Object.isFrozen(streamdownComponentsFor(true))).toBe(true);
  });
});
