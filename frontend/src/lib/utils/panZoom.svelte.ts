// Pan and zoom for a full-viewport viewer: the diagram modal and the image
// lightbox share one controller so wheel, drag, pinch and keyboard feel the
// same in both.
//
// The state is three scalars (scale and a translation) applied as one CSS
// transform on the content host, so a zoom is a single style write and the
// compositor does the rest; nothing is re-laid-out at any scale. `scale`
// is content units per CSS pixel: 1 paints an image at one image pixel per
// CSS pixel and a diagram at one modelling unit per CSS pixel.
//
// Fit is the viewer's resting state. `userZoomed` records that the person
// has zoomed or panned by hand, which is what stops a canvas resize from
// clobbering their view; `fit()` clears it.

export interface PanZoomOptions {
  /** The element the content is positioned in; its rect anchors every zoom. */
  canvas: () => HTMLElement | undefined;
  /** Lowest and highest `scale` a manual zoom may reach. */
  minScale: number;
  maxScale: number;
  /**
   * The most `fit()` may enlarge content that is smaller than the canvas.
   * A tiny diagram fills the canvas up to this; an image never grows past
   * 1 on fit, so a small screenshot opens at its own size.
   */
  fitMaxScale: number;
}

export interface ContentSize {
  width: number;
  height: number;
}

/** How far one arrow key moves the content, in CSS pixels. */
export const PAN_STEP_PX = 50;
/** Factor one zoom-in key or button applies; zoom-out is its inverse. */
export const ZOOM_STEP = 1.25;
/**
 * Wheel distance to zoom factor: exponential so the feel is the same for a
 * trackpad's many small deltas and a wheel's few large ones. Tuned to macOS
 * natural scrolling.
 */
const WHEEL_ZOOM_RATE = 0.002;
/** A pinch narrower than this is a pointer jitter, not a zoom. */
const MIN_PINCH_DISTANCE_PX = 1;

interface PointerPoint {
  x: number;
  y: number;
}

export class PanZoom {
  scale = $state(1);
  tx = $state(0);
  ty = $state(0);
  userZoomed = $state(false);
  /** A drag or pinch is in progress; the cursor and selection follow it. */
  panning = $state(false);
  readonly transform = $derived(`matrix(${this.scale}, 0, 0, ${this.scale}, ${this.tx}, ${this.ty})`);

  private readonly options: PanZoomOptions;
  private readonly pointers = new Map<number, PointerPoint>();
  private panOrigin: { x: number; y: number; tx: number; ty: number } | null = null;
  private pinchOrigin: { distance: number; mid: PointerPoint } | null = null;

  constructor(options: PanZoomOptions) {
    this.options = options;
  }

  /** Centers `content` in the canvas at the largest scale that shows all of it. */
  fit(content: ContentSize): void {
    const rect = this.canvasRect();
    if (!rect || content.width <= 0 || content.height <= 0) return;
    const scale = Math.min(
      rect.width / content.width,
      rect.height / content.height,
      this.options.fitMaxScale,
    );
    this.apply(scale, (rect.width - content.width * scale) / 2, (rect.height - content.height * scale) / 2);
    this.userZoomed = false;
  }

  /** The scale `fit()` would choose now; NaN when the canvas is unknown. */
  fitScale(content: ContentSize): number {
    const rect = this.canvasRect();
    if (!rect || content.width <= 0 || content.height <= 0) return NaN;
    return Math.min(rect.width / content.width, rect.height / content.height, this.options.fitMaxScale);
  }

  /**
   * Zooms by `factor` keeping the content under the client point still,
   * which is what makes a wheel over a detail land on that detail.
   */
  zoomAt(clientX: number, clientY: number, factor: number): void {
    const rect = this.canvasRect();
    if (!rect) return;
    const cx = clientX - rect.left;
    const cy = clientY - rect.top;
    const next = clamp(this.scale * factor, this.options.minScale, this.options.maxScale);
    const ratio = next / this.scale;
    this.apply(next, cx - (cx - this.tx) * ratio, cy - (cy - this.ty) * ratio);
    this.userZoomed = true;
  }

  zoomCenter(factor: number): void {
    const rect = this.canvasRect();
    if (!rect) return;
    this.zoomAt(rect.left + rect.width / 2, rect.top + rect.height / 2, factor);
  }

  /**
   * A double click or double tap: to 1:1 around the point when the view is
   * at or below fit, back to fit otherwise. On a screen that fits the whole
   * image at 1:1 already, fit and 1:1 coincide and the toggle goes to 2x so
   * the gesture always does something.
   */
  toggle(clientX: number, clientY: number, content: ContentSize): void {
    const fit = this.fitScale(content);
    if (Number.isNaN(fit)) return;
    const atFit = Math.abs(this.scale - fit) < 1e-3;
    if (!atFit && this.scale > fit) {
      this.fit(content);
      return;
    }
    const target = Math.abs(fit - 1) < 1e-3 ? 2 : Math.max(1, fit);
    this.zoomAt(clientX, clientY, target / this.scale);
  }

  panBy(dx: number, dy: number): void {
    this.apply(this.scale, this.tx + dx, this.ty + dy);
    this.userZoomed = true;
  }

  /** Wheel over the canvas; prevents the page from scrolling or zooming. */
  onWheel(event: WheelEvent): void {
    event.preventDefault();
    this.zoomAt(event.clientX, event.clientY, Math.exp(-event.deltaY * WHEEL_ZOOM_RATE));
  }

  /**
   * One pointer drags; a second pointer turns the drag into a pinch that
   * zooms around the midpoint and follows it. The canvas must set
   * `touch-action: none` so the engine hands both fingers over.
   */
  onPointerDown(event: PointerEvent): void {
    if (event.pointerType === 'mouse' && event.button !== 0) return;
    const canvas = this.options.canvas();
    if (!canvas) return;
    this.pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    canvas.setPointerCapture(event.pointerId);
    this.panning = true;
    if (this.pointers.size >= 2) {
      this.panOrigin = null;
      this.pinchOrigin = this.pinchFrom(this.pointers);
    } else {
      this.panOrigin = { x: event.clientX, y: event.clientY, tx: this.tx, ty: this.ty };
    }
  }

  onPointerMove(event: PointerEvent): void {
    const tracked = this.pointers.get(event.pointerId);
    if (!tracked) return;
    tracked.x = event.clientX;
    tracked.y = event.clientY;
    if (this.pinchOrigin && this.pointers.size >= 2) {
      const now = this.pinchFrom(this.pointers);
      if (!now) return;
      // Zoom around where the midpoint WAS, so the content under it stays
      // put, then carry it to where the midpoint is now.
      const was = this.pinchOrigin.mid;
      if (this.pinchOrigin.distance >= MIN_PINCH_DISTANCE_PX) {
        this.zoomAt(was.x, was.y, now.distance / this.pinchOrigin.distance);
      }
      this.apply(this.scale, this.tx + now.mid.x - was.x, this.ty + now.mid.y - was.y);
      this.userZoomed = true;
      this.pinchOrigin = now;
      return;
    }
    if (this.panOrigin) {
      this.apply(this.scale, this.panOrigin.tx + (event.clientX - this.panOrigin.x), this.panOrigin.ty + (event.clientY - this.panOrigin.y));
      this.userZoomed = true;
    }
  }

  onPointerUp(event: PointerEvent): void {
    if (!this.pointers.delete(event.pointerId)) return;
    const canvas = this.options.canvas();
    if (canvas?.hasPointerCapture(event.pointerId)) canvas.releasePointerCapture(event.pointerId);
    if (this.pointers.size >= 2) {
      this.pinchOrigin = this.pinchFrom(this.pointers);
      return;
    }
    this.pinchOrigin = null;
    const remaining = this.pointers.values().next().value;
    if (remaining) {
      // The finger left over keeps dragging from where it is now.
      this.panOrigin = { x: remaining.x, y: remaining.y, tx: this.tx, ty: this.ty };
      return;
    }
    this.panOrigin = null;
    this.panning = false;
  }

  /**
   * Zoom and pan keys. Returns whether the key was one of them, so the
   * caller can leave Escape and navigation to the dialog.
   */
  onKeydown(event: KeyboardEvent, content: ContentSize): boolean {
    switch (event.key) {
      case '+':
      case '=':
        this.zoomCenter(ZOOM_STEP);
        break;
      case '-':
      case '_':
        this.zoomCenter(1 / ZOOM_STEP);
        break;
      case '0':
        this.fit(content);
        break;
      case 'ArrowLeft':
        this.panBy(PAN_STEP_PX, 0);
        break;
      case 'ArrowRight':
        this.panBy(-PAN_STEP_PX, 0);
        break;
      case 'ArrowUp':
        this.panBy(0, PAN_STEP_PX);
        break;
      case 'ArrowDown':
        this.panBy(0, -PAN_STEP_PX);
        break;
      default:
        return false;
    }
    event.preventDefault();
    return true;
  }

  private apply(scale: number, tx: number, ty: number): void {
    this.scale = scale;
    this.tx = tx;
    this.ty = ty;
  }

  private canvasRect(): DOMRect | undefined {
    return this.options.canvas()?.getBoundingClientRect();
  }

  private pinchFrom(pointers: Map<number, PointerPoint>): { distance: number; mid: PointerPoint } | null {
    const [a, b] = [...pointers.values()];
    if (!a || !b) return null;
    return {
      distance: Math.hypot(b.x - a.x, b.y - a.y),
      mid: { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 },
    };
  }
}

function clamp(n: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, n));
}
