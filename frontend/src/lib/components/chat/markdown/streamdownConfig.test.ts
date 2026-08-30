import { describe, expect, it } from 'vitest';
import {
  STREAMDOWN_CONTROLS,
  STREAMDOWN_STATIC_RENDERERS,
  STREAMDOWN_STATIC_WORK_SCHEDULER,
  streamdownComponentsFor,
} from './streamdownConfig';

describe('streamdownConfig', () => {
  it('reuses stable config objects across render updates', () => {
    expect(STREAMDOWN_CONTROLS).toBe(STREAMDOWN_CONTROLS);
    expect(STREAMDOWN_STATIC_RENDERERS).toBe(STREAMDOWN_STATIC_RENDERERS);
    expect(STREAMDOWN_STATIC_WORK_SCHEDULER).toBe(STREAMDOWN_STATIC_WORK_SCHEDULER);
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
    expect(Object.isFrozen(STREAMDOWN_CONTROLS)).toBe(true);
    expect(Object.isFrozen(STREAMDOWN_STATIC_RENDERERS)).toBe(true);
    expect(Object.isFrozen(STREAMDOWN_STATIC_WORK_SCHEDULER)).toBe(true);
    expect(Object.isFrozen(streamdownComponentsFor(false))).toBe(true);
    expect(Object.isFrozen(streamdownComponentsFor(true))).toBe(true);
  });
});
