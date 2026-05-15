function initialViewportWidth(): number {
  return typeof window === 'undefined' ? 1200 : window.innerWidth;
}

let appShellWidth = $state(initialViewportWidth());
let paneHostWidth = $state(Number.POSITIVE_INFINITY);
let paneWidthById: Map<string, number> = $state(new Map());
let appShellMeasured = $state(false);

export function getAppShellWidth(): number {
  return appShellMeasured ? appShellWidth : initialViewportWidth();
}

export function setAppShellWidth(width: number): void {
  if (!Number.isFinite(width) || width <= 0) return;
  appShellWidth = Math.round(width);
  appShellMeasured = true;
}

export function getPaneHostWidth(): number {
  return paneHostWidth;
}

export function setPaneHostWidth(width: number): void {
  if (!Number.isFinite(width) || width <= 0) return;
  paneHostWidth = Math.round(width);
}

export function getPaneWidth(paneId: string): number {
  const measuredPaneWidth = paneWidthById.get(paneId);
  if (measuredPaneWidth !== undefined) return measuredPaneWidth;
  const hostWidth = getPaneHostWidth();
  return Number.isFinite(hostWidth) ? hostWidth : getAppShellWidth();
}

export function setPaneWidth(paneId: string, width: number): void {
  if (!paneId || !Number.isFinite(width) || width <= 0) return;
  const rounded = Math.round(width);
  if (paneWidthById.get(paneId) === rounded) return;
  paneWidthById = new Map(paneWidthById).set(paneId, rounded);
}

export function clearPaneWidth(paneId: string): void {
  if (!paneWidthById.has(paneId)) return;
  paneWidthById = new Map(paneWidthById);
  paneWidthById.delete(paneId);
}

export function resetLayoutMetricsForTest(): void {
  appShellWidth = initialViewportWidth();
  appShellMeasured = false;
  paneHostWidth = Number.POSITIVE_INFINITY;
  paneWidthById = new Map();
}
