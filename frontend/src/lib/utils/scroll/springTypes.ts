import type { ScrollWriteCaller } from './types';
import type { ScrollGrid } from './grid';

export interface SpringWriteRefusalEvent {
  phase: 'latched' | 'healed' | 'abandoned';
  consecutiveRefusals: number;
  requested: number;
  scrollTop: number;
  target: number;
  wedgeMs: number;
}

/** Acceptance of an exact target request that the browser rounded or clamped. */
export interface ArrivalReadback {
  matches(target: number): boolean;
  record(target: number): void;
  shouldWriteExact(target: number): boolean;
  writeExact(caller: ScrollWriteCaller, target: number): void;
  clear(): void;
  invalidateStale(target: number): void;
}

export interface SpringChaseDeps {
  getScrollEl(): HTMLElement | undefined;
  isPaused(): boolean;
  /** Following intent, independent of whether geometry has caught up. */
  isAtBottom(): boolean;
  isEscaped(): boolean;
  /** Suspend writes and advance the clock without ending the chase. */
  selectionActive(): boolean;
  /** Force a native geometry read for sentinel/clamp/refusal arbitration. */
  targetScrollTop(forceLayoutRead?: boolean): number;
  currentScrollTop(forceLayoutRead?: boolean): number;
  arrival: ArrivalReadback;
  writeScrollTop(caller: ScrollWriteCaller, value: number, bottomTarget: number): number | undefined;
  liveContentActive(): boolean;
  prefersReducedMotion(): boolean;
  scrollGrid(): ScrollGrid;
  forceNextSpringTickTrace(): void;
  scrollTopUnexplained(): boolean;
  reportWriteRefusal(event: SpringWriteRefusalEvent): void;
}

export interface SpringChase {
  start(): void;
  cancel(): void;
  snapOscillationToBottom(caller: ScrollWriteCaller, top: number): void;
  gateOpen(): boolean;
  isActive(): boolean;
  token(): number;
  /** CSS pixels per 60Hz-equivalent frame; not the quantized displacement. */
  velocityForTest(): number;
  requestStop(): void;
  clearStopRequest(): void;
  stopRequested(): boolean;
  markTargetChanged(): void;
  markStructuralAppend(): void;
  structuralAppendPending(): boolean;
  clearStructuralAppend(): void;
  sentinelTarget(): number;
  sentinelClampWitnessed(): boolean;
  refusalLatched(): boolean;
}
