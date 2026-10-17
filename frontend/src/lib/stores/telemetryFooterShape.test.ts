import { describe, expect, it } from 'vitest';
import {
  MAX_FOOTER_LINES,
  MAX_FOOTER_SHAPES,
  getTelemetryFooterShape,
  recordTelemetryFooterShape,
  telemetryFooterShapeKey,
} from './telemetryFooterShape.svelte';

const STORAGE = 'agent-overflow:frontend:telemetry-footer-shapes';
const shape = (lines: number, picker = false) => ({ lines, picker });

function storedKeys(): string[] {
  return (JSON.parse(localStorage.getItem(STORAGE) ?? '[]') as Array<[string]>).map(([key]) => key);
}

describe('telemetry footer shapes', () => {
  it('keys by footer, sorted selection and period', () => {
    expect(telemetryFooterShapeKey('usage', ['b', 'a'], 'month')).toBe(telemetryFooterShapeKey('usage', ['a', 'b'], 'month'));
    expect(telemetryFooterShapeKey('usage', null, 'month')).not.toBe(telemetryFooterShapeKey('usage', [], 'month'));
    expect(telemetryFooterShapeKey('usage', null, 'month')).not.toBe(telemetryFooterShapeKey('usage', null, 'day'));
    expect(telemetryFooterShapeKey('usage', null)).not.toBe(telemetryFooterShapeKey('system', null));
  });

  it('records and persists a shape', () => {
    recordTelemetryFooterShape('k', shape(2, true));
    expect(getTelemetryFooterShape('k')).toEqual(shape(2, true));
    expect(JSON.parse(localStorage.getItem(STORAGE)!)).toEqual([['k', 2, 1]]);
  });

  it('clamps line counts', () => {
    recordTelemetryFooterShape('negative', shape(-3));
    recordTelemetryFooterShape('large', shape(MAX_FOOTER_LINES + 10));
    recordTelemetryFooterShape('fraction', shape(2.7));
    expect(getTelemetryFooterShape('negative')?.lines).toBe(0);
    expect(getTelemetryFooterShape('large')?.lines).toBe(MAX_FOOTER_LINES);
    expect(getTelemetryFooterShape('fraction')?.lines).toBe(2);
  });

  it(`keeps at most ${MAX_FOOTER_SHAPES} shapes, evicting the oldest`, () => {
    for (let i = 0; i <= MAX_FOOTER_SHAPES; i++) recordTelemetryFooterShape(`k${i}`, shape(1));
    expect(getTelemetryFooterShape('k0')).toBeNull();
    expect(getTelemetryFooterShape(`k${MAX_FOOTER_SHAPES}`)).not.toBeNull();
    expect(storedKeys()).toHaveLength(MAX_FOOTER_SHAPES);
  });

  it('refreshes recency when an unchanged shape is recorded again', () => {
    recordTelemetryFooterShape('default', shape(1));
    for (let i = 1; i < MAX_FOOTER_SHAPES; i++) recordTelemetryFooterShape(`k${i}`, shape(1));
    recordTelemetryFooterShape('default', shape(1));
    recordTelemetryFooterShape('newest', shape(1));
    expect(getTelemetryFooterShape('default')).toEqual(shape(1));
    expect(getTelemetryFooterShape('k1')).toBeNull();
    expect(storedKeys().at(-2)).toBe('default');
  });

  it('drops malformed stored entries when another window writes', () => {
    localStorage.setItem(STORAGE, JSON.stringify([['ok', 2, 0], ['bad-lines', -1, 0], ['bad-picker', 1, 2], [3, 1, 0], ['short', 1]]));
    window.dispatchEvent(new StorageEvent('storage', { key: STORAGE, storageArea: localStorage }));
    expect(getTelemetryFooterShape('ok')).toEqual(shape(2));
    for (const key of ['bad-lines', 'bad-picker', 'short']) expect(getTelemetryFooterShape(key)).toBeNull();
  });
});
