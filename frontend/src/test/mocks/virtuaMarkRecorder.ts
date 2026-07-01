// Shared recorder for StubVirtualizer.svelte. The stub records each
// markProgrammaticScroll() call so a test can assert MessageTimeline's
// onBeforeScrollTopWrite wiring actually reaches the bound virtua handle
// (patches/virtua@0.49.1.patch). Both the stub and the test import this
// module, so they share the same counter instance.

let markCalls = 0;

export function recordVirtuaMark(): void {
  markCalls += 1;
}

export function virtuaMarkCalls(): number {
  return markCalls;
}

export function resetVirtuaMarkCalls(): void {
  markCalls = 0;
}
