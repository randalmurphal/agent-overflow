// The local half of a browser pane's geometry report on the Windows
// launcher. The SPA's webview and the pane's native view live in the same
// launcher process, so the launcher takes each report straight from here
// (chrome.webview.postMessage, which Wails hands to its raw message handler)
// and moves the view on its own UI thread, while the same report still goes
// to the backend over the transport. The Go half is
// internal/webview2host/geometry.go; the names below mirror it.

// GeometryMessagePrefix: the launcher reads only messages starting with it.
const MESSAGE_PREFIX = 'ao:pane-geometry:';

// GeometryCapabilityGlobal: set by a launcher that takes these messages. An
// older launcher sets nothing and is never posted to.
const CAPABILITY_GLOBAL = '__aoPaneGeometryBridge';

/** One report, in the SPA's CSS pixels; `seq` counts per mount from 1. */
export interface PaneGeometryReport {
  mount: string;
  seq: number;
  x: number;
  y: number;
  width: number;
  height: number;
  clipX: number;
  clipY: number;
  clipWidth: number;
  clipHeight: number;
  viewportWidth: number;
  viewportHeight: number;
  devicePixelRatio: number;
  visible: boolean;
  background: string;
}

interface BridgeWindow {
  [CAPABILITY_GLOBAL]?: unknown;
  chrome?: { webview?: { postMessage?: (message: string) => void } };
}

/** Whether this document's host takes pane geometry directly. */
export function paneGeometryBridgeAvailable(): boolean {
  if (typeof window === 'undefined') return false;
  const w = window as Window & BridgeWindow;
  return w[CAPABILITY_GLOBAL] === 1 && typeof w.chrome?.webview?.postMessage === 'function';
}

/**
 * Posts one report to the host when it takes them, and answers whether it
 * did. A throw from the native bridge propagates to the caller.
 */
export function postPaneGeometry(report: PaneGeometryReport): boolean {
  if (!paneGeometryBridgeAvailable()) return false;
  // The bridge method needs its owning receiver: call it on webview.
  const webview = (window as Window & BridgeWindow).chrome!.webview!;
  webview.postMessage!(MESSAGE_PREFIX + JSON.stringify(report));
  return true;
}
