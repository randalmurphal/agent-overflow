import { describe, expect, it } from 'vitest';
import { isLocalEnter } from './localEnter';

const key = (init: Partial<KeyboardEvent>) => ({ key: 'Enter', altKey: false, ctrlKey: false, metaKey: false, ...init });

describe('isLocalEnter', () => {
  it('owns plain Enter and Shift+Enter', () => {
    expect(isLocalEnter(key({}))).toBe(true);
    expect(isLocalEnter(key({ shiftKey: true }))).toBe(true);
  });

  it('leaves Enter with Alt, Ctrl or Meta, and other keys, to the app', () => {
    expect(isLocalEnter(key({ altKey: true }))).toBe(false);
    expect(isLocalEnter(key({ ctrlKey: true }))).toBe(false);
    expect(isLocalEnter(key({ metaKey: true }))).toBe(false);
    expect(isLocalEnter(key({ key: ' ' }))).toBe(false);
  });
});
