// Read CSS custom properties as concrete, parseable sRGB colors.
//
// App-agnostic and pure: nothing in here knows about mermaid, themes, or
// which tokens exist. It answers one question — "what color does
// `var(--x)` actually paint, expressed in something a non-CSS consumer
// can parse?" — for callers that have to hand colors to a library which
// cannot be handed CSS (mermaid's `khroma` color math is the first one).
//
// Two browser facts shape the implementation, both verified in real
// Chromium and pinned by `components/chat/markdown/mermaidTokens.browser.test.ts`:
//
// 1. **`getComputedStyle()` does NOT return `rgb()`.** Per CSS Color 4, a
//    color declared in a non-legacy space serializes in that space:
//    Chromium/WebKit hand back `oklch(0.178 0.014 285.82)` verbatim, and a
//    `color-mix()` token comes back as `oklab(…)`. Do not "simplify" the
//    canvas step below away on the assumption that the browser normalizes
//    to `rgb()`. It does not, and neither does `ctx.fillStyle`'s getter
//    (it preserves the color space too). What DOES normalize is painting
//    the color and reading the pixel back: the canvas backing store is
//    8-bit sRGB, so `getImageData` is a guaranteed-parseable exit.
//
// 2. **`fillStyle` silently KEEPS its previous value** when handed
//    something it cannot parse. Without a known seed, a rejected value
//    would read back as whatever the previous caller painted. Seeding with
//    `transparent` settles it: a rejected value leaves the pixel at alpha
//    0, which is also the right answer for a color that really is fully
//    transparent.
//
// Resolution is two hops, both required: a probe element turns
// `var(--token)` into a computed color (custom properties resolve through
// the cascade, including `--card: var(--surface-1)` style indirection,
// which `getPropertyValue('--card')` would return unresolved), and a 1×1
// canvas turns that computed color into sRGB.

/** Strict hex only — `#rgb`, `#rgba`, `#rrggbb`, `#rrggbbaa`. */
const HEX_COLOR = /^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/;

let canvasCtx: CanvasRenderingContext2D | null | undefined;

function getCanvasContext(): CanvasRenderingContext2D | null {
  if (canvasCtx !== undefined) return canvasCtx;
  try {
    const canvas = document.createElement('canvas');
    canvas.width = 1;
    canvas.height = 1;
    canvasCtx = canvas.getContext('2d', { willReadFrequently: true });
  } catch {
    canvasCtx = null;
  }
  return canvasCtx;
}

/** Drops the memoized canvas context. Tests only. */
export function resetCssColorProbe(): void {
  canvasCtx = undefined;
}

function canvasRoundTrip(value: string): string | undefined {
  const ctx = getCanvasContext();
  if (!ctx) return undefined;
  try {
    ctx.clearRect(0, 0, 1, 1);
    // See fact 2 in the header: the transparent seed is what makes a
    // REJECTED value distinguishable from the previous caller's color.
    ctx.fillStyle = 'transparent';
    ctx.fillStyle = value;
    ctx.fillRect(0, 0, 1, 1);
    const [r, g, b, a] = ctx.getImageData(0, 0, 1, 1).data;
    if (!a) return undefined;
    return a === 255
      ? `rgb(${r}, ${g}, ${b})`
      : `rgba(${r}, ${g}, ${b}, ${(a / 255).toFixed(3)})`;
  } catch {
    return undefined;
  }
}

/**
 * Normalizes any CSS color string to something a non-CSS color parser can
 * read: a validated hex literal, or an sRGB `rgb()` / `rgba()` string.
 *
 * Returns `undefined` when the value cannot be normalized — no canvas
 * (happy-dom), an unparseable input, or a fully transparent one. Callers
 * must OMIT the value rather than pass the raw string through: the
 * downstream consumer's default is defensible, and a string its parser
 * throws on is not.
 *
 * The fast path is deliberately narrow: ONLY a strictly validated hex
 * literal skips the canvas. Everything else — `rgb()` / `rgba()` included
 * — takes the round trip, so transparent-drop and validation semantics
 * are uniform and a malformed value can never reach the consumer by
 * looking superficially color-shaped.
 */
export function toConcreteColor(raw: string | undefined): string | undefined {
  const value = raw?.trim();
  if (!value) return undefined;
  if (HEX_COLOR.test(value)) {
    // 4- and 8-digit forms carry alpha in the trailing nibble / byte;
    // fully transparent is the same nothing a rejected value is.
    if (value.length === 5 && value[4] === '0') return undefined;
    if (value.length === 9 && parseInt(value.slice(7), 16) === 0) return undefined;
    return value;
  }
  return canvasRoundTrip(value);
}

/** 8-bit sRGB channels plus alpha in 0–1. */
export interface RgbChannels {
  readonly r: number;
  readonly g: number;
  readonly b: number;
  readonly a: number;
}

const RGB_FUNCTION = /^rgba?\(\s*([0-9.]+)[\s,]+([0-9.]+)[\s,]+([0-9.]+)(?:[\s,/]+([0-9.]+%?))?/;

/**
 * The channels behind a value {@link toConcreteColor} produced.
 *
 * BOTH its output forms are handled — the `rgb()` / `rgba()` string the canvas
 * round trip returns AND the `#hex` literal the fast path returns — because
 * every caller that re-parses a concrete color has to accept whatever that
 * function chose. Two hand-rolled copies of the `rgb()` regex existed before
 * this, and the second one silently dropped hex, which is why terminal
 * selection lost its tint under a hex-valued accent.
 *
 * `undefined` for anything else, including a value that never went through
 * `toConcreteColor` and is still in a wide-gamut space.
 */
export function rgbChannels(value: string | undefined): RgbChannels | undefined {
  const raw = value?.trim();
  if (!raw) return undefined;

  if (raw.startsWith('#')) {
    if (!HEX_COLOR.test(raw)) return undefined;
    const digits = raw.slice(1);
    const short = digits.length === 3 || digits.length === 4;
    const at = (i: number): number => {
      const part = short ? digits[i]! + digits[i]! : digits.slice(i * 2, i * 2 + 2);
      return parseInt(part, 16);
    };
    const hasAlpha = digits.length === 4 || digits.length === 8;
    return { r: at(0), g: at(1), b: at(2), a: hasAlpha ? at(3) / 255 : 1 };
  }

  const match = RGB_FUNCTION.exec(raw);
  if (!match) return undefined;
  const alpha = match[4];
  const a =
    alpha === undefined ? 1 : alpha.endsWith('%') ? Number(alpha.slice(0, -1)) / 100 : Number(alpha);
  const channels = { r: Number(match[1]), g: Number(match[2]), b: Number(match[3]), a };
  return Number.isFinite(channels.r) &&
    Number.isFinite(channels.g) &&
    Number.isFinite(channels.b) &&
    Number.isFinite(channels.a)
    ? channels
    : undefined;
}

// ---------------------------------------------------------------------------
// Probe
// ---------------------------------------------------------------------------

// Unresolved-token detection. An invalid `var()` makes the declaration
// "invalid at computed-value time", which is NOT the same as absent: the
// property falls back to its inherited or initial value and reads back as
// a perfectly plausible — and completely wrong — color. Every slot below
// is therefore written as `var(--token, SENTINEL)`, and the wrapper and
// probe both paint SENTINEL into every slot so the *other* invalid path
// (a token that exists but holds a non-color) lands on it too. A read-back
// equal to the sentinel means "this token did not resolve".
const SENTINEL = 'rgb(1, 2, 3)';
const SENTINEL_KEY = 'rgb(1,2,3)';

const FONT_SENTINEL = '__ao-unresolved-font__';

/**
 * Color-valued properties used as read slots, one token per slot in a
 * single style recalc.
 *
 * `color` is deliberately NOT a slot: it is painted with the sentinel on
 * the probe so every slot whose initial value is `currentcolor`
 * (`border-*-color`, `outline-color`, `text-decoration-color`,
 * `column-rule-color`) falls back to the sentinel rather than to a real
 * color. Inherited slots (`caret-color`, the `-webkit-text-*` pair,
 * `text-emphasis-color`) inherit the sentinel from the wrapper instead.
 * `background-color`'s initial is `transparent`, which `toConcreteColor`
 * already reports as unresolved, so it needs neither.
 */
const COLOR_SLOTS = [
  'border-top-color',
  'border-right-color',
  'border-bottom-color',
  'border-left-color',
  'outline-color',
  'text-decoration-color',
  'column-rule-color',
  'caret-color',
  '-webkit-text-fill-color',
  '-webkit-text-stroke-color',
  'text-emphasis-color',
  'background-color',
] as const;

const SENTINEL_DECLS = ['color', ...COLOR_SLOTS]
  .map((prop) => `${prop}:${SENTINEL}`)
  .join(';');

// Out-of-flow and zero-sized so it cannot affect layout, but it IS in the
// document — a detached element inherits nothing, so `var()` would not
// resolve on it.
const OFFSCREEN = 'position:absolute;left:-9999px;top:0;width:0;height:0;pointer-events:none;';

function isSentinel(raw: string): boolean {
  return raw.replace(/\s+/g, '') === SENTINEL_KEY;
}

export interface TokenStyles {
  /** Keyed by the custom-property name that was asked for. */
  colors: Record<string, string | undefined>;
  fontFamily: string | undefined;
}

/**
 * Resolves a batch of color tokens (and optionally one font-family token)
 * in ONE probe append and ONE style recalc.
 *
 * Every requested name is present in `colors`; an unresolved or
 * unparseable token maps to `undefined`. More tokens than there are read
 * slots is handled by chunking (one extra recalc per chunk) rather than
 * refusing.
 */
export function readTokenStyles(
  tokenNames: readonly string[],
  fontFamilyToken?: string,
): TokenStyles {
  const colors: Record<string, string | undefined> = {};
  for (const name of tokenNames) colors[name] = undefined;
  let fontFamily: string | undefined;

  if (typeof document === 'undefined' || !document.body) {
    return { colors, fontFamily };
  }

  const wrapper = document.createElement('div');
  wrapper.setAttribute('aria-hidden', 'true');
  wrapper.style.cssText = `${OFFSCREEN}${SENTINEL_DECLS}`;
  const probe = document.createElement('div');
  probe.style.cssText = SENTINEL_DECLS;
  wrapper.appendChild(probe);
  document.body.appendChild(wrapper);

  try {
    const computed = getComputedStyle(probe);
    let fontPending = fontFamilyToken !== undefined;
    if (fontFamilyToken !== undefined) {
      probe.style.setProperty('font-family', `var(${fontFamilyToken}, ${FONT_SENTINEL})`);
    }

    const readFont = (): void => {
      fontPending = false;
      const raw = computed.fontFamily?.trim();
      if (!raw) return;
      // Quoting of a single custom-ident family is engine-dependent.
      if (raw.replace(/["']/g, '') === FONT_SENTINEL) return;
      fontFamily = raw;
    };

    for (let start = 0; start < tokenNames.length; start += COLOR_SLOTS.length) {
      const batch = tokenNames.slice(start, start + COLOR_SLOTS.length);
      batch.forEach((token, i) => {
        probe.style.setProperty(COLOR_SLOTS[i], `var(${token}, ${SENTINEL})`);
      });
      // `getComputedStyle` is live: the first read below forces the style
      // recalc, and every later read in this pass is served from the same
      // snapshot. That is the whole point of the slot table.
      batch.forEach((token, i) => {
        const raw = computed.getPropertyValue(COLOR_SLOTS[i]);
        colors[token] = isSentinel(raw) ? undefined : toConcreteColor(raw);
      });
      if (fontPending) readFont();
      // Clear so a short final chunk cannot read the previous chunk's slot.
      batch.forEach((_, i) => probe.style.removeProperty(COLOR_SLOTS[i]));
    }
    if (fontPending) readFont();
  } catch {
    // Leave whatever resolved before the throw; callers treat `undefined`
    // as "omit".
  } finally {
    wrapper.remove();
  }

  return { colors, fontFamily };
}

/** `readTokenStyles` for callers that only want colors. */
export function readTokenColors(
  tokenNames: readonly string[],
): Record<string, string | undefined> {
  return readTokenStyles(tokenNames).colors;
}

/** `readTokenStyles` for callers that only want a font stack. */
export function readTokenFontFamily(tokenName: string): string | undefined {
  return readTokenStyles([], tokenName).fontFamily;
}
