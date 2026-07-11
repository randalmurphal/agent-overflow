// Tracks the moment the document last returned to visibility. The spring's
// chase-distance clamp uses this as one of its two discontinuity signals
// (the other is the observed rAF gap on the current tick): a chase that
// starts fresh shortly after visibilitychange → visible may clamp a
// larger-than-viewport backlog, while a chase during normal visible
// operation never clamps — a >viewport structural mount (diff card, big
// command flush) is spring-routed by design during live follow
// (resolver.ts positiveWillPin) and must keep its full bounded glide.
//
// One idempotent document-level listener, installed lazily by the first
// spring factory. Clock goes through nowMs so tests share the mocked
// performance.now the rAF harnesses drive.
import { nowMs } from './time';

let lastResumeAt: number | null = null;
let installed = false;

export function installDocumentResumeTracking(): void {
  if (installed || typeof document === 'undefined') return;
  installed = true;
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') lastResumeAt = nowMs();
  });
}

export function msSinceDocumentResume(): number {
  return lastResumeAt === null ? Number.POSITIVE_INFINITY : nowMs() - lastResumeAt;
}

export function setDocumentResumeAtForTest(value: number | null): void {
  lastResumeAt = value;
}
