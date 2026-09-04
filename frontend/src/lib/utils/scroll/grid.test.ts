import { describe, expect, it, vi } from 'vitest';
vi.unmock('./grid');
import { measureScrollGrid } from './grid';

describe('scroll grid measurement', () => {
  it.each([1, 0.8, 2/3, 0.5, 1/2.625, 1.25, 2])('measures quantum %s under rounding, flooring, and limited readback precision', (quantum) => {
    for (const floor of [false, true]) {
      for (const precision of [0, 32]) {
        const read = (v: number) => {
          const pixels = floor ? Math.floor(v / quantum) : Math.round(v / quantum);
          const position = pixels * quantum;
          return precision ? Math.round(position * precision) / precision : position;
        };
        const grid = measureScrollGrid(read);
        expect(grid.quantum).toBeCloseTo(quantum, precision ? 2 : 6);
        for (const initial of [10, 100, 10000]) {
          let current = read(initial);
          for (const direction of [1, -1, 1]) {
            for (let event = 0; event < 100; event++) {
              const next = read(current + direction * grid.quantum + grid.writeOffset);
              expect((next - current) * direction).toBeGreaterThan(0);
              expect(Math.abs(next - current) - quantum).toBeCloseTo(0, precision ? 1 : 6);
              current = next;
            }
          }
        }
      }
    }
  });
  it('fails clearly on an unscrollable calibration surface', () => {
    expect(() => measureScrollGrid(() => 0)).toThrow('could not advance');
  });
});
