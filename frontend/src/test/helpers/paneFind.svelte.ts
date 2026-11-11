// A reactive stand-in find provider for component tests of the find bar.
import { vi, type Mock } from 'vitest';
import type { PaneFindProvider } from '../../lib/stores/paneFind.svelte';

export interface FakeFindProvider extends PaneFindProvider {
  set(result: { count?: number | null; current?: number; pending?: boolean; capped?: boolean }): void;
  readonly search: Mock<(query: string) => void>;
  readonly next: Mock<() => void>;
  readonly previous: Mock<() => void>;
  readonly clear: Mock<() => void>;
}

export function fakeFindProvider(noun = 'thread'): FakeFindProvider {
  const state = $state<{ count: number | null; current: number; pending: boolean; capped: boolean }>({
    count: 0, current: -1, pending: false, capped: false,
  });
  return {
    noun,
    result: {
      get count() { return state.count; },
      get current() { return state.current; },
      get pending() { return state.pending; },
      get capped() { return state.capped; },
    },
    set(next) { Object.assign(state, next); },
    search: vi.fn<(query: string) => void>(),
    next: vi.fn<() => void>(),
    previous: vi.fn<() => void>(),
    clear: vi.fn<() => void>(),
  };
}
