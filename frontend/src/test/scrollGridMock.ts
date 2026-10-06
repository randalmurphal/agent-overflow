// happy-dom stores scrollTop verbatim and has no layout engine. The browser
// suites exercise real calibration; unit controllers use a whole-CSS-pixel
// engine.
//
// Its own setup file: Vitest keeps every file's mock factory for the life of
// the worker, and a factory declared in setup.ts would keep that file's whole
// module graph reachable through setup.ts's module scope.
import { vi } from 'vitest';

vi.mock('../lib/utils/scroll/grid', () => ({
  documentScrollGrid: () => ({ quantum: 1, writeOffset: 0, readbackError: 0 }),
}));
