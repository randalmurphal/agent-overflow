// Reactive stand-in find providers (a stepper, a filter) for component
// tests of the find bar.
import { vi, type Mock } from 'vitest';
import type { PaneFindProvider } from '../../lib/stores/paneFind.svelte';

export interface FakeFindProvider extends PaneFindProvider {
  set(result: { count?: number | null; current?: number; pending?: boolean; capped?: boolean; progressive?: boolean; error?: string | null }): void;
  readonly search: Mock<(query: string) => void>;
  readonly next: Mock<() => void>;
  readonly previous: Mock<() => void>;
  readonly clear: Mock<() => void>;
}

export function fakeFindProvider(noun = 'thread'): FakeFindProvider {
  const state = $state<{ count: number | null; current: number; pending: boolean; capped: boolean; progressive: boolean; error: string | null }>({
    count: 0, current: -1, pending: false, capped: false, progressive: false, error: null,
  });
  return {
    noun,
    result: {
      get count() { return state.count; },
      get current() { return state.current; },
      get pending() { return state.pending; },
      get capped() { return state.capped; },
      get progressive() { return state.progressive; },
      get error() { return state.error; },
    },
    set(next) { Object.assign(state, next); },
    search: vi.fn<(query: string) => void>(),
    next: vi.fn<() => void>(),
    previous: vi.fn<() => void>(),
    clear: vi.fn<() => void>(),
  };
}

export interface FakeFilterProvider extends PaneFindProvider {
  set(result: { count?: number; pending?: boolean }): void;
  readonly search: Mock<(query: string) => void>;
  readonly focusFirst: Mock<() => boolean>;
  readonly clear: Mock<() => void>;
}

/** A filter: no `next` / `previous`; Enter runs `focusFirst`. */
export function fakeFilterProvider(noun: string, focusFirst: () => boolean = () => false): FakeFilterProvider {
  const state = $state({ count: 0, pending: false });
  return {
    noun,
    result: {
      get count() { return state.count; },
      current: -1,
      get pending() { return state.pending; },
    },
    set(next) { Object.assign(state, next); },
    search: vi.fn<(query: string) => void>(),
    focusFirst: vi.fn<() => boolean>(focusFirst),
    clear: vi.fn<() => void>(),
  };
}
