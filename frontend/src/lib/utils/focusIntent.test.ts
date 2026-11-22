import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { elementFocusScope, focusChosen, intendFocus, resetFocusIntentForTest } from './focusIntent';

describe('the latest focus intent', () => {
  let area: HTMLElement;
  let outside: HTMLInputElement;
  beforeEach(() => {
    area = document.createElement('div');
    outside = document.createElement('input');
    document.body.append(area, outside);
  });
  afterEach(() => {
    area.remove();
    outside.remove();
    resetFocusIntentForTest();
  });

  it('is canceled, with its callback, by a newer intent, and the newer one lands', () => {
    const first = vi.fn();
    const older = intendFocus(elementFocusScope(area), first);
    const newer = intendFocus(elementFocusScope(area));
    expect(older.current).toBe(false);
    expect(first).toHaveBeenCalledTimes(1);
    expect(older.land(() => outside.focus())).toBe(false);
    const landed = vi.fn();
    expect(newer.land(landed)).toBe(true);
    expect(landed).toHaveBeenCalledTimes(1);
  });

  it('ends quietly when its owner ends it or it lands, and watches nothing after', () => {
    const canceled = vi.fn();
    intendFocus(elementFocusScope(area), canceled).end();
    intendFocus(elementFocusScope(area), canceled).land(() => {});
    outside.focus();
    focusChosen();
    expect(canceled).not.toHaveBeenCalled();
  });

  it('is canceled by a focus outside its scope, once', () => {
    const canceled = vi.fn();
    const intent = intendFocus(elementFocusScope(area), canceled);
    outside.focus();
    expect(intent.current).toBe(false);
    outside.blur();
    outside.focus();
    expect(canceled).toHaveBeenCalledTimes(1);
  });
});
